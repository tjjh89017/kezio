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
	"errors"
	"testing"
)

func TestResultRoundTrip(t *testing.T) {
	want := Result{
		Success: true,
		Disk: &ResultDisk{
			SizeBytes:      123456,
			PartitionTable: "gpt",
			Partitions: []ResultPartition{
				{Number: 1, Role: "esp", FSType: "vfat", UsedBytes: 100, SizeBytes: 1048576, LastExtentEnd: 100, PieceLength: 16 << 20, InfoHash: "abc123"},
				{Number: 2, Role: "swap", UUID: "some-uuid"},
			},
		},
	}

	data, err := MarshalResult(want)
	if err != nil {
		t.Fatalf("MarshalResult: %v", err)
	}
	if len(data) > 4096 {
		t.Errorf("marshaled result is %d bytes, exceeds the termination message cap of 4096", len(data))
	}

	got, err := UnmarshalResult(data)
	if err != nil {
		t.Fatalf("UnmarshalResult: %v", err)
	}
	if got.Success != want.Success || got.Disk.SizeBytes != want.Disk.SizeBytes ||
		len(got.Disk.Partitions) != len(want.Disk.Partitions) {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
	if got.Disk.Partitions[0].LastExtentEnd != 100 || got.Disk.Partitions[0].PieceLength != 16<<20 {
		t.Fatalf("round trip lost LastExtentEnd/PieceLength: got %+v", got.Disk.Partitions[0])
	}
}

func TestFailureResult(t *testing.T) {
	r := FailureResult(errors.New("boom"))
	if r.Success {
		t.Error("FailureResult should report Success = false")
	}
	if r.Error != "boom" {
		t.Errorf("Error = %q, want boom", r.Error)
	}
	if r.Disk != nil {
		t.Error("FailureResult should not populate Disk")
	}
}

func TestUnmarshalResult_Invalid(t *testing.T) {
	if _, err := UnmarshalResult([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}
