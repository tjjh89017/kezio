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
			SfdiskJSON:     `{"partitiontable":{"label":"gpt","partitions":[]}}`,
			Partitions: []ResultPartition{
				{Number: 1, Role: "esp", FSType: "vfat", UsedBytes: 100, SizeBytes: 1048576, LastExtentEnd: 100, PieceLength: 16 << 20, TypeGUID: "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"},
				{Number: 2, Role: "swap", UUID: "some-uuid"},
			},
		},
	}

	data, err := MarshalResult(want)
	if err != nil {
		t.Fatalf("MarshalResult: %v", err)
	}
	if len(data) > TerminationMessageLimit {
		t.Errorf("marshaled result is %d bytes, exceeds the termination message cap of %d", len(data), TerminationMessageLimit)
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
	if got.Disk.SfdiskJSON != want.Disk.SfdiskJSON {
		t.Fatalf("round trip lost SfdiskJSON: got %q", got.Disk.SfdiskJSON)
	}
}

func TestResultRoundTrip_Publish(t *testing.T) {
	want := Result{Success: true, Publish: &ResultPublish{InfoHash: "8f4e2b1c0d9a7f6e5b4c3d2a1908f7e6d5c4b3a2"}}

	data, err := MarshalResult(want)
	if err != nil {
		t.Fatalf("MarshalResult: %v", err)
	}
	got, err := UnmarshalResult(data)
	if err != nil {
		t.Fatalf("UnmarshalResult: %v", err)
	}
	if got.Publish == nil || got.Publish.InfoHash != want.Publish.InfoHash {
		t.Fatalf("round trip lost the publish info hash: got %+v", got.Publish)
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
