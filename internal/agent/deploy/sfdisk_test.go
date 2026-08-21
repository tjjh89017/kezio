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

package deploy

import (
	"strings"
	"testing"
)

const fixtureSfdiskJSON = `{
  "partitiontable": {
    "label": "gpt",
    "id": "11111111-1111-1111-1111-111111111111",
    "device": "/dev/loop0",
    "unit": "sectors",
    "sectorsize": 512,
    "partitions": [
      {"node": "/dev/loop0p1", "start": 2048, "size": 2048, "type": "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", "uuid": "AAAAAAAA-1111-1111-1111-111111111111", "name": "EFI System"},
      {"node": "/dev/loop0p2", "start": 4096, "size": 2048, "type": "0FC63DAF-8483-4772-8E79-3D69D8477DE4", "uuid": "BBBBBBBB-2222-2222-2222-222222222222"},
      {"node": "/dev/loop0p3", "start": 6144, "size": 2048, "type": "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F", "uuid": "CCCCCCCC-3333-3333-3333-333333333333"}
    ]
  }
}`

func TestSfdiskScript_GPT(t *testing.T) {
	script, err := SfdiskScript(fixtureSfdiskJSON, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("SfdiskScript: %v", err)
	}

	wantLines := []string{
		"label: gpt",
		"label-id: 11111111-1111-1111-1111-111111111111",
		"unit: sectors",
		"sector-size: 512",
		`/dev/nvme0n1p1 : start=2048, size=2048, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=AAAAAAAA-1111-1111-1111-111111111111, name="EFI System"`,
		"/dev/nvme0n1p2 : start=4096, size=2048, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=BBBBBBBB-2222-2222-2222-222222222222",
		"/dev/nvme0n1p3 : start=6144, size=2048, type=0657FD6D-A4AB-43C4-84E5-0933C84B4F4F, uuid=CCCCCCCC-3333-3333-3333-333333333333",
	}
	for _, want := range wantLines {
		if !strings.Contains(script, want) {
			t.Errorf("script missing line %q; got:\n%s", want, script)
		}
	}
}

func TestSfdiskScript_DeviceUsesTargetDiskNamingConvention(t *testing.T) {
	// "sda" has no trailing digit, so its partitions do not get a "p"
	// infix - the same convention agentapi.DevicePartitionPath applies,
	// which the controller already used to compute PlanPartition.Device.
	script, err := SfdiskScript(fixtureSfdiskJSON, "/dev/sda")
	if err != nil {
		t.Fatalf("SfdiskScript: %v", err)
	}
	if !strings.Contains(script, "/dev/sda1 : start=2048") {
		t.Errorf("script does not address partition 1 as /dev/sda1; got:\n%s", script)
	}
}

func TestSfdiskScript_MBR(t *testing.T) {
	const dosJSON = `{
	  "partitiontable": {
	    "label": "dos",
	    "id": "0xabcdef12",
	    "device": "/dev/loop0",
	    "unit": "sectors",
	    "partitions": [
	      {"node": "/dev/loop0p1", "start": 2048, "size": 204800, "type": "83", "bootable": true}
	    ]
	  }
	}`
	script, err := SfdiskScript(dosJSON, "/dev/sda")
	if err != nil {
		t.Fatalf("SfdiskScript: %v", err)
	}
	if !strings.Contains(script, "label: dos") || !strings.Contains(script, "label-id: 0xabcdef12") {
		t.Errorf("script missing dos header lines; got:\n%s", script)
	}
	if !strings.Contains(script, "/dev/sda1 : start=2048, size=204800, type=83, bootable") {
		t.Errorf("script missing MBR partition line; got:\n%s", script)
	}
}

func TestSfdiskScript_RejectsMissingLabel(t *testing.T) {
	if _, err := SfdiskScript(`{"partitiontable":{"partitions":[]}}`, "/dev/sda"); err == nil {
		t.Fatal("SfdiskScript: got nil error for a dump with no label, want an error")
	}
}

func TestSfdiskScript_RejectsMalformedJSON(t *testing.T) {
	if _, err := SfdiskScript("not json", "/dev/sda"); err == nil {
		t.Fatal("SfdiskScript: got nil error for malformed JSON, want an error")
	}
}

func TestMinimumDiskBytes(t *testing.T) {
	got, err := MinimumDiskBytes(fixtureSfdiskJSON)
	if err != nil {
		t.Fatalf("MinimumDiskBytes: %v", err)
	}
	// The furthest-out partition ends at (6144+2048) sectors * 512 bytes.
	want := int64(6144+2048) * 512
	if got != want {
		t.Errorf("MinimumDiskBytes = %d, want %d", got, want)
	}
}

func TestMinimumDiskBytes_DefaultsSectorSize(t *testing.T) {
	const noSectorSize = `{"partitiontable":{"label":"gpt","partitions":[{"node":"/dev/loop0p1","start":10,"size":10}]}}`
	got, err := MinimumDiskBytes(noSectorSize)
	if err != nil {
		t.Fatalf("MinimumDiskBytes: %v", err)
	}
	if want := int64(20 * 512); got != want {
		t.Errorf("MinimumDiskBytes = %d, want %d (default 512-byte sectors)", got, want)
	}
}

func TestDevicePartitionPath(t *testing.T) {
	cases := []struct {
		disk   string
		number int32
		want   string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/vda", 2, "/dev/vda2"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/mmcblk0", 1, "/dev/mmcblk0p1"},
		{"", 1, ""},
	}
	for _, c := range cases {
		if got := DevicePartitionPath(c.disk, c.number); got != c.want {
			t.Errorf("DevicePartitionPath(%q, %d) = %q, want %q", c.disk, c.number, got, c.want)
		}
	}
}
