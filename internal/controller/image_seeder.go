/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// imageSeederEmptySinceAnnotation records (RFC3339, UTC) on a seeder
// Deployment when its Site's seed-demand was last observed absent.
// reconcileImageSeederSite starts a grace-period countdown from this
// timestamp instead of deleting the Deployment the moment demand drops,
// so a leeching swarm mid-download is never stranded by demand that
// clears and reappears (for example, across a short deploy queue). It is
// cleared the moment demand reappears (cancelSeederShutdown). Stored on
// the Deployment itself, not any status, so the countdown lives and dies
// with the object it describes.
const imageSeederEmptySinceAnnotation = "kezio.kojuro.date/seeder-empty-since"

// imageSeederUnreadySinceAnnotation records (RFC3339, UTC) on a seeder
// Deployment the moment it is observed with active demand
// (wantsSeeder) but zero AvailableReplicas: a crash loop, a bad image,
// or an unsatisfiable nodeSelector all look identical from here. Without
// this, such a Deployment surfaces nowhere - an agent building a deploy
// plan against this Image just finds no served content and has no way
// to tell why. Cleared the moment the Deployment reports an available
// replica again, following the same discipline as
// imageSeederEmptySinceAnnotation. Unlike that annotation, this drives
// no grace-period countdown of its own: it is surfaced (via
// ImageConditionSeederDegraded) as soon as it is set, since the whole
// point is to stop an operator from waiting on a silently stuck
// Deployment.
const imageSeederUnreadySinceAnnotation = "kezio.kojuro.date/seeder-unready-since"

// seederSiteProblemKind classifies why a Site's seeder Deployment is not
// serving its demand, for reconcileImageSeeder to aggregate into
// ImageConditionSeederDegraded.
type seederSiteProblemKind int

const (
	// seederProblemNone is the zero value: nothing wrong at this Site.
	seederProblemNone seederSiteProblemKind = iota
	// seederProblemForeignOwner: the Deployment name this Site's seeder
	// would use is occupied by an object this Image does not control.
	seederProblemForeignOwner
	// seederProblemUnready: the Site's seeder Deployment has active
	// demand but has never reported an available replica.
	seederProblemUnready
)

// seederSiteProblem is one Site's contribution to
// reconcileImageSeeder's aggregated ImageConditionSeederDegraded. The
// zero value (kind == seederProblemNone) means nothing to report.
type seederSiteProblem struct {
	kind seederSiteProblemKind
	site string
}

// reconcileImageSeeder drives every Site's seeder Deployment lifecycle for
// an already-Ready image: create-on-demand, grace-period shutdown on
// demand loss, and terminating-Deployment handling - all per (Image,
// Site), the unit section 3.1 replaces the old per-content one with. It
// only ever runs once image is Ready (see onChange): a Deployment is
// never created for an Image whose referenced content is not all Ready
// yet.
//
// Demand is grouped by Site (imageSeedDemandBySite); a Site whose
// resolution failed is already excluded there and never blocks another
// Site's own processing here, nor does an error reconciling one Site
// block another's.
func (r *ImageReconciler) reconcileImageSeeder(ctx context.Context, image *keziov1alpha3.Image) (ctrl.Result, error) {
	demand, noSeederSites, err := r.imageSeedDemandBySite(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}

	existing, err := listImageSeederDeployments(ctx, r.Client, image)
	if err != nil {
		return ctrl.Result{}, err
	}

	sites := make(map[string]bool, len(demand)+len(existing))
	for site := range demand {
		sites[site] = true
	}
	for site := range existing {
		sites[site] = true
	}

	var contents []seededContent
	if len(demand) > 0 {
		contents, err = r.imageSeededContents(ctx, image)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	var result ctrl.Result
	var errs []error
	var problems []seederSiteProblem
	for site := range sites {
		siteResult, problem, err := r.reconcileImageSeederSite(ctx, image, site, demand[site], existing[site], contents)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if problem.kind != seederProblemNone {
			problems = append(problems, problem)
		}
		result = minRequeue(result, siteResult)
	}
	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}

	if err := r.updateImageSeederCondition(ctx, image, problems, noSeederSites); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// reconcileImageSeederSite runs one Site's own seeder Deployment step,
// mirroring the per-content reconcileSeeder's state machine one level up:
// create on demand, patch placement drift, cancel or run a grace-period
// shutdown, and leave a terminating Deployment alone.
func (r *ImageReconciler) reconcileImageSeederSite(ctx context.Context, image *keziov1alpha3.Image, site string, demand *seederSiteDemand, dep *appsv1.Deployment, contents []seededContent) (ctrl.Result, seederSiteProblem, error) {
	wantsSeeder := demand != nil && demand.count > 0

	switch {
	case dep != nil && !dep.DeletionTimestamp.IsZero():
		// Terminating: never force-delete, and never attempt to recreate
		// under the same name while GC is still cleaning up (Create would
		// just fail AlreadyExists). The Deployment watch (Owns, unfiltered)
		// retriggers a reconcile once it is actually gone.
		return ctrl.Result{}, seederSiteProblem{}, nil

	case dep == nil && wantsSeeder:
		if !r.Seeder.ready() {
			// No seeder image configured: demand is simply not acted on at
			// this Site. PartitionContentReconciler's own status derivation
			// (no available Deployment at this Site) is what surfaces this
			// to a user, not anything written here.
			return ctrl.Result{}, seederSiteProblem{}, nil
		}
		foreign, err := r.createImageSeederDeployment(ctx, image, site, contents, demand.resolution)
		if err != nil {
			return ctrl.Result{}, seederSiteProblem{}, err
		}
		if foreign {
			return ctrl.Result{}, seederSiteProblem{kind: seederProblemForeignOwner, site: site}, nil
		}
		return ctrl.Result{}, seederSiteProblem{}, nil

	case dep != nil && wantsSeeder:
		dep, err := r.ensureImageSeederPlacement(ctx, image, site, dep, contents, demand.resolution)
		if err != nil {
			return ctrl.Result{}, seederSiteProblem{}, err
		}
		if err := r.cancelSeederShutdown(ctx, dep); err != nil {
			return ctrl.Result{}, seederSiteProblem{}, err
		}
		unready, err := r.checkSeederUnready(ctx, dep)
		if err != nil {
			return ctrl.Result{}, seederSiteProblem{}, err
		}
		if unready {
			return ctrl.Result{}, seederSiteProblem{kind: seederProblemUnready, site: site}, nil
		}
		return ctrl.Result{}, seederSiteProblem{}, nil

	case dep != nil && !wantsSeeder:
		result, err := r.shutdownImageSeederDeployment(ctx, dep)
		return result, seederSiteProblem{}, err

	default: // dep == nil && !wantsSeeder
		return ctrl.Result{}, seederSiteProblem{}, nil
	}
}

// createImageSeederDeployment creates image's seeder Deployment at site.
// r.Seeder must be ready() - the caller gates on that before calling this.
//
// foreign reports whether the Deployment name is already occupied by an
// object image does not control (metav1.IsControlledBy): a stale
// survivor of a deleted-and-recreated same-named Image, or a hand-applied
// object. That object is read only to check ownership, never patched,
// updated, or deleted - a foreign Deployment must never be adopted or
// overwritten, and the caller must not count this Site as served.
func (r *ImageReconciler) createImageSeederDeployment(ctx context.Context, image *keziov1alpha3.Image, site string, contents []seededContent, res sitederive.Resolution) (foreign bool, err error) {
	dep := r.buildImageSeederDeployment(image, site, contents, res)
	if err := controllerutil.SetControllerReference(image, dep, r.Scheme); err != nil {
		return false, fmt.Errorf("image %q: setting seeder deployment owner reference: %w", image.Name, err)
	}
	if err := r.Create(ctx, dep); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("image %q: creating seeder deployment %q: %w", image.Name, dep.Name, err)
		}
		existing := &appsv1.Deployment{}
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(dep), existing); getErr != nil {
			return false, fmt.Errorf("image %q: getting seeder deployment %q after AlreadyExists: %w", image.Name, dep.Name, getErr)
		}
		return !metav1.IsControlledBy(existing, image), nil
	}
	return false, nil
}

// ensureImageSeederPlacement patches dep's placement (the Multus
// annotation, nodeSelector, and labels) and its containers (image, env,
// ports - everything buildImageSeederDeployment puts on the pod spec
// besides volumes) in place when either differs from the freshly-built
// desired shape, and leaves dep untouched otherwise - so a reconcile with
// no drift never issues a write. Converging the containers here is what
// lets an already-running seeder Deployment pick up a manager restarted
// with a new PARTITIONCONTENT_SEEDER_IMAGE (or changed
// MaxUploads/MaxConnections/GracePeriod-derived env), the same way
// reconcileBootdDeployment converges the bootd Deployment's own Spec.
// Volumes/mounts travel with the containers since
// buildImageSeederDeployment builds both from the same contents, but the
// content set itself can never legitimately change after creation: image
// only reaches this once Ready, at which point its referenced content set
// is fixed (PartitionContentSpec/ImageSpec are both immutable). Selector
// is never touched either - it is immutable on an existing Deployment, and
// this Deployment's own name already makes it exact per (Image, Site).
func (r *ImageReconciler) ensureImageSeederPlacement(ctx context.Context, image *keziov1alpha3.Image, site string, dep *appsv1.Deployment, contents []seededContent, res sitederive.Resolution) (*appsv1.Deployment, error) {
	desired := r.buildImageSeederDeployment(image, site, contents, res)

	wantAnnotations := desired.Spec.Template.Annotations
	wantNodeSelector := desired.Spec.Template.Spec.NodeSelector
	wantLabels := desired.Labels
	wantContainers := desired.Spec.Template.Spec.Containers

	if equality.Semantic.DeepEqual(dep.Spec.Template.Annotations, wantAnnotations) &&
		equality.Semantic.DeepEqual(dep.Spec.Template.Spec.NodeSelector, wantNodeSelector) &&
		equality.Semantic.DeepEqual(dep.Labels, wantLabels) &&
		equality.Semantic.DeepEqual(dep.Spec.Template.Spec.Containers, wantContainers) {
		return dep, nil
	}

	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Template.Annotations = wantAnnotations
	dep.Spec.Template.Spec.NodeSelector = wantNodeSelector
	dep.Labels = wantLabels
	dep.Spec.Template.Spec.Containers = wantContainers
	if err := r.Patch(ctx, dep, patch); err != nil {
		return nil, fmt.Errorf("image %q: updating seeder deployment placement %q: %w", image.Name, dep.Name, err)
	}
	return dep, nil
}

// parseSeederEmptySince reads dep's grace-period countdown start time,
// reporting ok = false when it is absent or unparsable (treated as
// "countdown not started yet" by callers).
func parseSeederEmptySince(dep *appsv1.Deployment) (time.Time, bool) {
	raw, ok := dep.Annotations[imageSeederEmptySinceAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// stampSeederEmptySince records at (UTC, RFC3339) on dep, starting the
// grace-period countdown.
func (r *ImageReconciler) stampSeederEmptySince(ctx context.Context, dep *appsv1.Deployment, at time.Time) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[imageSeederEmptySinceAnnotation] = at.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: stamping empty-since: %w", dep.Name, err)
	}
	return nil
}

// cancelSeederShutdown clears dep's grace-period countdown, called when
// demand reappears before the countdown expired.
func (r *ImageReconciler) cancelSeederShutdown(ctx context.Context, dep *appsv1.Deployment) error {
	if _, marked := dep.Annotations[imageSeederEmptySinceAnnotation]; !marked {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	delete(dep.Annotations, imageSeederEmptySinceAnnotation)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: clearing empty-since: %w", dep.Name, err)
	}
	return nil
}

// checkSeederUnready stamps or clears dep's unready-since annotation
// against its current AvailableReplicas, reporting whether dep should be
// surfaced as unready this reconcile. Callers only reach this for a
// Deployment with active demand (wantsSeeder) - readiness is meaningless
// to report otherwise.
func (r *ImageReconciler) checkSeederUnready(ctx context.Context, dep *appsv1.Deployment) (bool, error) {
	if dep.Status.AvailableReplicas > 0 {
		if _, marked := dep.Annotations[imageSeederUnreadySinceAnnotation]; marked {
			if err := r.cancelSeederUnready(ctx, dep); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if _, marked := dep.Annotations[imageSeederUnreadySinceAnnotation]; !marked {
		if err := r.stampSeederUnreadySince(ctx, dep, r.Seeder.now()); err != nil {
			return false, err
		}
	}
	return true, nil
}

// stampSeederUnreadySince records at (UTC, RFC3339) on dep, marking when
// its pod was first observed with active demand but zero available
// replicas.
func (r *ImageReconciler) stampSeederUnreadySince(ctx context.Context, dep *appsv1.Deployment, at time.Time) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[imageSeederUnreadySinceAnnotation] = at.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: stamping unready-since: %w", dep.Name, err)
	}
	return nil
}

// cancelSeederUnready clears dep's unready-since annotation, called once
// dep reports an available replica again.
func (r *ImageReconciler) cancelSeederUnready(ctx context.Context, dep *appsv1.Deployment) error {
	if _, marked := dep.Annotations[imageSeederUnreadySinceAnnotation]; !marked {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	delete(dep.Annotations, imageSeederUnreadySinceAnnotation)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: clearing unready-since: %w", dep.Name, err)
	}
	return nil
}

// shutdownImageSeederDeployment runs one grace-period step for dep, whose
// Site no longer has an active seed-demand: it starts the countdown on
// first observation, waits it out on later ones, and only deletes dep
// once the countdown has actually elapsed. See
// imageSeederEmptySinceAnnotation's doc comment for why this is a
// countdown rather than an immediate delete.
func (r *ImageReconciler) shutdownImageSeederDeployment(ctx context.Context, dep *appsv1.Deployment) (ctrl.Result, error) {
	now := r.Seeder.now()

	since, marked := parseSeederEmptySince(dep)
	if !marked {
		if err := r.stampSeederEmptySince(ctx, dep, now); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.Seeder.gracePeriod()}, nil
	}

	if remaining := r.Seeder.gracePeriod() - now.Sub(since); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting seeder deployment %q: %w", dep.Name, err)
	}
	return ctrl.Result{}, nil
}

// minRequeue returns whichever of a/b has the smaller positive
// RequeueAfter, or whichever is non-zero when only one is - the merge
// rule reconcileImageSeeder needs to combine every Site's own
// ctrl.Result into one for the single Reconcile call they all share.
func minRequeue(a, b ctrl.Result) ctrl.Result {
	switch {
	case a.RequeueAfter == 0:
		return b
	case b.RequeueAfter == 0:
		return a
	case a.RequeueAfter < b.RequeueAfter:
		return a
	default:
		return b
	}
}
