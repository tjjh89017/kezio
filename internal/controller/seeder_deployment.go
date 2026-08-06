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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// seederBTPort is the fixed BitTorrent listen port every per-Image
// seeder container uses. Fixed (not ephemeral) for the same reason
// config/seeder/ezio-seeder-deployment.yaml's EZIO_BT_PORT is fixed: each
// pod gets its own network namespace, so there is no cross-pod
// collision, and a stable value is one less thing to plumb through when
// this is later wired to an announced address.
const seederBTPort int32 = 16881

// defaultSeederGracePeriod is how long a per-Image, per-site seeder
// Deployment is kept after its reference count reaches zero, in case a
// sequential deploy queue drives the count straight back up. See
// SeederDeploymentConfig.gracePeriod.
const defaultSeederGracePeriod = 5 * time.Minute

// seederDeploymentImageLabel names the Image (by name, within the
// Deployment's own namespace) a per-Image seeder Deployment was created
// for, so reconcileSeederDeployments can list every Deployment it owns
// for a given Image with a single label selector.
const seederDeploymentImageLabel = "kezio.kojuro.date/seeder-image"

// seederDeploymentSiteAnnotation records the site (Machine
// spec.networkSite value) a per-Image seeder Deployment serves. This is
// an annotation, not a label: networkSite is free-form operator text
// with no guarantee of fitting Kubernetes' label-value syntax (unlike an
// Image name, which is already a valid object name and therefore a
// valid label value - see ingestJobLabel's use of it directly),
// including the empty string itself being a valid, distinct site.
const seederDeploymentSiteAnnotation = "kezio.kojuro.date/seeder-site"

// seederDeploymentEmptySinceAnnotation records (RFC3339) when a per-Image
// seeder Deployment's site last dropped out of the demand set. Its
// presence and age are the whole grace-period mechanism: the reference
// count decides *whether* a Deployment is wanted, and this annotation
// only smooths *when* a no-longer-wanted one actually gets deleted (see
// reconcileSeederDeployments). Stored on the Deployment itself, not
// Image status, so it needs no reconciliation of its own to stay
// consistent - it lives and dies with the object it describes.
const seederDeploymentEmptySinceAnnotation = "kezio.kojuro.date/seeder-empty-since"

// maxSeederDeploymentNameLength is the Kubernetes Deployment name limit:
// a ReplicaSet name adds a hash suffix and a Pod name adds a further
// one on top of that, so (as with ingestJobName's Job name) the
// Deployment name itself is kept well inside the 63-character DNS-1035
// label limit those generated names must also satisfy.
const maxSeederDeploymentNameLength = 63

// seederDeploymentNamePrefix identifies a Deployment as a per-Image
// seeder this controller manages, at a glance in `kubectl get
// deployments`.
const seederDeploymentNamePrefix = "kezio-seeder-"

// SeederDeploymentConfig configures how the Image reconciler creates and
// removes per-Image, per-site seeder Deployments. Its zero value (Image
// == "") disables this entirely - the same inert-by-default shape
// IngestConfig and SeederConfig use, so no existing deployment or the
// e2e suite is affected until an operator opts in.
type SeederDeploymentConfig struct {
	// Image is the ezio-seeder container image reference.
	Image string
	// StoreVolume is mounted read-only into every seeder Deployment this
	// reconciler creates, at storeMountPath. It is expected to be the
	// same store the ingest pipeline writes to (see IngestConfig's
	// StoreVolume) - a seeder with its own, separately provisioned store
	// would have nothing to serve.
	StoreVolume corev1.VolumeSource
	// GracePeriod overrides defaultSeederGracePeriod when positive.
	GracePeriod time.Duration
	// Now returns the current time. Defaults to time.Now; tests override
	// it to drive the grace-period countdown without sleeping.
	Now func() time.Time
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

// machineHoldsSeederReference reports whether machine's current state
// means it is (or, in error backoff, still intends to be) deploying an
// Image right now - the condition that keeps a per-Image seeder
// Deployment demanded for machine's site. This is enumerated against
// the actual state machine (see machine_controller.go's onChange),
// rather than left at "Provisioning" alone:
//
//   - Provisioning obviously holds: the deploy plan machine's leecher is
//     following right now names torrents a seeder Deployment must be
//     serving.
//   - Error holds only when the failure that stopped it was a
//     provisioning failure (reconcileError resumes reasonProvisionFailed
//     straight back into reconcileProvisioning): the controller retries
//     that same phase with backoff, so the machine still intends to
//     resume the same deploy and still needs the seeder. Error entered
//     from a Register or Inspect failure (or with no recorded reason)
//     resumes into Enrolling/Inspecting instead - no Image is being
//     deployed yet, so it holds no reference.
//   - Every other state - Enrolling, Inspecting, Available, Provisioned
//   - is not currently deploying anything: Enrolling/Inspecting
//     precede image selection even mattering, Available is idle between
//     deploys, and Provisioned already finished (its Image reference
//     stays protected from deletion via status.provisioning - see
//     collectImageRefs - but that is a different concern from active
//     seeder demand).
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

// seederDemandBySite counts, per Machine spec.networkSite, how many
// Machines currently hold a seeder reference (machineHoldsSeederReference)
// to image. It reuses collectImageRefs - the same reference enumeration
// isImageInUse uses - rather than re-deriving which refs on a Machine
// name an Image. A Machine counts at most once per site even if it
// references image more than once (for example as both spec.imageRef
// and a spec.dataImages entry).
func seederDemandBySite(machines *keziov1alpha1.MachineList, image *keziov1alpha1.Image) map[string]int32 {
	demand := map[string]int32{}
	for i := range machines.Items {
		machine := &machines.Items[i]
		if !machineHoldsSeederReference(machine) {
			continue
		}
		for _, ref := range collectImageRefs(machine) {
			if ref.Name != image.Name || keziov1alpha1.ResolveNamespace(ref, machine.Namespace) != image.Namespace {
				continue
			}
			demand[machine.Spec.NetworkSite]++
			break
		}
	}
	return demand
}

// reconcileSeederDeployments ensures exactly the per-site seeder
// Deployments image's current demand (seederDemandBySite) calls for
// exist, deletes ones that have been out of demand for at least
// r.SeederDeployment.gracePeriod(), and writes the computed per-site
// counts to image's status. Deployment(Image, site) existing is derived
// state - this function is the whole mechanism; the grace period below
// is only a smoothing parameter on the delete side of it, so a
// sequential deploy queue that drives a site's count 1 -> 0 -> 1 does
// not tear down and cold-start a Deployment for every single Machine.
func (r *ImageReconciler) reconcileSeederDeployments(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	if !r.SeederDeployment.enabled() {
		return ctrl.Result{}, nil
	}

	machines := &keziov1alpha1.MachineList{}
	if err := r.List(ctx, machines); err != nil {
		return ctrl.Result{}, fmt.Errorf("list machines for seeder demand: %w", err)
	}
	demand := seederDemandBySite(machines, image)

	existing := &appsv1.DeploymentList{}
	if err := r.List(ctx, existing,
		client.InNamespace(image.Namespace),
		client.MatchingLabels{seederDeploymentImageLabel: image.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list seeder deployments: %w", err)
	}
	existingBySite := make(map[string]*appsv1.Deployment, len(existing.Items))
	for i := range existing.Items {
		dep := &existing.Items[i]
		existingBySite[dep.Annotations[seederDeploymentSiteAnnotation]] = dep
	}

	sites := make(map[string]int32, len(demand))
	now := r.SeederDeployment.now()
	var requeueAfter time.Duration

	for site, count := range demand {
		sites[site] = count
		dep, ok := existingBySite[site]
		if !ok {
			newDep, err := r.buildSeederDeployment(image, site)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, newDep); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("create seeder deployment: %w", err)
			}
			continue
		}
		if _, draining := dep.Annotations[seederDeploymentEmptySinceAnnotation]; draining {
			// Demand came back before the grace period elapsed: this is
			// exactly the thrash the grace period exists to absorb, so
			// clear the countdown instead of letting it run out under a
			// Deployment that is wanted again.
			if err := r.clearSeederEmptySince(ctx, dep); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	for site, dep := range existingBySite {
		if _, wanted := demand[site]; wanted {
			continue
		}
		sites[site] = 0

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

	if err := r.updateSeederStatus(ctx, image, sites); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
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

// parseSeederEmptySince reads dep's empty-since annotation, reporting ok
// = false when it is absent or unparsable (treated as "not draining yet"
// - reconcileSeederDeployments then stamps a fresh one).
func parseSeederEmptySince(dep *appsv1.Deployment) (time.Time, bool) {
	raw, ok := dep.Annotations[seederDeploymentEmptySinceAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// stampSeederEmptySince records that dep's site has just dropped out of
// the demand set, starting its grace-period countdown.
func (r *ImageReconciler) stampSeederEmptySince(ctx context.Context, dep *appsv1.Deployment, at time.Time) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[seederDeploymentEmptySinceAnnotation] = at.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("stamp seeder deployment empty-since: %w", err)
	}
	return nil
}

// clearSeederEmptySince removes dep's grace-period countdown, called
// when its site is back in the demand set before the countdown expired.
func (r *ImageReconciler) clearSeederEmptySince(ctx context.Context, dep *appsv1.Deployment) error {
	patch := client.MergeFrom(dep.DeepCopy())
	delete(dep.Annotations, seederDeploymentEmptySinceAnnotation)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("clear seeder deployment empty-since: %w", err)
	}
	return nil
}

// updateSeederStatus writes sites into image.Status.Seeders (sorted by
// site name for a stable diff), skipping the API call entirely when the
// computed value already matches what is stored.
func (r *ImageReconciler) updateSeederStatus(ctx context.Context, image *keziov1alpha1.Image, sites map[string]int32) error {
	status := make([]keziov1alpha1.ImageSeederSiteStatus, 0, len(sites))
	for site, count := range sites {
		status = append(status, keziov1alpha1.ImageSeederSiteStatus{Site: site, MachineCount: count})
	}
	sort.Slice(status, func(i, j int) bool { return status[i].Site < status[j].Site })

	if seederStatusEqual(image.Status.Seeders, status) {
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
// collected the moment the Image itself is deleted (the whole
// Deployment(Image, site) lifecycle's final step is ordinary garbage
// collection, not code in this reconciler).
//
// The pod template deliberately mirrors
// config/seeder/ezio-seeder-deployment.yaml's shape (env, ports,
// security context, the read-only store mount): this slice is lifecycle
// only, replacing *how many* Deployments exist and *when*, not the
// network attachment or storage model those pods use - both stay future
// work.
func (r *ImageReconciler) buildSeederDeployment(image *keziov1alpha1.Image, site string) (*appsv1.Deployment, error) {
	replicas := int32(1)
	labels := map[string]string{
		"app.kubernetes.io/name":      "kezio",
		"app.kubernetes.io/component": "ezio-seeder",
		seederDeploymentImageLabel:    image.Name,
	}

	trueVal := true
	falseVal := false
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        seederDeploymentName(image.Name, site),
			Namespace:   image.Namespace,
			Labels:      labels,
			Annotations: map[string]string{seederDeploymentSiteAnnotation: site},
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
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: storeVolumeName, MountPath: storeMountPath, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: storeVolumeName, VolumeSource: r.SeederDeployment.StoreVolume},
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(image, dep, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on seeder deployment: %w", err)
	}
	return dep, nil
}

// seederDeploymentName returns the deterministic Deployment name for
// imageName's seeder at site. Deterministic so reconcileSeederDeployments
// stays idempotent, and always hash-suffixed (unlike ingestJobName,
// which only adds one when truncation is needed) because two different
// sites of the same Image must never collide on one name.
func seederDeploymentName(imageName, site string) string {
	sum := sha256.Sum256([]byte(imageName + "\x00" + site))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]

	name := seederDeploymentNamePrefix + imageName + suffix
	if len(name) <= maxSeederDeploymentNameLength {
		return name
	}

	maxBaseLen := maxSeederDeploymentNameLength - len(seederDeploymentNamePrefix) - len(suffix)
	return seederDeploymentNamePrefix + imageName[:maxBaseLen] + suffix
}
