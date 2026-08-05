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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// requestSet converts a []reconcile.Request into a set keyed by
// NamespacedName, so tests can assert on the requests mapMachineToImages
// returns without depending on its iteration order.
func requestSet(requests []reconcile.Request) map[types.NamespacedName]struct{} {
	set := make(map[types.NamespacedName]struct{}, len(requests))
	for _, req := range requests {
		set[req.NamespacedName] = struct{}{}
	}
	return set
}

// TestMapMachineToImages checks that the Machine watch map function
// collects every distinct Image a Machine references (spec.imageRef,
// spec.dataImages, and status.provisioning), deduplicates references that
// resolve to the same Image, resolves each reference's namespace
// independently of the Machine's own namespace, and returns nil for any
// object that is not a Machine.
func TestMapMachineToImages(t *testing.T) {
	ctx := context.Background()

	t.Run("imageRef only, same namespace", func(t *testing.T) {
		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef: &keziov1alpha1.NameRef{Name: "os-image"},
			},
		}

		got := requestSet(mapMachineToImages(ctx, machine))

		want := map[types.NamespacedName]struct{}{
			{Namespace: "default", Name: "os-image"}: {},
		}
		if len(got) != len(want) {
			t.Fatalf("mapMachineToImages() = %v, want %v", got, want)
		}
		for key := range want {
			if _, ok := got[key]; !ok {
				t.Errorf("mapMachineToImages() missing %v, got %v", key, got)
			}
		}
	})

	t.Run("imageRef plus two distinct dataImages", func(t *testing.T) {
		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef: &keziov1alpha1.NameRef{Name: "os-image"},
				DataImages: []keziov1alpha1.MachineDataImage{
					{ImageRef: keziov1alpha1.NameRef{Name: "data-image-1"}},
					{ImageRef: keziov1alpha1.NameRef{Name: "data-image-2"}},
				},
			},
		}

		got := requestSet(mapMachineToImages(ctx, machine))

		want := map[types.NamespacedName]struct{}{
			{Namespace: "default", Name: "os-image"}:     {},
			{Namespace: "default", Name: "data-image-1"}: {},
			{Namespace: "default", Name: "data-image-2"}: {},
		}
		if len(got) != len(want) {
			t.Fatalf("mapMachineToImages() = %v, want %v", got, want)
		}
		for key := range want {
			if _, ok := got[key]; !ok {
				t.Errorf("mapMachineToImages() missing %v, got %v", key, got)
			}
		}
	})

	t.Run("spec and status referencing the same image dedup to one request", func(t *testing.T) {
		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef: &keziov1alpha1.NameRef{Name: "os-image"},
			},
			Status: keziov1alpha1.MachineStatus{
				Provisioning: &keziov1alpha1.MachineProvisioningStatus{
					Image: &keziov1alpha1.MachineProvisionedImage{
						ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
					},
				},
			},
		}

		requests := mapMachineToImages(ctx, machine)

		if len(requests) != 1 {
			t.Fatalf("mapMachineToImages() = %v, want exactly 1 deduplicated request", requests)
		}
		want := types.NamespacedName{Namespace: "default", Name: "os-image"}
		if requests[0].NamespacedName != want {
			t.Errorf("mapMachineToImages()[0] = %v, want %v", requests[0].NamespacedName, want)
		}
	})

	t.Run("imageRef with an explicit cross-namespace ref resolves to that namespace", func(t *testing.T) {
		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef: &keziov1alpha1.NameRef{Namespace: "other-ns", Name: "os-image"},
			},
		}

		requests := mapMachineToImages(ctx, machine)

		if len(requests) != 1 {
			t.Fatalf("mapMachineToImages() = %v, want exactly 1 request", requests)
		}
		want := types.NamespacedName{Namespace: "other-ns", Name: "os-image"}
		if requests[0].NamespacedName != want {
			t.Errorf("mapMachineToImages()[0] = %v, want %v (ref namespace, not machine namespace)", requests[0].NamespacedName, want)
		}
	})

	t.Run("no refs anywhere returns no requests", func(t *testing.T) {
		machine := &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		}

		requests := mapMachineToImages(ctx, machine)

		if len(requests) != 0 {
			t.Fatalf("mapMachineToImages() = %v, want no requests", requests)
		}
	})

	t.Run("non-Machine object returns nil", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "not-a-machine", Namespace: "default"},
		}

		requests := mapMachineToImages(ctx, pod)

		if requests != nil {
			t.Fatalf("mapMachineToImages(pod) = %v, want nil", requests)
		}
	})
}
