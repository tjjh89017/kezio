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
	"maps"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/sitederive"
	"github.com/tjjh89017/kezio/internal/store"
)

// multusDefaultNetworkAnnotation is the Multus CNI pod annotation that
// REPLACES a pod's default network attachment, unlike
// multusNetworksAnnotation, which only adds a second one alongside it.
//
// A seeder must be single-homed on its Site's seeding Subnet, not
// dual-homed: leechers reach it at Status.PodIP, and a pod that keeps the
// cluster CNI as its default reports that cluster address there - the one
// address a machine on the provisioning segment cannot route to.
// Single-homing also keeps BitTorrent's own peer discovery honest, since
// the address a peer announces is then the address it actually listens
// on, with no NAT in between. site_tracker_deployment.go's tracker pod
// shares this same annotation for the same reason.
const multusDefaultNetworkAnnotation = "v1.multus-cni.io/default-network"

// seederPodAnnotations returns the pod template annotations placing
// res.SeederNetworkRef as the pod's default (and only) network, or nil
// when res carries no SeederNetworkRef. A bare NAD name defaults against
// the resolved Subnet's own namespace rather than the Image's.
//
// Unlike bootdPodAnnotations, this replaces the default network rather
// than adding to it - see multusDefaultNetworkAnnotation. Both seeder
// containers talk to each other over the pod-local loopback, so nothing
// in the pod needs the cluster network it gives up.
func seederPodAnnotations(res sitederive.Resolution) map[string]string {
	if res.SeederNetworkRef == nil {
		return nil
	}
	ns := resolveNamespace(*res.SeederNetworkRef, res.Subnet.Namespace)
	return map[string]string{multusDefaultNetworkAnnotation: ns + "/" + res.SeederNetworkRef.Name}
}

// seederRegisterEnv returns the seeder-register container's environment:
// the content root it scans (cmd/seeder/main.go's CONTENT_ROOT, matching
// ingest.ContentMountRoot - the same mount path buildImageSeederDeployment
// gives every content volume), the address it reaches the ezio container
// at over the pod's shared loopback (EZIO_TARGET, matching the ezio
// container's own EZIO_GRPC_LISTEN port above), the resolved AddTorrent
// tuning cfg carries, and res's Site's tracker URL (TRACKER_URL): the
// announce cmd/seeder bakes into every .torrent it builds from a
// content's torrent.info, registers with ezio, and serves over HTTP.
func seederRegisterEnv(cfg ImageSeederConfig, res sitederive.Resolution) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "CONTENT_ROOT", Value: ingest.ContentMountRoot},
		{Name: "EZIO_TARGET", Value: fmt.Sprintf("127.0.0.1:%d", seederdeploy.EzioGRPCPort)},
		{Name: "EZIO_MAX_UPLOADS", Value: strconv.Itoa(int(cfg.maxUploads()))},
		{Name: "EZIO_MAX_CONNECTIONS", Value: strconv.Itoa(int(cfg.maxConnections()))},
	}
	if res.TrackerURL != "" {
		env = append(env, corev1.EnvVar{Name: "TRACKER_URL", Value: res.TrackerURL})
	}
	return env
}

// imageSeededContents returns the info hash of every Ready
// PartitionContent image's layout slots reference, deduplicated and
// sorted for a deterministic Deployment spec. reconcileImageSeeder only
// calls this once image itself is Ready, at which point every non-blank
// slot's content is expected to already be Ready (ImageReconciler's own
// aggregation gate); a content that fails to resolve here is skipped
// rather than failing the whole reconcile; the content's own status
// change will requeue this Image once it does resolve.
func (r *ImageReconciler) imageSeededContents(ctx context.Context, image *keziov1alpha2.Image) ([]store.InfoHash, error) {
	seen := make(map[string]bool)
	hashes := make([]store.InfoHash, 0, len(image.Spec.Layout.Slots))
	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef == nil || seen[slot.ContentRef.Name] {
			continue
		}
		seen[slot.ContentRef.Name] = true

		ns := slot.ContentRef.Namespace
		if ns == "" {
			ns = image.Namespace
		}
		var pc keziov1alpha2.PartitionContent
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: slot.ContentRef.Name}, &pc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("image %q: getting partitioncontent %q: %w", image.Name, slot.ContentRef.Name, err)
		}
		if !meta.IsStatusConditionTrue(pc.Status.Conditions, keziov1alpha2.PartitionContentConditionReady) {
			continue
		}
		hash, err := store.ParseInfoHash(strings.TrimPrefix(pc.Name, "pc-"))
		if err != nil {
			return nil, fmt.Errorf("image %q: partitioncontent %q: name is not a valid content hash: %w", image.Name, pc.Name, err)
		}
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	return hashes, nil
}

// buildImageSeederDeployment constructs the (not yet created) per-(Image,
// Site) seeder Deployment: one replica running ezio alongside
// kezio-seeder-register (same image, different command - see
// docker/seeder), mounting every content in hashes read-only at
// ingest.ContentMountPath(hash) - one ezio process serving every torrent
// the Image's slots reference, at siteIdentity's Site.
//
// The Selector.MatchLabels/pod template labels carry imageSeederInstanceLabel
// set to this Deployment's own name, which is unique per (Image, Site) by
// construction (seederdeploy.Name) - this is what makes two seeder
// Deployments' selectors mutually exclusive; see imageSeederInstanceLabel's
// doc comment. Because a Deployment's spec.selector is immutable after
// creation, this label (and everything else the selector is built from)
// must never depend on anything that can change after the Deployment is
// first created - it does not: the name is deterministic from (Image,
// Site) alone.
func (r *ImageReconciler) buildImageSeederDeployment(image *keziov1alpha2.Image, siteIdentity string, hashes []store.InfoHash, res sitederive.Resolution) *appsv1.Deployment {
	name := seederdeploy.Name(image.Name, siteIdentity)
	labels := map[string]string{
		partitionContentAppNameLabel:      partitionContentAppNameValue,
		partitionContentAppComponentLabel: partitionContentSeederComponentValue,
		imageSeederInstanceLabel:          name,
	}
	depLabels := labels
	if res.Subnet != nil {
		depLabels = make(map[string]string, len(labels)+1)
		maps.Copy(depLabels, labels)
		depLabels[partitionContentSeederSubnetLabel] = res.Subnet.Name
	}

	volumes := make([]corev1.Volume, 0, len(hashes))
	mounts := make([]corev1.VolumeMount, 0, len(hashes))
	for _, hash := range hashes {
		volName := "content-" + hash.String()[:16]
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: store.PVCName(hash),
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, MountPath: ingest.ContentMountPath(hash), ReadOnly: true})
	}

	replicas := int32(1)
	trueVal, falseVal := true, false
	containerSecurityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &falseVal,
		ReadOnlyRootFilesystem:   &trueVal,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   image.Namespace,
			Labels:      depLabels,
			Annotations: map[string]string{imageSeederSiteAnnotation: siteIdentity},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: seederPodAnnotations(res)},
				Spec: corev1.PodSpec{
					NodeSelector: nodeSelectorOrNil(res.NodeSelector),
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
							Env: []corev1.EnvVar{
								// Explicit rather than relying on
								// entrypoint.sh's own matching default, so
								// the contract with the seeder-register
								// container's EZIO_TARGET below is stated
								// once rather than implied by two defaults
								// happening to agree.
								{Name: "EZIO_GRPC_LISTEN", Value: fmt.Sprintf("0.0.0.0:%d", seederdeploy.EzioGRPCPort)},
								// Pinned rather than left to ezio's own
								// ephemeral choice - see
								// seederdeploy.EzioBTPort's doc comment for
								// why.
								{Name: "EZIO_BT_PORT", Value: strconv.Itoa(int(seederdeploy.EzioBTPort))},
							},
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: seederdeploy.EzioGRPCPort, Protocol: corev1.ProtocolTCP},
								{Name: "bt", ContainerPort: seederdeploy.EzioBTPort, Protocol: corev1.ProtocolTCP},
							},
							SecurityContext: containerSecurityContext,
							VolumeMounts:    mounts,
						},
						{
							// Same image as ezio (both ship in it - see
							// docker/seeder/Dockerfile), different command:
							// registers this pod's every mounted content
							// with the ezio container above over its
							// pod-local gRPC listener and serves each
							// .torrent over HTTP by info hash.
							Name:    "seeder-register",
							Image:   r.Seeder.Image,
							Command: []string{"/usr/local/bin/kezio-seeder-register"},
							Env:     seederRegisterEnv(r.Seeder, res),
							Ports: []corev1.ContainerPort{
								{Name: "torrent", ContainerPort: seederdeploy.TorrentHTTPPort, Protocol: corev1.ProtocolTCP},
							},
							// Proves only that the .torrent HTTP server is
							// bound and answering, not that every content
							// has finished registering - AvailableReplicas
							// (and thus PartitionContent's own seeders[])
							// must reflect "actually serving", not
							// "registration complete".
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

// listImageSeederDeployments lists the seeder Deployments in image's own
// namespace that image itself controls (metav1.IsControlledBy), keyed by
// the Site identity each carries in imageSeederSiteAnnotation. Shared by
// ImageReconciler (to find the Deployment it must reconcile per Site) and
// PartitionContentReconciler (to read seeder availability for its own
// status.seeders[] reflection) - both take image (and this Client) rather
// than a receiver, so neither reconciler needs the other's type.
func listImageSeederDeployments(ctx context.Context, c client.Client, image *keziov1alpha2.Image) (map[string]*appsv1.Deployment, error) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(image.Namespace), client.MatchingLabels{
		partitionContentAppComponentLabel: partitionContentSeederComponentValue,
	}); err != nil {
		return nil, fmt.Errorf("list seeder deployments for image %s/%s: %w", image.Namespace, image.Name, err)
	}
	out := make(map[string]*appsv1.Deployment)
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !metav1.IsControlledBy(dep, image) {
			continue
		}
		site := dep.Annotations[imageSeederSiteAnnotation]
		if site == "" {
			continue
		}
		out[site] = dep
	}
	return out, nil
}
