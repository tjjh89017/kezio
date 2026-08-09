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
	"reflect"
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
// ctrl.SetControllerReference, no API server required.
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

// The PVC is owner-refed to image as a controller reference - deleting
// the Image cascades via ordinary Kubernetes garbage collection, not a
// finalizer.
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

// A tiny or zero UsedBytes must still floor to a viable request - some
// CSI drivers reject a zero-size PVC outright.
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

	dep, err := r.buildSeederDeployment(image, "site-a", nil)
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

// The annotation is qualified against the seeder Subnet's own namespace,
// not the Image's (where the Deployment itself lives).
func TestBuildSeederDeployment_NetworkAnnotation(t *testing.T) {
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

	t.Run("bare network name is qualified with the Subnet's own namespace", func(t *testing.T) {
		// Multus resolves an unqualified default-network value in its own
		// system namespace (kube-system), not the pod's, so a bare name
		// must be stamped as <subnet namespace>/<name>.
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "hq-seeder", Namespace: "hq"},
			Spec:       keziov1alpha1.SubnetSpec{SeederNetworkRef: &keziov1alpha1.NameRef{Name: "kezio-seeder-network"}},
		}
		dep, err := r.buildSeederDeployment(image, "site-a", seederSubnet)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		got := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]
		if got != "hq/kezio-seeder-network" {
			t.Errorf("pod template default-network annotation = %q, want %q", got, "hq/kezio-seeder-network")
		}
	})

	t.Run("qualified network name is kept verbatim", func(t *testing.T) {
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "hq-seeder", Namespace: "hq"},
			Spec:       keziov1alpha1.SubnetSpec{SeederNetworkRef: &keziov1alpha1.NameRef{Namespace: "other-ns", Name: "shared-network"}},
		}
		dep, err := r.buildSeederDeployment(image, "site-a", seederSubnet)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		got := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]
		if got != "other-ns/shared-network" {
			t.Errorf("pod template default-network annotation = %q, want %q", got, "other-ns/shared-network")
		}
	})

	t.Run("seeder Subnet has no SeederNetworkRef", func(t *testing.T) {
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "hq-seeder", Namespace: "hq"},
		}
		dep, err := r.buildSeederDeployment(image, "site-a", seederSubnet)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		if _, ok := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]; ok {
			t.Errorf("pod template carries a default-network annotation despite no SeederNetworkRef: %+v", dep.Spec.Template.Annotations)
		}
	})

	t.Run("nil seeder Subnet", func(t *testing.T) {
		dep, err := r.buildSeederDeployment(image, "site-a", nil)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		if _, ok := dep.Spec.Template.Annotations["v1.multus-cni.io/default-network"]; ok {
			t.Errorf("pod template carries a default-network annotation despite a nil seeder Subnet: %+v", dep.Spec.Template.Annotations)
		}
	})
}

func TestBuildSeederDeployment_NodeSelector(t *testing.T) {
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

	t.Run("propagates the seeder Subnet's own NodeSelector", func(t *testing.T) {
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "hq-seeder", Namespace: "hq"},
			Spec:       keziov1alpha1.SubnetSpec{NodeSelector: map[string]string{"segment": "hq-seeder-vlan"}},
		}
		dep, err := r.buildSeederDeployment(image, "site-a", seederSubnet)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		got := dep.Spec.Template.Spec.NodeSelector
		want := map[string]string{"segment": "hq-seeder-vlan"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pod NodeSelector = %#v, want %#v", got, want)
		}
	})

	t.Run("empty NodeSelector leaves the pod unconstrained, not an empty map", func(t *testing.T) {
		seederSubnet := &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "hq-seeder", Namespace: "hq"},
			Spec:       keziov1alpha1.SubnetSpec{NodeSelector: map[string]string{}},
		}
		dep, err := r.buildSeederDeployment(image, "site-a", seederSubnet)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		if got := dep.Spec.Template.Spec.NodeSelector; got != nil {
			t.Errorf("pod NodeSelector = %#v, want nil (unconstrained), not an empty map", got)
		}
	})

	t.Run("nil seeder Subnet leaves the pod unconstrained", func(t *testing.T) {
		dep, err := r.buildSeederDeployment(image, "site-a", nil)
		if err != nil {
			t.Fatalf("buildSeederDeployment: %v", err)
		}
		if got := dep.Spec.Template.Spec.NodeSelector; got != nil {
			t.Errorf("pod NodeSelector = %#v, want nil for a nil seeder Subnet", got)
		}
	})
}

// Pins seederdeploy.TorrentHTTPPort on the container so it can't drift
// from agentserver's resolveSeederTorrentURL, which assumes the same port.
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
	dep, err := r.buildSeederDeployment(image, "site-a", nil)
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
