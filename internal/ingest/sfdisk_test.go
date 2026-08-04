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

import "testing"

func TestParseSfdiskJSON_GPT(t *testing.T) {
	disk, err := ParseSfdiskJSON([]byte(fixtureSfdiskJSON))
	if err != nil {
		t.Fatalf("ParseSfdiskJSON: %v", err)
	}
	if disk.PartitionTable != "gpt" {
		t.Errorf("PartitionTable = %q, want gpt", disk.PartitionTable)
	}
	if disk.DiskGUID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("DiskGUID = %q", disk.DiskGUID)
	}
	if disk.SectorSize != 512 {
		t.Errorf("SectorSize = %d, want 512", disk.SectorSize)
	}
	if len(disk.Partitions) != 3 {
		t.Fatalf("got %d partitions, want 3", len(disk.Partitions))
	}
	p1 := disk.Partitions[0]
	if p1.Number != 1 || p1.StartBytes != 2048*512 || p1.SizeBytes != 2048*512 {
		t.Errorf("partition 1 = %+v", p1)
	}
	if p1.TypeGUID != "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" {
		t.Errorf("partition 1 TypeGUID = %q (want lowercased)", p1.TypeGUID)
	}
	if p1.PartUUID != "AAAAAAAA-1111-1111-1111-111111111111" {
		t.Errorf("partition 1 PartUUID = %q", p1.PartUUID)
	}
}

func TestParseSfdiskJSON_MBR(t *testing.T) {
	const dosJSON = `{
	  "partitiontable": {
	    "label": "dos",
	    "id": "0xabcdef12",
	    "device": "/dev/loop0",
	    "unit": "sectors",
	    "partitions": [
	      {"node": "/dev/loop0p1", "start": 2048, "size": 204800, "type": "83"}
	    ]
	  }
	}`

	disk, err := ParseSfdiskJSON([]byte(dosJSON))
	if err != nil {
		t.Fatalf("ParseSfdiskJSON: %v", err)
	}
	if disk.PartitionTable != "mbr" {
		t.Errorf("PartitionTable = %q, want mbr", disk.PartitionTable)
	}
	if disk.DiskGUID != "" {
		t.Errorf("DiskGUID = %q, want empty for mbr", disk.DiskGUID)
	}
	// sfdisk omitted "sectorsize"; the default must be assumed.
	if disk.SectorSize != defaultSectorSize {
		t.Errorf("SectorSize = %d, want default %d", disk.SectorSize, defaultSectorSize)
	}
	if len(disk.Partitions) != 1 || disk.Partitions[0].Number != 1 {
		t.Fatalf("partitions = %+v", disk.Partitions)
	}
}

func TestParseSfdiskJSON_UnsupportedLabel(t *testing.T) {
	const badJSON = `{"partitiontable": {"label": "sun", "partitions": []}}`
	if _, err := ParseSfdiskJSON([]byte(badJSON)); err == nil {
		t.Fatal("expected an error for an unsupported partition table label")
	}
}

func TestParseSfdiskJSON_InvalidJSON(t *testing.T) {
	if _, err := ParseSfdiskJSON([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestPartitionNumber_FallsBackToListPosition(t *testing.T) {
	if got := partitionNumber("/dev/loop0-no-digits", 4); got != 5 {
		t.Errorf("partitionNumber = %d, want 5 (1-based list position)", got)
	}
	if got := partitionNumber("/dev/loop0p12", 0); got != 12 {
		t.Errorf("partitionNumber = %d, want 12", got)
	}
}
