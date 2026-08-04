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
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func TestClassifyRole(t *testing.T) {
	cases := []struct {
		name     string
		typeGUID string
		fsType   string
		want     string
	}{
		{"gpt esp", espTypeGUIDGPT, "vfat", keziov1alpha1.PartitionRoleESP},
		{"gpt esp uppercase input", "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", "vfat", keziov1alpha1.PartitionRoleESP},
		{"mbr esp", espTypeMBR, "vfat", keziov1alpha1.PartitionRoleESP},
		{"gpt msr", msrTypeGUIDGPT, "", keziov1alpha1.PartitionRoleMSR},
		{"linux swap by fstype", "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f", "swap", keziov1alpha1.PartitionRoleSwap},
		{"linux data", "0fc63daf-8483-4772-8e79-3d69d8477de4", "ext4", keziov1alpha1.PartitionRoleData},
		{"unknown fs falls back to data", "0fc63daf-8483-4772-8e79-3d69d8477de4", "", keziov1alpha1.PartitionRoleData},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRole(tc.typeGUID, tc.fsType); got != tc.want {
				t.Errorf("classifyRole(%q, %q) = %q, want %q", tc.typeGUID, tc.fsType, got, tc.want)
			}
		})
	}
}
