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

package utils

import (
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func TestSmallestSeedablePartition(t *testing.T) {
	t.Run("picks the smallest UsedBytes among partitions with an infoHash", func(t *testing.T) {
		partitions := []keziov1alpha1.ImagePartitionStatus{
			{Number: 1, Role: keziov1alpha1.PartitionRoleESP, InfoHash: "a", UsedBytes: 100},
			{Number: 2, Role: keziov1alpha1.PartitionRoleData, InfoHash: "b", UsedBytes: 5000},
			{Number: 3, Role: keziov1alpha1.PartitionRoleSwap, UsedBytes: 1}, // no infoHash: not a candidate
		}

		got, err := SmallestSeedablePartition(partitions)
		if err != nil {
			t.Fatalf("SmallestSeedablePartition() error = %v", err)
		}
		if got.InfoHash != "a" {
			t.Errorf("SmallestSeedablePartition() = partition %d (hash %q), want the ESP (hash %q)",
				got.Number, got.InfoHash, "a")
		}
	})

	t.Run("errors when no partition has an infoHash", func(t *testing.T) {
		partitions := []keziov1alpha1.ImagePartitionStatus{
			{Number: 1, Role: keziov1alpha1.PartitionRoleSwap, UUID: "some-uuid"},
		}
		if _, err := SmallestSeedablePartition(partitions); err == nil {
			t.Error("SmallestSeedablePartition() error = nil, want an error")
		}
	})

	t.Run("errors on an empty list", func(t *testing.T) {
		if _, err := SmallestSeedablePartition(nil); err == nil {
			t.Error("SmallestSeedablePartition() error = nil, want an error")
		}
	})
}

func TestParseSHA256Sum(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name: "typical sha256sum output",
			output: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" +
				"  /leech/00000000000000000000000000100000\n",
			want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name: "tab separator",
			output: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" +
				"\t/store/contents/x/00000000000000000000000000100000",
			want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
		{
			name:    "not a digest",
			output:  "sha256sum: /leech/x: No such file or directory\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSHA256Sum(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSHA256Sum(%q) error = %v, wantErr %v", tt.output, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseSHA256Sum(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}
