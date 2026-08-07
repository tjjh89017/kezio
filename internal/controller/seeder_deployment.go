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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// SeederEZIOClient is the subset of internal/seeder.Client a seeder
// content-syncing path needs. Production wiring wraps *seeder.Client
// (via seeder.Dial); tests substitute a fake to exercise reconcile logic
// against an in-memory ezio double without a network connection.
type SeederEZIOClient interface {
	AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool, maxUploads, maxConnections int32) error
	GetTorrentStatus(ctx context.Context, hashes []string) (map[string]seeder.Torrent, error)
	PauseTorrent(ctx context.Context, hash string) error
	ResumeTorrent(ctx context.Context, hash string) error
	Close() error
}

// seederBTPort is the fixed BitTorrent listen port every per-Image
// seeder container uses. Fixed (not ephemeral) for the same reason
// config/seeder/ezio-seeder-deployment.yaml's EZIO_BT_PORT is fixed: each
// pod gets its own network namespace, so there is no cross-pod
// collision, and a stable value is one less thing to plumb through when
// this is later wired to an announced address.
const seederBTPort int32 = 16881

// multusDefaultNetworkAnnotation is the Multus CNI pod annotation that
// replaces a pod's default network attachment (as opposed to
// k8s.v1.cni.cncf.io/networks, which only adds one) - see
// SeederDeploymentConfig.Network's doc comment.
const multusDefaultNetworkAnnotation = "v1.multus-cni.io/default-network"

// defaultSeederGracePeriod is how long a per-Image, per-site seeder
// Deployment is kept after its reference count reaches zero, in case a
// sequential deploy queue drives the count straight back up. See
// SeederDeploymentConfig.gracePeriod.
const defaultSeederGracePeriod = 5 * time.Minute

// SeederDeploymentImageLabel names the Image (by name, within the
// Deployment's own namespace) a per-Image seeder Deployment was created
// for, so reconcileSeederDeployments can list every Deployment it owns
// for a given Image with a single label selector.
const SeederDeploymentImageLabel = "kezio.kojuro.date/seeder-image"

// SeederDeploymentSiteAnnotation records the site (Machine
// spec.networkSite value) a per-Image seeder Deployment serves. This is
// an annotation, not a label: networkSite is free-form operator text
// with no guarantee of fitting Kubernetes' label-value syntax (unlike an
// Image name, which is already a valid object name and therefore a
// valid label value - see ingestJobLabel's use of it directly),
// including the empty string itself being a valid, distinct site.
const SeederDeploymentSiteAnnotation = "kezio.kojuro.date/seeder-site"

// seederDeploymentEmptySinceAnnotation records (RFC3339) when a per-Image
// seeder Deployment's site last dropped out of the demand set. Its
// presence and age are the whole grace-period mechanism: the reference
// count decides *whether* a Deployment is wanted, and this annotation
// only smooths *when* a no-longer-wanted one actually gets deleted (see
// reconcileSeederDeployments). Stored on the Deployment itself, not
// Image status, so it needs no reconciliation of its own to stay
// consistent - it lives and dies with the object it describes.
const seederDeploymentEmptySinceAnnotation = "kezio.kojuro.date/seeder-empty-since"

// SeederDeploymentConfig configures how the Image reconciler creates and
// removes per-Image, per-site seeder Deployments. Its zero value (Image
// == "") disables this entirely - the same inert-by-default shape
// IngestConfig and SeederConfig use, so no existing deployment or the
// e2e suite is affected until an operator opts in.
type SeederDeploymentConfig struct {
	// Image is the ezio-seeder container image reference.
	Image string
	// GracePeriod overrides defaultSeederGracePeriod when positive.
	GracePeriod time.Duration
	// Now returns the current time. Defaults to time.Now; tests override
	// it to drive the grace-period countdown without sleeping.
	Now func() time.Time

	// TrackerURL is inserted as the "announce" field of every .torrent
	// syncSeederDeploymentContent builds from an Image's own
	// ImagePartitionStatus.TorrentInfo (no store volume is read here -
	// a partition's content lives in its own PVC, mounted directly onto
	// this Deployment's pod - see buildSeederDeployment). Left empty,
	// Deployments are still created and torn down by their reference
	// count (see contentEnabled) - only the content-adding half is
	// skipped.
	TrackerURL string
	// EzioTuning carries the cluster-wide default AddTorrent tuning
	// (MaxUploads/MaxConnections) applied to every content torrent
	// syncSeederDeploymentContent adds. Nil falls back to
	// seeder.DefaultMaxUploads/DefaultMaxConnections, the same as
	// SeederConfig.EzioTuning.
	EzioTuning *keziov1alpha1.MachineEzioTuning
	// Dial opens a client to one per-Image seeder pod's gRPC target
	// (host:port). Defaults to wrapping seeder.Dial when nil; tests
	// override it to hand back a fake, the same shape SeederConfig.Dial
	// uses.
	Dial func(target string) (SeederEZIOClient, error)
	// Network, when non-empty, names a Multus NetworkAttachmentDefinition
	// installed on the pod template as the multusDefaultNetworkAnnotation
	// value, which *replaces* (not adds to) the seeder pod's default
	// network attachment: the pod becomes single-homed on that
	// provisioning network, so pod.Status.PodIP is, by construction, the
	// address a Machine's leecher can reach directly - no ClusterIP
	// Service or NAT on the content data path (see
	// config/seeder/README.md's no-NAT rule). This is safe because
	// nothing dials into the pod over the cluster network any more:
	// content registration happens pod-locally (see
	// buildSeederDeployment's seeder-register container), and the only
	// inbound traffic this pod ever serves - the BitTorrent swarm and the
	// per-partition .torrent HTTP endpoint agentserver's DeployPlan
	// builder points a leecher at - both belong on the provisioning
	// network, not the cluster one. Empty (the default for every existing
	// deployment and the envtest suite) omits the annotation, leaving the
	// pod on the ordinary cluster network.
	Network string
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
		client.MatchingLabels{SeederDeploymentImageLabel: image.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list seeder deployments: %w", err)
	}
	existingBySite := make(map[string]*appsv1.Deployment, len(existing.Items))
	for i := range existing.Items {
		dep := &existing.Items[i]
		existingBySite[dep.Annotations[SeederDeploymentSiteAnnotation]] = dep
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

		// What each Deployment seeds is the seeder pod's own business: a
		// partition's .torrent lives in that partition's PVC, which only
		// that pod mounts (see buildSeederDeployment's seeder-register
		// container). This reconciler owns which Deployments exist.
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

// Content registration deliberately has no counterpart here. A
// partition's .torrent lives in that partition's PVC, and only the
// seeder pod mounts it, so the pod registers its own content with the
// ezio container beside it (cmd/seeder, wired as buildSeederDeployment's
// seeder-register container). This reconciler owns which Deployments
// exist, not what each one seeds.

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
		"app.kubernetes.io/name":   "kezio",
		AppComponentLabel:          SeederComponentValue,
		SeederDeploymentImageLabel: image.Name,
	}

	// One volume/mount per content partition, read-only: this
	// Deployment's whole storage need is exactly the partitions its own
	// Image owns, so it never mounts anything wider than that (compare
	// the single shared store volume every seeder used to mount).
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
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: seederPodAnnotations(image.Namespace, r.SeederDeployment.Network)},
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
								Drop: []corev1.Capability{dropAllCapabilities},
							},
						},
						VolumeMounts: mounts,
					}, {
						// Registers this pod's own content with the ezio
						// container beside it. It has to run here: a
						// partition's .torrent lives in that partition's
						// PVC, which only this pod mounts, so no reconciler
						// outside the pod can read it. Same image as ezio
						// (both ship in it), different command.
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
// per-Image seeder Deployment: the Multus default-network override when
// network is set (see SeederDeploymentConfig.Network's doc comment), or
// nil when it is empty - the same "no annotation" shape every existing
// deployment and the envtest suite already exercise.
//
// A bare NAD name is qualified with the Deployment's own namespace
// before it is stamped: Multus resolves an unqualified default-network
// value in its system namespace (kube-system), NOT in the pod's
// namespace the way the ordinary networks annotation is resolved, so a
// bare name silently points at a NAD that does not exist there. Probe
// run 31162430687 caught exactly this.
func seederPodAnnotations(namespace, network string) map[string]string {
	if network == "" {
		return nil
	}
	if !strings.Contains(network, "/") {
		network = namespace + "/" + network
	}
	return map[string]string{multusDefaultNetworkAnnotation: network}
}
