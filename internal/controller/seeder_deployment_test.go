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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// TestMachineHoldsSeederReference checks the reference-holding state set
// enumerated in machineHoldsSeederReference's doc comment: Provisioning
// always holds, Error only holds when it is retrying a provisioning
// failure, and every other state (including Error from a different
// phase) does not.
func TestMachineHoldsSeederReference(t *testing.T) {
	withReason := func(reason string) []metav1.Condition {
		return []metav1.Condition{{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason}}
	}

	cases := []struct {
		name  string
		state string
		conds []metav1.Condition
		want  bool
	}{
		{"Provisioning holds", keziov1alpha1.MachineStateProvisioning, nil, true},
		{"Error retrying a provisioning failure holds", keziov1alpha1.MachineStateError, withReason(reasonProvisionFailed), true},
		{"Error retrying a register failure does not hold", keziov1alpha1.MachineStateError, withReason(reasonRegisterFailed), false},
		{"Error retrying an inspect failure does not hold", keziov1alpha1.MachineStateError, withReason(reasonInspectFailed), false},
		{"Error with no recorded reason does not hold", keziov1alpha1.MachineStateError, nil, false},
		{"Enrolling does not hold", keziov1alpha1.MachineStateEnrolling, nil, false},
		{"Inspecting does not hold", keziov1alpha1.MachineStateInspecting, nil, false},
		{"Available does not hold", keziov1alpha1.MachineStateAvailable, nil, false},
		{"Provisioned does not hold", keziov1alpha1.MachineStateProvisioned, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha1.Machine{
				Status: keziov1alpha1.MachineStatus{
					State:      tc.state,
					Conditions: tc.conds,
				},
			}
			if got := machineHoldsSeederReference(machine); got != tc.want {
				t.Errorf("machineHoldsSeederReference(state=%s) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestSeederDemandBySite checks that seederDemandBySite counts only
// Machines that both reference the given Image and hold a seeder
// reference (per machineHoldsSeederReference), groups them by
// spec.networkSite, counts a Machine at most once even if it references
// the Image twice, and ignores Machines referencing a different Image.
func TestSeederDemandBySite(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}

	provisioning := func(name, site string, ref keziov1alpha1.NameRef, dataRefs ...keziov1alpha1.NameRef) keziov1alpha1.Machine {
		dataImages := make([]keziov1alpha1.MachineDataImage, 0, len(dataRefs))
		for _, d := range dataRefs {
			dataImages = append(dataImages, keziov1alpha1.MachineDataImage{ImageRef: d})
		}
		return keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef:    &ref,
				DataImages:  dataImages,
				NetworkSite: site,
			},
			Status: keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateProvisioning},
		}
	}

	machines := &keziov1alpha1.MachineList{
		Items: []keziov1alpha1.Machine{
			// two Provisioning Machines at site "a", referencing the target Image.
			provisioning("m1", "a", keziov1alpha1.NameRef{Name: "os-image"}),
			provisioning("m2", "a", keziov1alpha1.NameRef{Name: "os-image"}),
			// one Provisioning Machine at site "b".
			provisioning("m3", "b", keziov1alpha1.NameRef{Name: "os-image"}),
			// references the target Image twice (imageRef and dataImages) -
			// must still count once.
			provisioning("m4", "b", keziov1alpha1.NameRef{Name: "os-image"}, keziov1alpha1.NameRef{Name: "os-image"}),
			// Provisioning, but a different Image - must not count.
			provisioning("m5", "a", keziov1alpha1.NameRef{Name: "other-image"}),
			// Available (does not hold a seeder reference) - must not count
			// even though it references the target Image.
			{
				ObjectMeta: metav1.ObjectMeta{Name: "m6", Namespace: "default"},
				Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}, NetworkSite: "a"},
				Status:     keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateAvailable},
			},
			// Error retrying a provisioning failure, no networkSite set -
			// counts under the empty-string site.
			{
				ObjectMeta: metav1.ObjectMeta{Name: "m7", Namespace: "default"},
				Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}},
				Status: keziov1alpha1.MachineStatus{
					State: keziov1alpha1.MachineStateError,
					Conditions: []metav1.Condition{
						{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonProvisionFailed},
					},
				},
			},
		},
	}

	got := seederDemandBySite(machines, image)
	want := map[string]int32{"a": 2, "b": 2, "": 1}

	if len(got) != len(want) {
		t.Fatalf("seederDemandBySite() = %v, want %v", got, want)
	}
	for site, count := range want {
		if got[site] != count {
			t.Errorf("seederDemandBySite()[%q] = %d, want %d", site, got[site], count)
		}
	}
}

// TestSeederDeploymentName checks that seederDeploymentName is
// deterministic and distinguishes two sites of the same Image (which
// would otherwise collide, since both derive from the same Image name).
func TestSeederDeploymentName(t *testing.T) {
	first := seederDeploymentName("os-image", "site-a")
	second := seederDeploymentName("os-image", "site-b")
	repeat := seederDeploymentName("os-image", "site-a")

	if first == second {
		t.Fatalf("seederDeploymentName() collided across sites: %q", first)
	}
	if first != repeat {
		t.Fatalf("seederDeploymentName() not deterministic: %q != %q", first, repeat)
	}
	if len(first) > maxSeederDeploymentNameLength || len(second) > maxSeederDeploymentNameLength {
		t.Fatalf("seederDeploymentName() exceeded %d chars: %q, %q", maxSeederDeploymentNameLength, first, second)
	}
}
