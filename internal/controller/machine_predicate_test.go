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
	"sigs.k8s.io/controller-runtime/pkg/event"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestMachineUpdatePredicateUpdate(t *testing.T) {
	base := func() *keziov1alpha3.Machine {
		return &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "m",
				Generation:      1,
				Finalizers:      []string{keziov1alpha3.MachineFinalizer},
				Annotations:     map[string]string{"a": "1"},
				ResourceVersion: "1",
			},
			Status: keziov1alpha3.MachineStatus{State: keziov1alpha3.MachineStateEnrolling},
		}
	}

	cases := []struct {
		name  string
		newer func(*keziov1alpha3.Machine)
		want  bool
	}{
		{
			name: "status-only self-write does not trigger",
			newer: func(m *keziov1alpha3.Machine) {
				m.Status.State = keziov1alpha3.MachineStateInspecting
				m.ResourceVersion = "2"
			},
			want: false,
		},
		{
			name:  "generation bump triggers",
			newer: func(m *keziov1alpha3.Machine) { m.Generation = 2 },
			want:  true,
		},
		{
			name:  "annotation change triggers",
			newer: func(m *keziov1alpha3.Machine) { m.Annotations["a"] = "2" },
			want:  true,
		},
		{
			name:  "finalizers change triggers",
			newer: func(m *keziov1alpha3.Machine) { m.Finalizers = append(m.Finalizers, "extra/finalizer") },
			want:  true,
		},
		{
			name:  "no change at all does not trigger",
			newer: func(*keziov1alpha3.Machine) {},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := base()
			newObj := base()
			tc.newer(newObj)

			got := machineUpdatePredicate.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != tc.want {
				t.Errorf("machineUpdatePredicate.Update() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMachineUpdatePredicateLeavesOtherEventsUnfiltered(t *testing.T) {
	m := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m"}}

	if !machineUpdatePredicate.Create(event.CreateEvent{Object: m}) {
		t.Error("Create event must not be filtered")
	}
	if !machineUpdatePredicate.Delete(event.DeleteEvent{Object: m}) {
		t.Error("Delete event must not be filtered")
	}
	if !machineUpdatePredicate.Generic(event.GenericEvent{Object: m}) {
		t.Error("Generic event must not be filtered")
	}
}

func TestFinalizersChangedPredicateUpdate(t *testing.T) {
	cases := []struct {
		name string
		old  []string
		new  []string
		want bool
	}{
		{"unchanged", []string{"a", "b"}, []string{"a", "b"}, false},
		{"reordered only", []string{"a", "b"}, []string{"b", "a"}, false},
		{"added", []string{"a"}, []string{"a", "b"}, true},
		{"removed", []string{"a", "b"}, []string{"a"}, true},
		{"both nil", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Finalizers: tc.old}}
			newObj := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Finalizers: tc.new}}
			got := finalizersChangedPredicate.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != tc.want {
				t.Errorf("finalizersChangedPredicate.Update() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeployRunDeletionOnlyPredicate(t *testing.T) {
	run := &keziov1alpha3.DeployRun{}

	if deployRunDeletionOnly.Create(event.CreateEvent{Object: run}) {
		t.Error("Create must be filtered out")
	}
	if deployRunDeletionOnly.Update(event.UpdateEvent{ObjectOld: run, ObjectNew: run}) {
		t.Error("Update (including progress-only status writes) must be filtered out")
	}
	if !deployRunDeletionOnly.Delete(event.DeleteEvent{Object: run}) {
		t.Error("Delete must pass through")
	}
	if deployRunDeletionOnly.Generic(event.GenericEvent{Object: run}) {
		t.Error("Generic must be filtered out")
	}
}
