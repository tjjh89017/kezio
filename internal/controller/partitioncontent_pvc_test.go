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

	"k8s.io/apimachinery/pkg/api/resource"
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
