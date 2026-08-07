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

package ingest

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func newLayoutTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme, err := keziov1alpha1.SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestImageLayoutName(t *testing.T) {
	if got, want := ImageLayoutName("golden"), "golden-layout"; got != want {
		t.Errorf("ImageLayoutName(golden) = %q, want %q", got, want)
	}
}

func TestWriteImageLayout_CreatesWithOwnerRef(t *testing.T) {
	c := newLayoutTestClient(t)
	owner := ImageOwnerRef{Name: "golden", UID: types.UID("abc-123"), APIVersion: "kezio.kojuro.date/v1alpha1", Kind: "Image"}

	if err := WriteImageLayout(context.Background(), c, "default", owner, `{"partitiontable":{}}`); err != nil {
		t.Fatalf("WriteImageLayout: %v", err)
	}

	layout := &keziov1alpha1.ImageLayout{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "golden-layout"}, layout); err != nil {
		t.Fatalf("get imagelayout: %v", err)
	}
	if layout.Spec.SfdiskJSON != `{"partitiontable":{}}` {
		t.Errorf("layout.Spec.SfdiskJSON = %q, want the sfdisk JSON", layout.Spec.SfdiskJSON)
	}
	if len(layout.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want 1", len(layout.OwnerReferences))
	}
	ref := layout.OwnerReferences[0]
	if ref.Name != owner.Name || ref.UID != owner.UID || ref.APIVersion != owner.APIVersion || ref.Kind != owner.Kind {
		t.Errorf("owner reference = %+v, want it to match %+v", ref, owner)
	}
}

func TestWriteImageLayout_UpdatesExisting(t *testing.T) {
	owner := ImageOwnerRef{Name: "golden", UID: types.UID("abc-123"), APIVersion: "kezio.kojuro.date/v1alpha1", Kind: "Image"}
	c := newLayoutTestClient(t)
	if err := WriteImageLayout(context.Background(), c, "default", owner, `{"first":true}`); err != nil {
		t.Fatalf("first WriteImageLayout: %v", err)
	}

	if err := WriteImageLayout(context.Background(), c, "default", owner, `{"second":true}`); err != nil {
		t.Fatalf("second WriteImageLayout: %v", err)
	}

	layout := &keziov1alpha1.ImageLayout{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "golden-layout"}, layout); err != nil {
		t.Fatalf("get imagelayout: %v", err)
	}
	if layout.Spec.SfdiskJSON != `{"second":true}` {
		t.Errorf("layout.Spec.SfdiskJSON = %q, want the second write to win", layout.Spec.SfdiskJSON)
	}
	if len(layout.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references after update, want 1", len(layout.OwnerReferences))
	}
}
