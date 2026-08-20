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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// defaultSeederSite is the sentinel PartitionContentSeederSite.Site value
// this reconciler reports while it has no Site/Subnet awareness: every
// seeder Deployment it creates is site-unaware, so there is exactly one
// "site" to report. A later item that makes seeding site-aware replaces
// this with real per-site entries.
const defaultSeederSite = "default"

// partitionContentSeederEmptySinceAnnotation records (RFC3339, UTC) on a
// seeder Deployment when its content's seed-demand marker was last
// observed absent. reconcileSeeder starts a grace-period countdown from
// this timestamp instead of deleting the Deployment the moment demand
// drops, so a leeching swarm mid-download is never stranded by a marker
// that clears and reappears (for example, across a short deploy queue).
// It is cleared the moment demand reappears (cancelSeederShutdown).
// Stored on the Deployment itself, not PartitionContent status, so the
// countdown lives and dies with the object it describes. Mirrors the
// legacy per-site seederDeploymentEmptySinceAnnotation mechanism
// (internal/controller/seeder_deployment.go on the legacy branch).
const partitionContentSeederEmptySinceAnnotation = "kezio.kojuro.date/seeder-empty-since"

// hasSeedDemand reports whether pc carries the seed-demand marker - see
// PartitionContentAnnotationSeedDemand's doc comment.
func hasSeedDemand(pc *keziov1alpha2.PartitionContent) bool {
	_, ok := pc.Annotations[keziov1alpha2.PartitionContentAnnotationSeedDemand]
	return ok
}

// reconcileSeeder drives the seeder Deployment lifecycle for an
// already-Ready content: create-on-demand, grace-period shutdown on
// demand loss, and the seeders[]/SeederDegraded status reflection. It
// only ever runs once pc is Ready (see onChange): a Deployment is never
// created for content that has no .torrent yet, and PartitionContent
// never regresses out of Ready once reached (PartitionContentSpec is
// immutable), so no earlier state can have left a Deployment behind.
func (r *PartitionContentReconciler) reconcileSeeder(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) (ctrl.Result, error) {
	demand := hasSeedDemand(pc)

	dep, err := r.seederDeploymentFor(ctx, pc, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case dep != nil && !dep.DeletionTimestamp.IsZero():
		// Terminating: never force-delete, and never attempt to recreate
		// under the same name while GC is still cleaning up (Create would
		// just fail AlreadyExists). Treated as "no seeder yet" for status;
		// a fresh reconcile once it is actually gone re-evaluates demand
		// from scratch. The Deployment watch (Owns, unfiltered) retriggers
		// that reconcile the moment it is actually removed.
		return r.recordSeederStatus(ctx, pc, nil, demand)

	case dep == nil && demand:
		if !r.Seeder.ready() {
			return r.recordSeederConfigMissing(ctx, pc)
		}
		if err := r.createSeederDeployment(ctx, pc, hash); err != nil {
			return ctrl.Result{}, err
		}
		// Freshly created: not Available yet. The Deployment watch
		// retriggers a reconcile once its pod becomes Ready.
		return r.recordSeederStatus(ctx, pc, nil, demand)

	case dep != nil && demand:
		if err := r.cancelSeederShutdown(ctx, dep); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordSeederStatus(ctx, pc, dep, demand)

	case dep != nil && !demand:
		return r.shutdownSeederDeployment(ctx, pc, dep)

	default: // dep == nil && !demand
		return r.recordSeederStatus(ctx, pc, nil, demand)
	}
}

// seederDeploymentFor gets the seeder Deployment for hash, if any.
func (r *PartitionContentReconciler) seederDeploymentFor(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: pc.Namespace, Name: seederdeploy.Name(hash)}
	if err := r.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("partitioncontent %q: getting seeder deployment %q: %w", pc.Name, key.Name, err)
	}
	return dep, nil
}

// createSeederDeployment creates the seeder Deployment for pc/hash.
// r.Seeder must be ready() - the caller gates on that before calling
// this, since an unready config means SeederDegraded, not a Deployment.
func (r *PartitionContentReconciler) createSeederDeployment(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) error {
	dep := r.buildSeederDeployment(pc, hash)
	if err := controllerutil.SetControllerReference(pc, dep, r.Scheme); err != nil {
		return fmt.Errorf("partitioncontent %q: setting seeder deployment owner reference: %w", pc.Name, err)
	}
	if err := r.Create(ctx, dep); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("partitioncontent %q: creating seeder deployment %q: %w", pc.Name, dep.Name, err)
	}
	return nil
}

// buildSeederDeployment constructs the (not yet created) per-content
// seeder Deployment: one replica running ezio alongside
// kezio-seeder-register (same image, different command - see
// docker/seeder), both mounting the content PVC read-only at
// ingest.ContentMountPath(hash) - the exact path cmd/seeder's default
// CONTENT_ROOT (ingest.ContentMountRoot) scans for content
// subdirectories, and the same mount-path convention the publish Job
// uses.
func (r *PartitionContentReconciler) buildSeederDeployment(pc *keziov1alpha2.PartitionContent, hash store.InfoHash) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{
		partitionContentAppNameLabel:      partitionContentAppNameValue,
		partitionContentAppComponentLabel: partitionContentSeederComponentValue,
	}
	trueVal := true
	falseVal := false

	const volumeName = "content"
	volumes := []corev1.Volume{{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: store.PVCName(hash),
				ReadOnly:  true,
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: volumeName, MountPath: ingest.ContentMountPath(hash), ReadOnly: true}}
	containerSecurityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &falseVal,
		ReadOnlyRootFilesystem:   &trueVal,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      seederdeploy.Name(hash),
			Namespace: pc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &trueVal,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "ezio",
							Image: r.Seeder.Image,
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: 50051, Protocol: corev1.ProtocolTCP},
							},
							SecurityContext: containerSecurityContext,
							VolumeMounts:    mounts,
						},
						{
							// Same image as ezio (both ship in it - see
							// docker/seeder/Dockerfile), different command:
							// registers this pod's mounted content with the
							// ezio container above over its pod-local gRPC
							// listener and serves the .torrent over HTTP.
							Name:    "seeder-register",
							Image:   r.Seeder.Image,
							Command: []string{"/usr/local/bin/kezio-seeder-register"},
							Ports: []corev1.ContainerPort{
								{Name: "torrent", ContainerPort: seederdeploy.TorrentHTTPPort, Protocol: corev1.ProtocolTCP},
							},
							// Proves only that the .torrent HTTP server is
							// bound and answering, not that any content has
							// finished registering - AvailableReplicas (and
							// thus SeederAvailable/seeders[]) must reflect
							// "actually serving", not "torrent list
							// complete", or a pod would stay unready for as
							// long as registration is in progress.
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: seederdeploy.TorrentHealthzPath,
										Port: intstr.FromString("torrent"),
									},
								},
							},
							SecurityContext: containerSecurityContext,
							VolumeMounts:    mounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}

// parseSeederEmptySince reads dep's grace-period countdown start time,
// reporting ok = false when it is absent or unparsable (treated as
// "countdown not started yet" by callers).
func parseSeederEmptySince(dep *appsv1.Deployment) (time.Time, bool) {
	raw, ok := dep.Annotations[partitionContentSeederEmptySinceAnnotation]
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
func (r *PartitionContentReconciler) stampSeederEmptySince(ctx context.Context, dep *appsv1.Deployment, at time.Time) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[partitionContentSeederEmptySinceAnnotation] = at.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: stamping empty-since: %w", dep.Name, err)
	}
	return nil
}

// cancelSeederShutdown clears dep's grace-period countdown, called when
// demand reappears before the countdown expired.
func (r *PartitionContentReconciler) cancelSeederShutdown(ctx context.Context, dep *appsv1.Deployment) error {
	if _, marked := dep.Annotations[partitionContentSeederEmptySinceAnnotation]; !marked {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	delete(dep.Annotations, partitionContentSeederEmptySinceAnnotation)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("seeder deployment %q: clearing empty-since: %w", dep.Name, err)
	}
	return nil
}

// shutdownSeederDeployment runs one grace-period step for dep, whose
// content no longer has an active seed-demand marker: it starts the
// countdown on first observation, waits it out on later ones, and only
// deletes dep once the countdown has actually elapsed. See
// partitionContentSeederEmptySinceAnnotation's doc comment for why this
// is a countdown rather than an immediate delete.
func (r *PartitionContentReconciler) shutdownSeederDeployment(ctx context.Context, pc *keziov1alpha2.PartitionContent, dep *appsv1.Deployment) (ctrl.Result, error) {
	now := r.Seeder.now()

	since, marked := parseSeederEmptySince(dep)
	if !marked {
		if err := r.stampSeederEmptySince(ctx, dep, now); err != nil {
			return ctrl.Result{}, err
		}
		result, err := r.recordSeederStatus(ctx, pc, dep, false)
		result.RequeueAfter = r.Seeder.gracePeriod()
		return result, err
	}

	if remaining := r.Seeder.gracePeriod() - now.Sub(since); remaining > 0 {
		result, err := r.recordSeederStatus(ctx, pc, dep, false)
		result.RequeueAfter = remaining
		return result, err
	}

	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: deleting seeder deployment %q: %w", pc.Name, dep.Name, err)
	}
	return r.recordSeederStatus(ctx, pc, nil, false)
}

// recordSeederConfigMissing records SeederDegraded=True: demand asked
// for a seeder but r.Seeder carries no image, so no Deployment was
// created. This never touches PartitionContentConditionReady or
// pc.Status.State - see PartitionContentSeederConfig's doc comment for
// why a missing seeder image only blocks seeding, not content readiness.
func (r *PartitionContentReconciler) recordSeederConfigMissing(ctx context.Context, pc *keziov1alpha2.PartitionContent) (ctrl.Result, error) {
	pc.Status.Seeders = nil
	setPartitionContentSeederDegradedCondition(pc, metav1.ConditionTrue, "SeederImageMissing",
		"seed-demand is set but no seeder image is configured on the manager (PARTITIONCONTENT_SEEDER_IMAGE); content stays Ready but unseedable until it is set")
	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording seeder config missing: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordSeederStatus reflects dep's observed availability into
// pc.Status.Seeders and sets PartitionContentConditionSeederDegraded
// from demand and dep together, then writes both through
// applyPartitionContentStatus.
//
// Seeders and SeederDegraded are deliberately independent: a Deployment
// still running out its grace-period shutdown (demand == false) still
// counts as seeding for Seeders, while SeederDegraded only ever reacts to
// demand asking for something the Deployment is not currently providing
// - it is cleared entirely (not set False) when there is no demand, since
// "degraded" does not apply to something nobody asked for.
func (r *PartitionContentReconciler) recordSeederStatus(ctx context.Context, pc *keziov1alpha2.PartitionContent, dep *appsv1.Deployment, demand bool) (ctrl.Result, error) {
	available := dep != nil && dep.DeletionTimestamp.IsZero() && dep.Status.AvailableReplicas > 0
	if available {
		pc.Status.Seeders = []keziov1alpha2.PartitionContentSeederSite{{Site: defaultSeederSite, MachineCount: 0}}
	} else {
		pc.Status.Seeders = nil
	}

	switch {
	case !demand:
		meta.RemoveStatusCondition(&pc.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
	case available:
		setPartitionContentSeederDegradedCondition(pc, metav1.ConditionFalse, "SeederAvailable",
			"the seeder deployment has an available replica")
	default:
		setPartitionContentSeederDegradedCondition(pc, metav1.ConditionTrue, "SeederUnavailable",
			"seeding is demanded but the seeder deployment has no available replica yet")
	}

	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording seeder status: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}
