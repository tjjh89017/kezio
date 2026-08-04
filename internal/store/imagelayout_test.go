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

package store

import (
	"testing"
)

func TestWriteLoadImageLayoutRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := &ImageLayout{
		SchemaVersion:  ImageLayoutSchemaVersion,
		PartitionTable: "gpt",
		DiskGUID:       "11111111-1111-1111-1111-111111111111",
		SizeBytes:      1 << 30,
		SectorSize:     512,
		Slots: []ImageLayoutSlot{
			{
				Number:          1,
				StartBytes:      1048576,
				SizeBytes:       268435456,
				TypeGUID:        "c12a7328-f81f-11d2-ba4b-00a0c93ec93b",
				PartUUID:        "22222222-2222-2222-2222-222222222222",
				Role:            "esp",
				FSType:          "vfat",
				ContentInfoHash: "0102030405060708090a0b0c0d0e0f101112131",
			},
			{
				Number:          2,
				StartBytes:      269484032,
				SizeBytes:       9663676416,
				TypeGUID:        "0fc63daf-8483-4772-8e79-3d69d8477de4",
				PartUUID:        "33333333-3333-3333-3333-333333333333",
				Role:            "data",
				FSType:          "ext4",
				ContentInfoHash: "1112131415161718191a1b1c1d1e1f202122232",
			},
			{
				Number:     3,
				StartBytes: 9933160448,
				SizeBytes:  2147483648,
				TypeGUID:   "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f",
				PartUUID:   "44444444-4444-4444-4444-444444444444",
				Role:       "swap",
				SwapUUID:   "55555555-5555-5555-5555-555555555555",
			},
			{
				// A blank slot: no content, formatted fresh at deploy time.
				Number:     4,
				StartBytes: 12080644096,
				SizeBytes:  1073741824,
				TypeGUID:   "0fc63daf-8483-4772-8e79-3d69d8477de4",
				PartUUID:   "66666666-6666-6666-6666-666666666666",
				Role:       "data",
				FSType:     "ext4",
			},
		},
	}

	if err := WriteImageLayout(root, "golden", want); err != nil {
		t.Fatalf("WriteImageLayout: %v", err)
	}

	got, err := LoadImageLayout(root, "golden")
	if err != nil {
		t.Fatalf("LoadImageLayout: %v", err)
	}

	if got.PartitionTable != want.PartitionTable || got.DiskGUID != want.DiskGUID ||
		got.SizeBytes != want.SizeBytes || len(got.Slots) != len(want.Slots) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}
	for i := range want.Slots {
		if got.Slots[i] != want.Slots[i] {
			t.Fatalf("slot %d mismatch: got %+v, want %+v", i, got.Slots[i], want.Slots[i])
		}
	}

	if got.Slots[2].IsBlank() {
		t.Fatalf("swap slot should not be reported blank")
	}
	if !got.Slots[3].IsBlank() {
		t.Fatalf("slot with no content and no swap UUID should be reported blank")
	}
}

func TestLoadImageLayoutMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadImageLayout(root, "does-not-exist"); err == nil {
		t.Fatalf("expected an error loading a missing layout.json")
	}
}
