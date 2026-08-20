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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestPartitionContentPVCSize(t *testing.T) {
	cases := []struct {
		name      string
		sizeBytes int64
		want      resource.Quantity
	}{
		{
			name:      "typical size gets sizeBytes plus fixed headroom",
			sizeBytes: 100 * 1024 * 1024,
			want:      *resource.NewQuantity(100*1024*1024+partitionContentPVCHeadroomBytes, resource.BinarySI),
		},
		{
			name:      "zero size gets exactly the headroom",
			sizeBytes: 0,
			want:      *resource.NewQuantity(partitionContentPVCHeadroomBytes, resource.BinarySI),
		},
		{
			name:      "tiny size still gets the headroom, rounded up by one byte",
			sizeBytes: 1,
			want:      *resource.NewQuantity(partitionContentPVCHeadroomBytes+1, resource.BinarySI),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionContentPVCSize(tc.sizeBytes)
			if got.Cmp(tc.want) != 0 {
				t.Errorf("partitionContentPVCSize(%d) = %s, want %s", tc.sizeBytes, got.String(), tc.want.String())
			}
		})
	}
}

func TestBuildContentPVC(t *testing.T) {
	pc := &keziov1alpha2.PartitionContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pc-test", Namespace: "default"},
		Spec:       keziov1alpha2.PartitionContentSpec{SizeBytes: 1024},
	}

	cases := []struct {
		name           string
		publish        PartitionContentPublishConfig
		wantClass      *string
		wantAccessMode []corev1.PersistentVolumeAccessMode
	}{
		{
			name:           "unset config defaults to no storage class and ReadWriteMany",
			publish:        PartitionContentPublishConfig{},
			wantClass:      nil,
			wantAccessMode: defaultPartitionContentAccessModes,
		},
		{
			name:           "configured storage class is carried onto the PVC",
			publish:        PartitionContentPublishConfig{StorageClassName: "fast-rwx"},
			wantClass:      strPtr("fast-rwx"),
			wantAccessMode: defaultPartitionContentAccessModes,
		},
		{
			name:           "configured access modes are carried onto the PVC exactly, not merged with the default",
			publish:        PartitionContentPublishConfig{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
			wantClass:      nil,
			wantAccessMode: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &PartitionContentReconciler{Publish: tc.publish}
			pvc := r.buildContentPVC(pc, "content-pc-test")

			if tc.wantClass == nil {
				if pvc.Spec.StorageClassName != nil {
					t.Errorf("StorageClassName = %q, want unset", *pvc.Spec.StorageClassName)
				}
			} else {
				if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != *tc.wantClass {
					t.Errorf("StorageClassName = %v, want %q", pvc.Spec.StorageClassName, *tc.wantClass)
				}
			}

			if len(pvc.Spec.AccessModes) != len(tc.wantAccessMode) {
				t.Fatalf("AccessModes = %v, want %v", pvc.Spec.AccessModes, tc.wantAccessMode)
			}
			for i, m := range tc.wantAccessMode {
				if pvc.Spec.AccessModes[i] != m {
					t.Errorf("AccessModes = %v, want %v", pvc.Spec.AccessModes, tc.wantAccessMode)
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }
