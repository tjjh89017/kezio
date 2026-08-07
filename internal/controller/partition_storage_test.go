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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// newPartitionStorageTestScheme builds a scheme with kezio's own types
// registered - all buildPartitionPVC/buildSeederDeployment need to call
// ctrl.SetControllerReference, no API server required (see
// setupAgentWalkEnv's doc comment for why a plain *testing.T test in
// this package builds its own scheme rather than reusing the
// Ginkgo-managed one).
func newPartitionStorageTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme (core/v1): %v", err)
	}
	return s
}

// TestPartitionPVCName checks that partitionPVCName is deterministic,
// distinguishes two partitions of the same Image (which would otherwise
// collide, since both derive from the same Image name), and never
// exceeds the PVC name length limit even for a Image name long enough to
// need truncation.
func TestPartitionPVCName(t *testing.T) {
	first := partitionPVCName("os-image", 1)
	second := partitionPVCName("os-image", 2)
	repeat := partitionPVCName("os-image", 1)

	if first == second {
		t.Fatalf("partitionPVCName() collided across partitions: %q", first)
	}
	if first != repeat {
		t.Fatalf("partitionPVCName() not deterministic: %q != %q", first, repeat)
	}

	longName := strings.Repeat("a", 300)
	truncated1 := partitionPVCName(longName, 1)
	truncated2 := partitionPVCName(longName, 2)
	if len(truncated1) > maxPartitionPVCNameLength {
		t.Fatalf("partitionPVCName() exceeded %d chars: %q", maxPartitionPVCNameLength, truncated1)
	}
	if truncated1 == truncated2 {
		t.Fatalf("partitionPVCName() collided across partitions after truncation: %q", truncated1)
	}
}

// TestBuildPartitionPVC checks that buildPartitionPVC sizes the request
// from UsedBytes (floored at minPartitionStorageSize), applies the
// configured access modes and StorageClass, and owner-refs the PVC to
// image as a controller reference - the mechanism that makes deleting
// the Image cascade to its partitions' content (ordinary Kubernetes
// garbage collection, not a finalizer).
func TestBuildPartitionPVC(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default", UID: "img-uid"}}
	r := &ImageReconciler{
		Scheme: newPartitionStorageTestScheme(t),
		Ingest: IngestConfig{PartitionStorageClassName: "rwx-class"},
	}

	pvc, err := r.buildPartitionPVC(image, keziov1alpha1.ImagePartitionStatus{Number: 3, InfoHash: "aa", UsedBytes: 5 * 1024 * 1024})
	if err != nil {
		t.Fatalf("buildPartitionPVC: %v", err)
	}

	if pvc.Name != partitionPVCName("os-image", 3) {
		t.Errorf("pvc.Name = %q, want %q", pvc.Name, partitionPVCName("os-image", 3))
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != 5*1024*1024 {
		t.Errorf("requested storage = %v, want 5Mi", got)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "rwx-class" {
		t.Errorf("StorageClassName = %v, want rwx-class", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("AccessModes = %v, want [ReadWriteOnce] (the default)", pvc.Spec.AccessModes)
	}

	if len(pvc.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %v, want exactly one", pvc.OwnerReferences)
	}
	owner := pvc.OwnerReferences[0]
	if owner.Name != "os-image" || owner.Controller == nil || !*owner.Controller {
		t.Errorf("owner reference = %+v, want a controller reference to os-image", owner)
	}
}

// TestBuildPartitionPVC_FloorsTinyRequests checks that a partition whose
// UsedBytes is smaller than minPartitionStorageSize (including zero)
// still gets a viable PVC request rather than one some CSI drivers
// reject outright.
func TestBuildPartitionPVC_FloorsTinyRequests(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	r := &ImageReconciler{Scheme: newPartitionStorageTestScheme(t)}

	pvc, err := r.buildPartitionPVC(image, keziov1alpha1.ImagePartitionStatus{Number: 1, InfoHash: "aa", UsedBytes: 0})
	if err != nil {
		t.Fatalf("buildPartitionPVC: %v", err)
	}
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.IsZero() {
		t.Errorf("requested storage is zero, want at least the floor (%s)", minPartitionStorageSize)
	}
}

// TestBuildSeederDeployment_MountsOnlyContentPartitions checks that
// buildSeederDeployment mounts exactly one read-only volume per content
// partition (InfoHash != ""), skipping swap and blank partitions, at
// the deterministic partitionMountPath each partition's own PVC -
// never a single shared store volume.
func TestBuildSeederDeployment_MountsOnlyContentPartitions(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"},
		Status: keziov1alpha1.ImageStatus{
			Partitions: []keziov1alpha1.ImagePartitionStatus{
				{Number: 1, Role: keziov1alpha1.PartitionRoleESP, InfoHash: "hash-1"},
				{Number: 2, Role: keziov1alpha1.PartitionRoleSwap, UUID: "swap-uuid"},
				{Number: 3, Role: keziov1alpha1.PartitionRoleData, InfoHash: "hash-3"},
				{Number: 4, Role: keziov1alpha1.PartitionRoleData}, // blank: no infoHash
			},
		},
	}
	r := &ImageReconciler{
		Scheme:           newPartitionStorageTestScheme(t),
		SeederDeployment: SeederDeploymentConfig{Image: "ezio-seeder:test"},
	}

	dep, err := r.buildSeederDeployment(image, "site-a")
	if err != nil {
		t.Fatalf("buildSeederDeployment: %v", err)
	}

	container := dep.Spec.Template.Spec.Containers[0]
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("len(VolumeMounts) = %d, want 2 (partitions 1 and 3 only)", len(container.VolumeMounts))
	}
	if len(dep.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("len(Volumes) = %d, want 2", len(dep.Spec.Template.Spec.Volumes))
	}

	mountsByPath := map[string]corev1.VolumeMount{}
	for _, m := range container.VolumeMounts {
		mountsByPath[m.MountPath] = m
	}
	for _, number := range []int32{1, 3} {
		m, ok := mountsByPath[partitionMountPath(number)]
		if !ok {
			t.Errorf("no VolumeMount at %s (partition %d)", partitionMountPath(number), number)
			continue
		}
		if !m.ReadOnly {
			t.Errorf("VolumeMount at %s is not read-only", partitionMountPath(number))
		}
	}

	volumesByName := map[string]corev1.Volume{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		volumesByName[v.Name] = v
	}
	for _, m := range container.VolumeMounts {
		v, ok := volumesByName[m.Name]
		if !ok {
			t.Fatalf("VolumeMount %q has no matching Volume", m.Name)
		}
		if v.PersistentVolumeClaim == nil {
			t.Fatalf("Volume %q is not backed by a PVC", v.Name)
		}
		if !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("Volume %q's PVC source is not read-only", v.Name)
		}
	}
}

// TestBuildSeederDeployment_NetworkAnnotation checks that
// SeederDeploymentConfig.Network, when set, lands on the pod template as
// the Multus default-network annotation - the mechanism that makes the
// seeder pod single-homed on the provisioning network so its PodIP is
// directly reachable (see resolveSeederTorrentURL in
// internal/agentserver/plan.go) - and that leaving it empty (every
// existing deployment and the envtest suite) omits the annotation
// entirely rather than setting it to an empty string.
func TestBuildSeederDeployment_NetworkAnnotation(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"},
		Status: keziov1alpha1.ImageStatus{
			Partitions: []keziov1alpha1.ImagePartitionStatus{
				{Number: 1, Role: keziov1alpha1.PartitionRoleESP, InfoHash: "hash-1"},
			},
		},
	}

	t.Run("bare network name is namespace-qualified", func(t *testing.T) {
		// Multus resolves an unqualified default-network value in its own
		// system namespace (kube-system), not the pod's, so a bare name
		// must be stamped as <image namespace>/<name>.
		r := &ImageReconciler{
			Scheme:           newPartitionStorageTestScheme(t),
			SeederDeployment: SeederDeploymentConfig{Image: "ezio-seeder:test", Network: "kezio-seeder-network"},
		}
		dep, err := r.buildSeederDeployment(image, "site-a")
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		got := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]
		if got != "default/kezio-seeder-network" {
			t.Errorf("pod template default-network annotation = %q, want %q", got, "default/kezio-seeder-network")
		}
	})

	t.Run("qualified network name is kept verbatim", func(t *testing.T) {
		r := &ImageReconciler{
			Scheme:           newPartitionStorageTestScheme(t),
			SeederDeployment: SeederDeploymentConfig{Image: "ezio-seeder:test", Network: "other-ns/shared-network"},
		}
		dep, err := r.buildSeederDeployment(image, "site-a")
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		got := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]
		if got != "other-ns/shared-network" {
			t.Errorf("pod template default-network annotation = %q, want %q", got, "other-ns/shared-network")
		}
	})

	t.Run("network unset", func(t *testing.T) {
		r := &ImageReconciler{
			Scheme:           newPartitionStorageTestScheme(t),
			SeederDeployment: SeederDeploymentConfig{Image: "ezio-seeder:test"},
		}
		dep, err := r.buildSeederDeployment(image, "site-a")
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		if _, ok := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]; ok {
			t.Errorf("pod template carries a default-network annotation despite Network being unset: %+v", dep.Spec.Template.Annotations)
		}
	})
}

// TestBuildSeederDeployment_RegisterContainerExposesTorrentPort checks
// that the seeder-register container (cmd/seeder, wired as
// kezio-seeder-register) exposes seederdeploy.TorrentHTTPPort - the port
// its own HTTP server listens on to serve .torrent files by info hash -
// so agentserver's resolveSeederTorrentURL and this container's actual
// listener never drift apart.
func TestBuildSeederDeployment_RegisterContainerExposesTorrentPort(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"},
		Status: keziov1alpha1.ImageStatus{
			Partitions: []keziov1alpha1.ImagePartitionStatus{
				{Number: 1, Role: keziov1alpha1.PartitionRoleESP, InfoHash: "hash-1"},
			},
		},
	}
	r := &ImageReconciler{
		Scheme:           newPartitionStorageTestScheme(t),
		SeederDeployment: SeederDeploymentConfig{Image: "ezio-seeder:test"},
	}
	dep, err := r.buildSeederDeployment(image, "site-a")
	if err != nil {
		t.Fatalf("buildSeederDeployment: %v", err)
	}

	register := dep.Spec.Template.Spec.Containers[1]
	if register.Name != "seeder-register" {
		t.Fatalf("Containers[1].Name = %q, want seeder-register", register.Name)
	}
	for _, p := range register.Ports {
		if p.ContainerPort == seederdeploy.TorrentHTTPPort {
			return
		}
	}
	t.Errorf("seeder-register container has no port %d: %+v", seederdeploy.TorrentHTTPPort, register.Ports)
}
