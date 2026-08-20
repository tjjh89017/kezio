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

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestPartitionContentUpdatePredicateUpdate(t *testing.T) {
	base := func() *keziov1alpha2.PartitionContent {
		return &keziov1alpha2.PartitionContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "pc-test",
				Generation:      1,
				Annotations:     map[string]string{"a": "1"},
				ResourceVersion: "1",
			},
			Status: keziov1alpha2.PartitionContentStatus{State: keziov1alpha2.PartitionContentStatePending},
		}
	}

	cases := []struct {
		name  string
		newer func(*keziov1alpha2.PartitionContent)
		want  bool
	}{
		{
			name: "status-only self-write does not trigger",
			newer: func(pc *keziov1alpha2.PartitionContent) {
				pc.Status.State = keziov1alpha2.PartitionContentStatePublishing
				pc.ResourceVersion = "2"
			},
			want: false,
		},
		{
			name:  "generation bump triggers",
			newer: func(pc *keziov1alpha2.PartitionContent) { pc.Generation = 2 },
			want:  true,
		},
		{
			name:  "annotation change triggers",
			newer: func(pc *keziov1alpha2.PartitionContent) { pc.Annotations["a"] = "2" },
			want:  true,
		},
		{
			name:  "finalizers change triggers",
			newer: func(pc *keziov1alpha2.PartitionContent) { pc.Finalizers = append(pc.Finalizers, "extra/finalizer") },
			want:  true,
		},
		{
			name:  "no change at all does not trigger",
			newer: func(*keziov1alpha2.PartitionContent) {},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := base()
			newObj := base()
			tc.newer(newObj)

			got := partitionContentUpdatePredicate.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != tc.want {
				t.Errorf("partitionContentUpdatePredicate.Update() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPartitionContentUpdatePredicateLeavesOtherEventsUnfiltered(t *testing.T) {
	pc := &keziov1alpha2.PartitionContent{ObjectMeta: metav1.ObjectMeta{Name: "pc-test"}}

	if !partitionContentUpdatePredicate.Create(event.CreateEvent{Object: pc}) {
		t.Error("Create event must not be filtered")
	}
	if !partitionContentUpdatePredicate.Delete(event.DeleteEvent{Object: pc}) {
		t.Error("Delete event must not be filtered")
	}
	if !partitionContentUpdatePredicate.Generic(event.GenericEvent{Object: pc}) {
		t.Error("Generic event must not be filtered")
	}
}
