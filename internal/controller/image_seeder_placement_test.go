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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// TestSeederPodAnnotationsQualifiesBareRefWithSeedingSubnetNamespace pins
// the rule Multus needs: a bare SeederNetworkRef name must resolve
// against the seeding Subnet's own namespace, not the seeder
// Deployment's (the Image's own). Multus resolves an unqualified
// default-network annotation value in its own system namespace, so
// qualifying with the wrong one silently points at a NAD that does not
// exist.
func TestSeederPodAnnotationsQualifiesBareRefWithSeedingSubnetNamespace(t *testing.T) {
	res := sitederive.Resolution{
		HasSeeder:        true,
		SeederNetworkRef: &keziov1alpha3.NameRef{Name: "seeder-nad"},
		Subnet: &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "seeding-subnet", Namespace: "site-ns"},
		},
	}

	annotations := seederPodAnnotations(res)
	got := annotations[multusDefaultNetworkAnnotation]
	want := "site-ns/seeder-nad"
	if got != want {
		t.Errorf("seederPodAnnotations()[%s] = %q, want %q (the seeding Subnet's namespace, not the Image's)", multusDefaultNetworkAnnotation, got, want)
	}
}

// TestSeederPodAnnotationsExplicitRefNamespaceWins pins that an explicit
// Namespace on the ref itself is never overridden by the seeding
// Subnet's own namespace.
func TestSeederPodAnnotationsExplicitRefNamespaceWins(t *testing.T) {
	res := sitederive.Resolution{
		HasSeeder:        true,
		SeederNetworkRef: &keziov1alpha3.NameRef{Namespace: "explicit-ns", Name: "seeder-nad"},
		Subnet: &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "seeding-subnet", Namespace: "site-ns"},
		},
	}

	got := seederPodAnnotations(res)[multusDefaultNetworkAnnotation]
	want := "explicit-ns/seeder-nad"
	if got != want {
		t.Errorf("seederPodAnnotations()[%s] = %q, want %q", multusDefaultNetworkAnnotation, got, want)
	}
}

// TestSeederPodAnnotationsNilWithoutSeederNetworkRef pins the supported
// "seeding Subnet with no data-plane NAD" topology: the seeder pod gets
// no Multus default-network annotation and runs on the ordinary cluster
// network instead.
func TestSeederPodAnnotationsNilWithoutSeederNetworkRef(t *testing.T) {
	res := sitederive.Resolution{
		HasSeeder: true,
		Subnet: &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "seeding-subnet", Namespace: "site-ns"},
		},
	}

	if got := seederPodAnnotations(res); got != nil {
		t.Errorf("seederPodAnnotations() = %v, want nil when res carries no SeederNetworkRef", got)
	}
}
