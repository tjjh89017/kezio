/*
Copyright 2026 Date Huang.

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// SeederEZIOClient is the subset of internal/seeder.Client a seeder
// content-syncing path needs.
type SeederEZIOClient interface {
	AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool, maxUploads, maxConnections int32) error
	GetTorrentStatus(ctx context.Context, hashes []string) (map[string]seeder.Torrent, error)
	PauseTorrent(ctx context.Context, hash string) error
	ResumeTorrent(ctx context.Context, hash string) error
	Close() error
}

// seederBTPort is the fixed BitTorrent listen port every per-Image
// seeder container uses. Each pod has its own network namespace, so
// fixing it causes no cross-pod collision.
const seederBTPort int32 = 16881

// multusDefaultNetworkAnnotation is the Multus CNI pod annotation that
// replaces a pod's default network attachment (as opposed to
// k8s.v1.cni.cncf.io/networks, which only adds one). This keeps a seeder
// pod single-homed on its per-Site network with pod.Status.PodIP
// directly reachable, no NAT.
const multusDefaultNetworkAnnotation = "v1.multus-cni.io/default-network"

// errForeignSeederDeployment is logged whenever a per-Image seeder
// Deployment's expected name is occupied by an object not
// controller-owned by that Image (metav1.IsControlledBy fails).
var errForeignSeederDeployment = errors.New("seeder deployment name is controlled by a different Image")

// defaultSeederGracePeriod is how long a per-Image, per-site seeder
// Deployment is kept after its reference count reaches zero, in case a
// sequential deploy queue drives the count straight back up.
const defaultSeederGracePeriod = 5 * time.Minute

// SeederDeploymentImageLabel names the Image (by name, within the
// Deployment's own namespace) a per-Image seeder Deployment was created
// for.
const SeederDeploymentImageLabel = "kezio.kojuro.date/seeder-image"

// SeederDeploymentSiteAnnotation records the Site's namespace-qualified
// identity (sitederive.Resolve's SiteName) a per-Image seeder Deployment
// serves. An annotation rather than a label since it is never used as a
// selector target.
const SeederDeploymentSiteAnnotation = "kezio.kojuro.date/seeder-site"

// seederDeploymentEmptySinceAnnotation records (RFC3339) when a per-Image
// seeder Deployment's site last dropped out of the demand set, driving
// the grace-period deletion in reconcileSeederDeployments. Stored on the
// Deployment itself so it lives and dies with the object it describes.
const seederDeploymentEmptySinceAnnotation = "kezio.kojuro.date/seeder-empty-since"

// seederDeploymentUnreadySinceAnnotation records (RFC3339) when a
// per-Image seeder Deployment with active demand was last observed with
// zero AvailableReplicas. Without this, a Deployment whose pod never
// becomes Ready (crashloop, bad image, unsatisfiable NodeSelector)
// surfaces nowhere, since agents just poll ActionWait forever.
const seederDeploymentUnreadySinceAnnotation = "kezio.kojuro.date/seeder-unready-since"

// SeederDeploymentConfig configures how the Image reconciler creates and
// removes per-Image, per-site seeder Deployments. Its zero value (Image
// == "") disables this entirely.
type SeederDeploymentConfig struct {
	// Image is the ezio-seeder container image reference.
	Image string
	// GracePeriod overrides defaultSeederGracePeriod when positive.
	GracePeriod time.Duration
	// Now returns the current time. Defaults to time.Now; tests override
	// it to drive the grace-period countdown without sleeping.
	Now func() time.Time

	// TrackerURL is passed to the publish Job as TRACKER_URL, the
	// "announce" field baked into each content partition's .torrent (see
	// image_publish.go's buildPublishJob). Empty is valid: the publish
	// step then copies content with no .torrent.
	TrackerURL string
	// EzioTuning carries the cluster-wide default AddTorrent tuning
	// (MaxUploads/MaxConnections) applied to every content torrent
	// syncSeederDeploymentContent adds. Nil falls back to
	// seeder.DefaultMaxUploads/DefaultMaxConnections.
	EzioTuning *keziov1alpha1.MachineEzioTuning
	// Dial opens a client to one per-Image seeder pod's gRPC target
	// (host:port). Defaults to wrapping seeder.Dial when nil; tests
	// override it to hand back a fake.
	Dial func(target string) (SeederEZIOClient, error)
}

// enabled reports whether per-Image seeder Deployments are configured.
func (c SeederDeploymentConfig) enabled() bool {
	return c.Image != ""
}

// gracePeriod returns c.GracePeriod, falling back to
// defaultSeederGracePeriod when unset.
func (c SeederDeploymentConfig) gracePeriod() time.Duration {
	if c.GracePeriod > 0 {
		return c.GracePeriod
	}
	return defaultSeederGracePeriod
}

// now returns c.Now(), falling back to time.Now.
func (c SeederDeploymentConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// machineHoldsSeederReference reports whether machine is (or, in error
// backoff, still intends to be) deploying an Image right now, the
// condition that keeps a per-Image seeder Deployment demanded for
// machine's site. Error only holds when reconcileError would resume it
// into reconcileProvisioning (reason reasonProvisionFailed); a Register
// or Inspect failure resumes into Enrolling/Inspecting instead, where no
// Image is being deployed yet.
func machineHoldsSeederReference(machine *keziov1alpha1.Machine) bool {
	switch machine.Status.State {
	case keziov1alpha1.MachineStateProvisioning:
		return true
	case keziov1alpha1.MachineStateError:
		cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.ConditionReady)
		return cond != nil && cond.Reason == reasonProvisionFailed
	default:
		return false
	}
}

// seederDemandBySite counts, per derived site, how many Machines
// currently hold a seeder reference (machineHoldsSeederReference) to
// image. A Machine counts at most once per site even if it references
// image more than once (for example as both spec.imageRef and a
// spec.dataImages entry).
//
// A Machine whose subnetRef or Subnet.siteRef is dangling is logged and
// skipped rather than aborting the whole count, so one misconfigured
// Machine cannot block every other site's seeder demand. Any other
// error from Resolve is transient and returned so the caller requeues
// instead of undercounting demand.
func seederDemandBySite(ctx context.Context, c client.Client, machines *keziov1alpha1.MachineList, image *keziov1alpha1.Image) (map[string]seederSiteDemand, error) {
	log := logf.FromContext(ctx)
	demand := map[string]seederSiteDemand{}
	for i := range machines.Items {
		machine := &machines.Items[i]
		if !machineHoldsSeederReference(machine) {
			continue
		}
		references := false
		for _, ref := range collectImageRefs(machine) {
			if ref.Name == image.Name && keziov1alpha1.ResolveNamespace(ref, machine.Namespace) == image.Namespace {
				references = true
				break
			}
		}
		if !references {
			continue
		}

		res, err := sitederive.Resolve(ctx, c, machine)
		if err != nil {
			if errors.Is(err, sitederive.ErrSubnetNotFound) || errors.Is(err, sitederive.ErrSiteNotFound) {
				log.Error(err, "skipping machine with unresolved site for seeder demand",
					"machine", client.ObjectKeyFromObject(machine))
				continue
			}
			return nil, fmt.Errorf("resolve site for machine %s/%s: %w", machine.Namespace, machine.Name, err)
		}

		entry := demand[res.SiteName]
		entry.Site = res.Site
		entry.Count++
		demand[res.SiteName] = entry
	}
	return demand, nil
}

// seederSiteDemand is one site's contribution to seederDemandBySite's
// result: how many Machines currently demand a seeder there, and the
// Site object itself (already fetched by sitederive.Resolve).
type seederSiteDemand struct {
	Site  *keziov1alpha1.Site
	Count int32
}

// reconcileSeederDeployments ensures exactly the per-site seeder
// Deployments image's current demand (seederDemandBySite) calls for
// exist, deletes ones out of demand for at least
// r.SeederDeployment.gracePeriod(), and writes the computed per-site
// counts to image's status. The grace period smooths a sequential
// deploy queue driving a site's count 1 -> 0 -> 1 so it does not
// tear down and cold-start a Deployment for every single Machine.
func (r *ImageReconciler) reconcileSeederDeployments(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	if !r.SeederDeployment.enabled() {
		return ctrl.Result{}, nil
	}

	machines := &keziov1alpha1.MachineList{}
	if err := r.List(ctx, machines); err != nil {
		return ctrl.Result{}, fmt.Errorf("list machines for seeder demand: %w", err)
	}
	demand, err := seederDemandBySite(ctx, r.Client, machines, image)
	if err != nil {
		return ctrl.Result{}, err
	}

	existing := &appsv1.DeploymentList{}
	if err := r.List(ctx, existing,
		client.InNamespace(image.Namespace),
		client.MatchingLabels{SeederDeploymentImageLabel: image.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list seeder deployments: %w", err)
	}
	existingBySite, foreignBySite := partitionSeederDeploymentsBySite(existing, image)

	sites := make(map[string]int32, len(demand))
	now := r.SeederDeployment.now()
	var requeueAfter time.Duration
	var degraded seederDegradedSites
	log := logf.FromContext(ctx)

	for site, sd := range demand {
		sites[site] = sd.Count
		if foreign, isForeign := foreignBySite[site]; isForeign {
			log.Error(errForeignSeederDeployment, "existing seeder deployment name is controlled by a different Image; leaving it untouched and not counting the site as served",
				"site", site, "deployment", foreign.Name)
			degraded.foreignOwner = append(degraded.foreignOwner, site)
			delete(sites, site)
			continue
		}

		// Resolved once per site: both the create and reuse branches below
		// need the Subnet, and both react to a site that has none the same
		// way.
		seederSubnet, err := r.resolveSeederSubnet(ctx, sd.Site)
		switch {
		case errors.Is(err, errSeederSubnetNotFound):
			// Surfaced rather than returned, so one Site's dangling
			// reference does not fail every other site's reconcile.
			degraded.missingSeederSubnet = append(degraded.missingSeederSubnet,
				fmt.Sprintf("%s (seederSubnetRef %s)", site, seederSubnetKey(sd.Site)))
		case err != nil:
			return ctrl.Result{}, err
		case seederSubnet == nil:
			degraded.unsetSeederSubnet = append(degraded.unsetSeederSubnet, site)
		}

		dep, ok := existingBySite[site]
		if !ok {
			if seederSubnet == nil {
				// No Deployment is created for a site with no seeder
				// Subnet; the cause recorded above keeps that visible.
				continue
			}
			newDep, err := r.buildSeederDeployment(image, site, seederSubnet)
			if err != nil {
				return ctrl.Result{}, err
			}
			foreign, err := r.createSeederDeployment(ctx, image, newDep)
			if err != nil {
				return ctrl.Result{}, err
			}
			if foreign {
				log.Error(errForeignSeederDeployment, "seeder deployment name was taken by a different Image between list and create; not adopting it and not counting the site as served",
					"site", site, "deployment", newDep.Name)
				degraded.foreignOwner = append(degraded.foreignOwner, site)
				delete(sites, site)
			}
			continue
		}
		unready, requeueIn, err := r.reconcileExistingSeederDeployment(ctx, log, image, site, dep, seederSubnet, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if unready {
			degraded.unready = append(degraded.unready, site)
		}
		requeueAfter = soonestRequeue(requeueAfter, requeueIn)
	}

	for site, dep := range existingBySite {
		if _, wanted := demand[site]; wanted {
			continue
		}
		sites[site] = 0

		// The unready countdown is scoped to "actively demanded but not
		// serving"; demand dropping ends it too.
		if _, marked := dep.Annotations[seederDeploymentUnreadySinceAnnotation]; marked {
			if err := r.clearSeederTimeAnnotation(ctx, dep, seederDeploymentUnreadySinceAnnotation); err != nil {
				return ctrl.Result{}, err
			}
		}

		emptySince, ok := parseSeederEmptySince(dep)
		if !ok {
			if err := r.stampSeederEmptySince(ctx, dep, now); err != nil {
				return ctrl.Result{}, err
			}
			requeueAfter = soonestRequeue(requeueAfter, r.SeederDeployment.gracePeriod())
			continue
		}

		if remaining := r.SeederDeployment.gracePeriod() - now.Sub(emptySince); remaining > 0 {
			requeueAfter = soonestRequeue(requeueAfter, remaining)
			continue
		}

		if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete seeder deployment: %w", err)
		}
		delete(sites, site)
	}

	if err := r.updateSeederStatus(ctx, image, sites, degraded); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileExistingSeederDeployment brings one already-existing,
// actively-demanded per-Image seeder Deployment up to date: clears a
// stale draining countdown, evaluates readiness
// (checkSeederDeploymentReady), and either leaves dep untouched (nil
// seederSubnet) or recomputes and pushes its desired spec on drift. The
// caller resolves seederSubnet and owns reporting why it is nil, since
// the create branch needs the very same resolution.
func (r *ImageReconciler) reconcileExistingSeederDeployment(ctx context.Context, log logr.Logger, image *keziov1alpha1.Image, site string, dep *appsv1.Deployment, seederSubnet *keziov1alpha1.Subnet, now time.Time) (unready bool, requeueAfter time.Duration, err error) {
	if _, draining := dep.Annotations[seederDeploymentEmptySinceAnnotation]; draining {
		// Demand came back before the grace period elapsed; clear the
		// countdown instead of letting it run out on a wanted Deployment.
		if err := r.clearSeederEmptySince(ctx, dep); err != nil {
			return false, 0, err
		}
	}

	unready, requeueAfter, err = r.checkSeederDeploymentReady(ctx, dep, now)
	if err != nil {
		return false, 0, err
	}
	if unready {
		log.Error(errors.New("seeder pod has not become Ready within the grace period"),
			"existing seeder deployment has no available replicas; surfacing as degraded",
			"site", site, "deployment", dep.Name)
	}

	// A site with active demand keeps its Deployment current: a
	// renamed/moved SeederNetworkRef, a changed NodeSelector, or a new
	// SeederDeploymentConfig.Image must still reach an already-created
	// Deployment.
	if seederSubnet == nil {
		// The Deployment and its pod already exist; pushing
		// buildSeederDeployment's output for a nil seederSubnet would
		// silently degrade it, since seederNodeSelector and
		// seederPodAnnotations both go nil, dropping the Multus
		// default-network annotation and moving the pod onto the
		// cluster's default network while it keeps reporting Available.
		// Leave it on its last-known-good network and surface the broken
		// reference on Image status instead.
		log.Error(errors.New("site's seeder Subnet reference no longer resolves"),
			"existing seeder deployment left untouched; not updating it onto a network-less spec",
			"site", site, "deployment", dep.Name)
		return unready, requeueAfter, nil
	}

	desired, err := r.buildSeederDeployment(image, site, seederSubnet)
	if err != nil {
		return unready, requeueAfter, err
	}
	if !equality.Semantic.DeepEqual(dep.Spec, desired.Spec) || !equality.Semantic.DeepEqual(dep.Labels, desired.Labels) {
		dep.Labels = desired.Labels
		dep.Spec = desired.Spec
		if err := r.Update(ctx, dep); err != nil {
			return unready, requeueAfter, fmt.Errorf("update seeder deployment: %w", err)
		}
	}

	return unready, requeueAfter, nil
}

// partitionSeederDeploymentsBySite splits existing's per-Image seeder
// Deployments by site into two maps: existingBySite for ones
// controller-owned by image, and foreignBySite for ones that are not
// (metav1.IsControlledBy compares owner UID, not name). Foreign objects
// are never adopted or mutated - a Deployment named for (image, site)
// but owned by something else is either hand-applied or the stale
// survivor of a deleted-and-recreated same-named Image, since owner-ref
// GC is asynchronous.
func partitionSeederDeploymentsBySite(existing *appsv1.DeploymentList, image *keziov1alpha1.Image) (existingBySite, foreignBySite map[string]*appsv1.Deployment) {
	existingBySite = make(map[string]*appsv1.Deployment, len(existing.Items))
	foreignBySite = make(map[string]*appsv1.Deployment)
	for i := range existing.Items {
		dep := &existing.Items[i]
		site := dep.Annotations[SeederDeploymentSiteAnnotation]
		if !metav1.IsControlledBy(dep, image) {
			foreignBySite[site] = dep
			continue
		}
		existingBySite[site] = dep
	}
	return existingBySite, foreignBySite
}

// createSeederDeployment creates newDep, tolerating AlreadyExists only
// after confirming what raced it in. Returns foreign = true when the
// object now at newDep's name is not controller-owned by image: the
// caller must not count that site as served. A plain AlreadyExists with
// an owned object is the benign double-reconcile race this tolerance
// exists for (foreign = false, err = nil).
func (r *ImageReconciler) createSeederDeployment(ctx context.Context, image *keziov1alpha1.Image, newDep *appsv1.Deployment) (foreign bool, err error) {
	if err := r.Create(ctx, newDep); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create seeder deployment: %w", err)
		}
		// Get and check ownership rather than assuming the benign case:
		// this could be a foreign/stale object instead of a double-reconcile
		// race.
		raced := &appsv1.Deployment{}
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(newDep), raced); getErr != nil {
			return false, fmt.Errorf("get seeder deployment after AlreadyExists: %w", getErr)
		}
		return !metav1.IsControlledBy(raced, image), nil
	}
	return false, nil
}

// errSeederSubnetNotFound classifies a resolveSeederSubnet failure
// caused by Site.spec.seederSubnetRef naming a Subnet that does not
// exist. A user-facing misconfiguration, never retried away.
var errSeederSubnetNotFound = errors.New("seeder subnet not found")

// resolveSeederSubnet fetches the Subnet site designates as its seeder
// attachment point (SiteSpec.SeederSubnetRef). A nil Subnet with a nil
// error means site designates no seeder Subnet at all; an error matching
// errSeederSubnetNotFound means the ref names one that does not exist.
// Callers must treat these two differently. Any other error is
// transient and worth a retry.
func (r *ImageReconciler) resolveSeederSubnet(ctx context.Context, site *keziov1alpha1.Site) (*keziov1alpha1.Subnet, error) {
	if site.Spec.SeederSubnetRef == nil {
		return nil, nil
	}
	ref := *site.Spec.SeederSubnetRef
	ns := keziov1alpha1.ResolveNamespace(ref, site.Namespace)
	subnet := &keziov1alpha1.Subnet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s (site %s/%s spec.seederSubnetRef)", errSeederSubnetNotFound, ns, ref.Name, site.Namespace, site.Name)
		}
		return nil, fmt.Errorf("get seeder subnet %s/%s for site %s/%s: %w", ns, ref.Name, site.Namespace, site.Name, err)
	}
	return subnet, nil
}

// seederSubnetKey formats site's seederSubnetRef as the namespace/name
// an operator must go looking for. Returns "" when the ref is unset.
func seederSubnetKey(site *keziov1alpha1.Site) string {
	ref := site.Spec.SeederSubnetRef
	if ref == nil {
		return ""
	}
	return keziov1alpha1.ResolveNamespace(*ref, site.Namespace) + "/" + ref.Name
}

// soonestRequeue returns the smaller of two RequeueAfter candidates,
// treating a non-positive value (the zero Duration ctrl.Result starts
// from) as "no candidate yet" rather than "requeue immediately".
func soonestRequeue(current, candidate time.Duration) time.Duration {
	if current <= 0 {
		return candidate
	}
	if candidate > 0 && candidate < current {
		return candidate
	}
	return current
}

// parseSeederTimeAnnotation reads dep's RFC3339 timestamp annotation
// named key, reporting ok = false when it is absent or unparsable
// (treated as "countdown not started yet" by callers).
func parseSeederTimeAnnotation(dep *appsv1.Deployment, key string) (time.Time, bool) {
	raw, ok := dep.Annotations[key]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// stampSeederTimeAnnotation records at (UTC, RFC3339) under dep's
// annotation named key, starting a grace-period countdown.
func (r *ImageReconciler) stampSeederTimeAnnotation(ctx context.Context, dep *appsv1.Deployment, key string, at time.Time) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[key] = at.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("stamp seeder deployment %s: %w", key, err)
	}
	return nil
}

// clearSeederTimeAnnotation removes dep's annotation named key, called
// once the condition that started its countdown no longer holds.
func (r *ImageReconciler) clearSeederTimeAnnotation(ctx context.Context, dep *appsv1.Deployment, key string) error {
	patch := client.MergeFrom(dep.DeepCopy())
	delete(dep.Annotations, key)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("clear seeder deployment %s: %w", key, err)
	}
	return nil
}

// parseSeederEmptySince reads dep's empty-since annotation. Thin
// wrapper over parseSeederTimeAnnotation for reconcileSeederDeployments'
// draining branch.
func parseSeederEmptySince(dep *appsv1.Deployment) (time.Time, bool) {
	return parseSeederTimeAnnotation(dep, seederDeploymentEmptySinceAnnotation)
}

// stampSeederEmptySince records that dep's site has just dropped out of
// the demand set, starting its grace-period countdown.
func (r *ImageReconciler) stampSeederEmptySince(ctx context.Context, dep *appsv1.Deployment, at time.Time) error {
	return r.stampSeederTimeAnnotation(ctx, dep, seederDeploymentEmptySinceAnnotation, at)
}

// clearSeederEmptySince removes dep's grace-period countdown, called
// when its site is back in the demand set before the countdown expired.
func (r *ImageReconciler) clearSeederEmptySince(ctx context.Context, dep *appsv1.Deployment) error {
	return r.clearSeederTimeAnnotation(ctx, dep, seederDeploymentEmptySinceAnnotation)
}

// checkSeederDeploymentReady evaluates dep's readiness against its
// seeder-unready-since countdown. It reports unready = true once dep has
// reported zero AvailableReplicas for at least the grace period.
// requeueAfter names when the caller should next reconsider (0 when
// there is nothing to wait for: already unready and reported, or
// currently ready).
func (r *ImageReconciler) checkSeederDeploymentReady(ctx context.Context, dep *appsv1.Deployment, now time.Time) (unready bool, requeueAfter time.Duration, err error) {
	if dep.Status.AvailableReplicas > 0 {
		if _, marked := dep.Annotations[seederDeploymentUnreadySinceAnnotation]; marked {
			if err := r.clearSeederTimeAnnotation(ctx, dep, seederDeploymentUnreadySinceAnnotation); err != nil {
				return false, 0, err
			}
		}
		return false, 0, nil
	}

	unreadySince, marked := parseSeederTimeAnnotation(dep, seederDeploymentUnreadySinceAnnotation)
	if !marked {
		if err := r.stampSeederTimeAnnotation(ctx, dep, seederDeploymentUnreadySinceAnnotation, now); err != nil {
			return false, 0, err
		}
		return false, r.SeederDeployment.gracePeriod(), nil
	}

	if remaining := r.SeederDeployment.gracePeriod() - now.Sub(unreadySince); remaining > 0 {
		return false, remaining, nil
	}
	return true, 0, nil
}

// seederDegradedCause is one reason updateSeederStatus can mark
// ImageConditionSeederDegraded: a Reason, the sites it applies to, and
// the message fragment describing it once those sites are known.
type seederDegradedCause struct {
	reason  string
	sites   []string
	message func(sites string) string
}

// seederDegradedSites collects, per cause, the sites one
// reconcileSeederDeployments pass found something wrong at. Each field
// becomes at most one seederDegradedCause.
type seederDegradedSites struct {
	// unsetSeederSubnet holds sites whose Site.spec.seederSubnetRef is
	// unset while demand is active.
	unsetSeederSubnet []string
	// missingSeederSubnet holds sites whose Site.spec.seederSubnetRef
	// names a Subnet that does not exist. Entries carry that Subnet's
	// namespace/name, so the condition tells the operator which name is
	// wrong rather than only which Site holds it.
	missingSeederSubnet []string
	// foreignOwner holds sites where a Deployment at the expected name is
	// not controller-owned by the Image (see reconcileSeederDeployments'
	// existingBySite/foreignBySite split).
	foreignOwner []string
	// unready holds sites whose Deployment has reported zero
	// AvailableReplicas for longer than the grace period.
	unready []string
}

// updateSeederStatus writes sites into image.Status.Seeders (sorted by
// site name for a stable diff) and sets ImageConditionSeederDegraded
// from degraded. Skips the API call when neither value changes anything
// stored.
func (r *ImageReconciler) updateSeederStatus(ctx context.Context, image *keziov1alpha1.Image, sites map[string]int32, degraded seederDegradedSites) error {
	status := make([]keziov1alpha1.ImageSeederSiteStatus, 0, len(sites))
	for site, count := range sites {
		status = append(status, keziov1alpha1.ImageSeederSiteStatus{Site: site, MachineCount: count})
	}
	sort.Slice(status, func(i, j int) bool { return status[i].Site < status[j].Site })
	seedersChanged := !seederStatusEqual(image.Status.Seeders, status)

	// Each cause gets its own Reason when it is the only one present, so
	// a human goes straight to the specific remediation. Multiple causes
	// share Reason "SeederDegraded" instead, since
	// ImageConditionSeederDegraded has room for only one condition.
	var causes []seederDegradedCause
	if len(degraded.unsetSeederSubnet) > 0 {
		causes = append(causes, seederDegradedCause{
			reason: "SeederSubnetRefUnset",
			sites:  degraded.unsetSeederSubnet,
			message: func(sites string) string {
				return fmt.Sprintf("Site.spec.seederSubnetRef is unset for site(s) %s; no seeder Deployment is created there and any existing one is left untouched rather than pushed onto a network-less spec, so Machines there cannot build a deploy plan for this Image until it is set", sites)
			},
		})
	}
	if len(degraded.missingSeederSubnet) > 0 {
		causes = append(causes, seederDegradedCause{
			reason: "SeederSubnetRefNotFound",
			sites:  degraded.missingSeederSubnet,
			message: func(sites string) string {
				return fmt.Sprintf("Site.spec.seederSubnetRef names a Subnet that does not exist for site(s) %s; no seeder Deployment is created there and any existing one is left untouched, so create the named Subnet or correct the reference", sites)
			},
		})
	}
	if len(degraded.foreignOwner) > 0 {
		causes = append(causes, seederDegradedCause{
			reason: "SeederDeploymentForeignOwner",
			sites:  degraded.foreignOwner,
			message: func(sites string) string {
				return fmt.Sprintf("seeder deployment name is already controlled by a different Image for site(s) %s; not adopted or mutated, and the site is not counted as served", sites)
			},
		})
	}
	if len(degraded.unready) > 0 {
		causes = append(causes, seederDegradedCause{
			reason: "SeederPodUnready",
			sites:  degraded.unready,
			message: func(sites string) string {
				return fmt.Sprintf("seeder pod has reported no available replicas for longer than the grace period for site(s) %s; check the seeder Deployment's pod status (crashloop, bad image, or an unsatisfiable NodeSelector)", sites)
			},
		})
	}

	var condChanged bool
	switch len(causes) {
	case 0:
		if apimeta.FindStatusCondition(image.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded) != nil {
			condChanged = apimeta.RemoveStatusCondition(&image.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
		}
	case 1:
		sort.Strings(causes[0].sites)
		condChanged = apimeta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha1.ImageConditionSeederDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             causes[0].reason,
			Message:            causes[0].message(strings.Join(causes[0].sites, ", ")),
			ObservedGeneration: image.Generation,
		})
	default:
		messages := make([]string, len(causes))
		for i, cause := range causes {
			sort.Strings(cause.sites)
			messages[i] = cause.message(strings.Join(cause.sites, ", "))
		}
		condChanged = apimeta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha1.ImageConditionSeederDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             "SeederDegraded",
			Message:            strings.Join(messages, "; "),
			ObservedGeneration: image.Generation,
		})
	}

	if !seedersChanged && !condChanged {
		return nil
	}
	image.Status.Seeders = status
	if err := r.Status().Update(ctx, image); err != nil {
		return fmt.Errorf("update image seeder status: %w", err)
	}
	return nil
}

// seederStatusEqual compares two ImageSeederSiteStatus slices by value,
// treating nil and empty as equal (both mean "no site currently
// demands a seeder").
func seederStatusEqual(a, b []keziov1alpha1.ImageSeederSiteStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildSeederDeployment constructs the (not yet created) per-Image,
// per-site seeder Deployment, owner-ref'd to image so it is garbage
// collected once the Image itself is deleted. The pod template mirrors
// config/seeder/ezio-seeder-deployment.yaml's shape. seederSubnet is
// site's own seeder Subnet (already resolved by resolveSeederSubnet);
// its SeederNetworkRef, if any, becomes the pod's Multus default-network
// annotation (seederPodAnnotations), and its own NodeSelector - not any
// other Subnet in site - becomes the pod's NodeSelector.
func (r *ImageReconciler) buildSeederDeployment(image *keziov1alpha1.Image, site string, seederSubnet *keziov1alpha1.Subnet) (*appsv1.Deployment, error) {
	replicas := int32(1)
	labels := map[string]string{
		AppNameLabel:               AppNameValue,
		AppComponentLabel:          SeederComponentValue,
		SeederDeploymentImageLabel: image.Name,
	}

	// One volume/mount per content partition, read-only.
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	for _, p := range image.Status.Partitions {
		if p.InfoHash == "" {
			continue
		}
		volName := fmt.Sprintf("content-%d", p.Number)
		volumes = append(volumes, corev1.Volume{Name: volName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: partitionPVCName(image.Name, p.Number),
				ReadOnly:  true,
			},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, MountPath: partitionMountPath(p.Number), ReadOnly: true})
	}

	trueVal := true
	falseVal := false
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        seederdeploy.Name(image.Name, site),
			Namespace:   image.Namespace,
			Labels:      labels,
			Annotations: map[string]string{SeederDeploymentSiteAnnotation: site},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: seederPodAnnotations(seederSubnet)},
				Spec: corev1.PodSpec{
					NodeSelector: seederNodeSelector(seederSubnet),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &trueVal,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "ezio-seeder",
						Image: r.SeederDeployment.Image,
						Env: []corev1.EnvVar{
							{Name: "EZIO_GRPC_LISTEN", Value: "0.0.0.0:50051"},
							{Name: "EZIO_BT_PORT", Value: strconv.Itoa(int(seederBTPort))},
						},
						Ports: []corev1.ContainerPort{
							{Name: "grpc", ContainerPort: 50051, Protocol: corev1.ProtocolTCP},
							{Name: "bt", ContainerPort: seederBTPort, Protocol: corev1.ProtocolTCP},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{dropAllCapabilities},
							},
						},
						VolumeMounts: mounts,
					}, {
						// Same image as ezio (both ship in it), different command.
						Name:    "seeder-register",
						Image:   r.SeederDeployment.Image,
						Command: []string{"/usr/local/bin/kezio-seeder-register"},
						Env: []corev1.EnvVar{
							{Name: "CONTENT_ROOT", Value: ingest.ContentRoot},
							{Name: "EZIO_TARGET", Value: "127.0.0.1:50051"},
							{Name: "EZIO_MAX_UPLOADS", Value: strconv.Itoa(int(seeder.ResolveMaxUploads(r.SeederDeployment.EzioTuning)))},
							{Name: "EZIO_MAX_CONNECTIONS", Value: strconv.Itoa(int(seeder.ResolveMaxConnections(r.SeederDeployment.EzioTuning)))},
						},
						Ports: []corev1.ContainerPort{
							{Name: "torrent", ContainerPort: seederdeploy.TorrentHTTPPort, Protocol: corev1.ProtocolTCP},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{dropAllCapabilities},
							},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(image, dep, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on seeder deployment: %w", err)
	}
	return dep, nil
}

// seederPodAnnotations returns the pod template annotations for a
// per-Image seeder Deployment: the Multus default-network override
// naming seederSubnet's own SeederNetworkRef, or nil when seederSubnet
// carries none.
//
// A bare NAD name is qualified with seederSubnet's own namespace, NOT
// image's: the NAD lives wherever its Subnet lives, while the seeder
// Deployment lives in the Image's namespace. Multus resolves an
// unqualified default-network value in its own system namespace
// (kube-system), not the pod's, so qualifying with the wrong namespace
// silently points at a NAD that does not exist there.
func seederPodAnnotations(seederSubnet *keziov1alpha1.Subnet) map[string]string {
	if seederSubnet == nil || seederSubnet.Spec.SeederNetworkRef == nil {
		return nil
	}
	ref := *seederSubnet.Spec.SeederNetworkRef
	ns := keziov1alpha1.ResolveNamespace(ref, seederSubnet.Namespace)
	return map[string]string{multusDefaultNetworkAnnotation: ns + "/" + ref.Name}
}

// seederNodeSelector returns the seeder pod template's NodeSelector:
// seederSubnet's own declared NodeSelector (nodeSelectorOrNil, defined
// in bootd_deployment.go), or nil when seederSubnet itself is nil.
func seederNodeSelector(seederSubnet *keziov1alpha1.Subnet) map[string]string {
	if seederSubnet == nil {
		return nil
	}
	return nodeSelectorOrNil(seederSubnet.Spec.NodeSelector)
}
