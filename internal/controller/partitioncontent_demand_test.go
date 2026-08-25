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
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestClaimImageRefs(t *testing.T) {
	cases := []struct {
		name  string
		claim *keziov1alpha3.MachineClaim
		want  []client.ObjectKey
	}{
		{
			name:  "no imageRef and no dataImages returns nothing",
			claim: &keziov1alpha3.MachineClaim{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}},
			want:  nil,
		},
		{
			name: "imageRef with no namespace defaults to the claim's own",
			claim: &keziov1alpha3.MachineClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec:       keziov1alpha3.MachineClaimSpec{ImageRef: &keziov1alpha3.NameRef{Name: "os-image"}},
			},
			want: []client.ObjectKey{{Namespace: "ns", Name: "os-image"}},
		},
		{
			name: "imageRef with an explicit namespace is respected",
			claim: &keziov1alpha3.MachineClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec:       keziov1alpha3.MachineClaimSpec{ImageRef: &keziov1alpha3.NameRef{Namespace: "other", Name: "os-image"}},
			},
			want: []client.ObjectKey{{Namespace: "other", Name: "os-image"}},
		},
		{
			name: "imageRef plus dataImages, deduplicated",
			claim: &keziov1alpha3.MachineClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec: keziov1alpha3.MachineClaimSpec{
					ImageRef: &keziov1alpha3.NameRef{Name: "os-image"},
					DataImages: []keziov1alpha3.MachineDataImage{
						{ImageRef: keziov1alpha3.NameRef{Name: "data-image"}},
						{ImageRef: keziov1alpha3.NameRef{Name: "os-image"}}, // duplicate of ImageRef
					},
				},
			},
			want: []client.ObjectKey{
				{Namespace: "ns", Name: "os-image"},
				{Namespace: "ns", Name: "data-image"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claimImageRefs(tc.claim)
			if len(got) != len(tc.want) {
				t.Fatalf("claimImageRefs() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("claimImageRefs()[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestAnyLiveClaim(t *testing.T) {
	live := func(name string) keziov1alpha3.MachineClaim {
		return keziov1alpha3.MachineClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	deleting := func(name string) keziov1alpha3.MachineClaim {
		now := metav1.Now()
		return keziov1alpha3.MachineClaim{ObjectMeta: metav1.ObjectMeta{Name: name, DeletionTimestamp: &now}}
	}

	cases := []struct {
		name   string
		claims []keziov1alpha3.MachineClaim
		want   bool
	}{
		{name: "no claims", claims: nil, want: false},
		{name: "one live claim", claims: []keziov1alpha3.MachineClaim{live("a")}, want: true},
		{name: "one deleting claim", claims: []keziov1alpha3.MachineClaim{deleting("a")}, want: false},
		{name: "a deleting claim alongside a live one", claims: []keziov1alpha3.MachineClaim{deleting("a"), live("b")}, want: true},
		{name: "every claim deleting", claims: []keziov1alpha3.MachineClaim{deleting("a"), deleting("b")}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyLiveClaim(tc.claims); got != tc.want {
				t.Errorf("anyLiveClaim() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestObjectKeyIndexString(t *testing.T) {
	got := objectKeyIndexString(client.ObjectKey{Namespace: "ns", Name: "name"})
	want := "ns/name"
	if got != want {
		t.Errorf("objectKeyIndexString() = %q, want %q", got, want)
	}
}
