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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/sitederive"
	"github.com/tjjh89017/kezio/internal/store"
)

// seederPlacementTestHash is a valid-looking 40-character hex info hash,
// distinct from partitionContentTestHash's sequence so these plain unit
// tests never collide with an envtest-created PartitionContent name.
const seederPlacementTestHash = "0000000000000000000000000000000009999999"

func seederPlacementTestPC() *keziov1alpha2.PartitionContent {
	return newTestPartitionContent("pc-" + seederPlacementTestHash)
}

func seederPlacementTestReconciler() *PartitionContentReconciler {
	return &PartitionContentReconciler{Seeder: PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"}}
}

// TestBuildSeederDeploymentNoResolutionUnchanged is the golden-shape
// check: a zero sitederive.Resolution (no Subnet resolved placement) must
// build a Deployment carrying no Multus annotation and no nodeSelector -
// the exact shape a Subnet-less environment (for example the image-only
// CI lane) has always built.
func TestBuildSeederDeploymentNoResolutionUnchanged(t *testing.T) {
	r := seederPlacementTestReconciler()
	pc := seederPlacementTestPC()
	hash, err := store.ParseInfoHash(seederPlacementTestHash)
	if err != nil {
		t.Fatalf("ParseInfoHash: %v", err)
	}

	dep := r.buildSeederDeployment(pc, hash, sitederive.Resolution{})

	if dep.Spec.Template.Annotations != nil {
		t.Errorf("Template.Annotations = %+v, want nil", dep.Spec.Template.Annotations)
	}
	if dep.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("Template.Spec.NodeSelector = %+v, want nil", dep.Spec.Template.Spec.NodeSelector)
	}
	if _, ok := dep.Labels[partitionContentSeederSubnetLabel]; ok {
		t.Errorf("Labels carry %s, want it absent with no Subnet resolved", partitionContentSeederSubnetLabel)
	}
	if _, ok := dep.Spec.Selector.MatchLabels[partitionContentSeederSubnetLabel]; ok {
		t.Errorf("Selector.MatchLabels carry %s, want it absent", partitionContentSeederSubnetLabel)
	}
}

// TestBuildSeederDeploymentWithSeederNetworkRefAddsPlacement checks that a
// resolved Subnet with SeederNetworkRef and NodeSelector adds the Multus
// annotation, the nodeSelector, and the seeder-subnet label - and that
// none of it leaks into Selector.MatchLabels, which must stay stable
// across a Subnet change (an immutable field once the Deployment exists).
func TestBuildSeederDeploymentWithSeederNetworkRefAddsPlacement(t *testing.T) {
	r := seederPlacementTestReconciler()
	pc := seederPlacementTestPC()
	hash, err := store.ParseInfoHash(seederPlacementTestHash)
	if err != nil {
		t.Fatalf("ParseInfoHash: %v", err)
	}
	subnet := &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-1", Namespace: pc.Namespace},
		Spec: keziov1alpha2.SubnetSpec{
			SeederNetworkRef: &keziov1alpha2.NameRef{Name: "seeder-nad"},
			NodeSelector:     map[string]string{"kubernetes.io/hostname": "node-1"},
		},
	}
	res := sitederive.ResolveSubnet(subnet)

	dep := r.buildSeederDeployment(pc, hash, res)

	wantAnnotation := pc.Namespace + "/seeder-nad"
	if got := dep.Spec.Template.Annotations[multusNetworksAnnotation]; got != wantAnnotation {
		t.Errorf("Multus annotation = %q, want %q", got, wantAnnotation)
	}
	if got := dep.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; got != "node-1" {
		t.Errorf("NodeSelector[kubernetes.io/hostname] = %q, want %q", got, "node-1")
	}
	if got := dep.Labels[partitionContentSeederSubnetLabel]; got != "rack-1" {
		t.Errorf("Labels[%s] = %q, want %q", partitionContentSeederSubnetLabel, got, "rack-1")
	}
	if _, ok := dep.Spec.Selector.MatchLabels[partitionContentSeederSubnetLabel]; ok {
		t.Errorf("Selector.MatchLabels carry %s, want it absent", partitionContentSeederSubnetLabel)
	}
	if _, ok := dep.Spec.Template.Labels[partitionContentSeederSubnetLabel]; ok {
		t.Errorf("Template.Labels carry %s, want it absent", partitionContentSeederSubnetLabel)
	}
}

// TestBuildSeederDeploymentSeederNetworkRefNamespaceDefaulting checks that
// a bare (namespace-less) SeederNetworkRef qualifies against the resolved
// Subnet's own namespace, and an explicit one is left as declared -
// mirroring bootdPodAnnotations.
func TestBuildSeederDeploymentSeederNetworkRefNamespaceDefaulting(t *testing.T) {
	r := seederPlacementTestReconciler()
	pc := seederPlacementTestPC()
	hash, err := store.ParseInfoHash(seederPlacementTestHash)
	if err != nil {
		t.Fatalf("ParseInfoHash: %v", err)
	}

	subnet := &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-1", Namespace: "seeder-subnet-ns"},
		Spec:       keziov1alpha2.SubnetSpec{SeederNetworkRef: &keziov1alpha2.NameRef{Name: "seeder-nad"}},
	}
	dep := r.buildSeederDeployment(pc, hash, sitederive.ResolveSubnet(subnet))
	if got, want := dep.Spec.Template.Annotations[multusNetworksAnnotation], "seeder-subnet-ns/seeder-nad"; got != want {
		t.Errorf("Multus annotation = %q, want %q (defaulted against Subnet's own namespace)", got, want)
	}

	subnet.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Namespace: "explicit-ns", Name: "seeder-nad"}
	dep = r.buildSeederDeployment(pc, hash, sitederive.ResolveSubnet(subnet))
	if got, want := dep.Spec.Template.Annotations[multusNetworksAnnotation], "explicit-ns/seeder-nad"; got != want {
		t.Errorf("Multus annotation = %q, want %q (explicit namespace kept)", got, want)
	}
}
