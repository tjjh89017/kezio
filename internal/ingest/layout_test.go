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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLayoutConfigMapName(t *testing.T) {
	if got, want := LayoutConfigMapName("golden"), "golden-layout"; got != want {
		t.Errorf("LayoutConfigMapName(golden) = %q, want %q", got, want)
	}
}

func TestWriteLayoutConfigMap_CreatesWithOwnerRef(t *testing.T) {
	client := fake.NewClientset()
	owner := ImageOwnerRef{Name: "golden", UID: types.UID("abc-123"), APIVersion: "kezio.kojuro.date/v1alpha1", Kind: "Image"}

	if err := WriteLayoutConfigMap(context.Background(), client, "default", owner, `{"partitiontable":{}}`); err != nil {
		t.Fatalf("WriteLayoutConfigMap: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "golden-layout", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data[LayoutConfigMapDataKey] != `{"partitiontable":{}}` {
		t.Errorf("cm.Data[%q] = %q, want the sfdisk JSON", LayoutConfigMapDataKey, cm.Data[LayoutConfigMapDataKey])
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want 1", len(cm.OwnerReferences))
	}
	ref := cm.OwnerReferences[0]
	if ref.Name != owner.Name || ref.UID != owner.UID || ref.APIVersion != owner.APIVersion || ref.Kind != owner.Kind {
		t.Errorf("owner reference = %+v, want it to match %+v", ref, owner)
	}
}

func TestWriteLayoutConfigMap_UpdatesExisting(t *testing.T) {
	owner := ImageOwnerRef{Name: "golden", UID: types.UID("abc-123"), APIVersion: "kezio.kojuro.date/v1alpha1", Kind: "Image"}
	client := fake.NewClientset()
	if err := WriteLayoutConfigMap(context.Background(), client, "default", owner, `{"first":true}`); err != nil {
		t.Fatalf("first WriteLayoutConfigMap: %v", err)
	}

	if err := WriteLayoutConfigMap(context.Background(), client, "default", owner, `{"second":true}`); err != nil {
		t.Fatalf("second WriteLayoutConfigMap: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "golden-layout", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data[LayoutConfigMapDataKey] != `{"second":true}` {
		t.Errorf("cm.Data[%q] = %q, want the second write to win", LayoutConfigMapDataKey, cm.Data[LayoutConfigMapDataKey])
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references after update, want 1", len(cm.OwnerReferences))
	}
}
