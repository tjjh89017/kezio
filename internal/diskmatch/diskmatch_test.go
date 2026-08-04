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

package diskmatch

import (
	"strings"
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
func int32Ptr(i int32) *int32 { return &i }

func nvme() keziov1alpha1.MachineHardwareDisk {
	return keziov1alpha1.MachineHardwareDisk{
		DeviceName:   "/dev/nvme0n1",
		SerialNumber: "S6XANJ0T123456",
		WWN:          "0x5002538e00000001",
		Model:        "Samsung SSD 990 PRO",
		Vendor:       "SAMSUNG",
		SizeBytes:    1000 * giB,
		Rotational:   boolPtr(false),
		PciePath:     "0000:01:00.0",
		HCTL:         "0:0:0:0",
		SlotNumber:   int32Ptr(1),
	}
}

func hdd() keziov1alpha1.MachineHardwareDisk {
	return keziov1alpha1.MachineHardwareDisk{
		DeviceName:   "/dev/sda",
		SerialNumber: "Z1D2AB3C",
		WWN:          "0x5000c500e0000002",
		Model:        "  Seagate Barracuda  ",
		Vendor:       " Seagate ",
		SizeBytes:    4000 * giB,
		Rotational:   boolPtr(true),
		PciePath:     "0000:02:00.0",
		HCTL:         "1:0:0:0",
		SlotNumber:   int32Ptr(2),
	}
}

func TestMatch_SingleHintFields(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme(), hdd()}

	cases := []struct {
		name    string
		hints   *keziov1alpha1.TargetDiskHints
		wantDev string
	}{
		{"deviceName", &keziov1alpha1.TargetDiskHints{DeviceName: "/dev/sda"}, "/dev/sda"},
		{"serialNumber", &keziov1alpha1.TargetDiskHints{SerialNumber: "S6XANJ0T123456"}, "/dev/nvme0n1"},
		{"wwn", &keziov1alpha1.TargetDiskHints{WWN: "0x5000c500e0000002"}, "/dev/sda"},
		{"model exact", &keziov1alpha1.TargetDiskHints{Model: "Samsung SSD 990 PRO"}, "/dev/nvme0n1"},
		{"model trimmed", &keziov1alpha1.TargetDiskHints{Model: "Seagate Barracuda"}, "/dev/sda"},
		{"vendor exact", &keziov1alpha1.TargetDiskHints{Vendor: "SAMSUNG"}, "/dev/nvme0n1"},
		{"vendor trimmed", &keziov1alpha1.TargetDiskHints{Vendor: "Seagate"}, "/dev/sda"},
		{"minSize excludes smaller", &keziov1alpha1.TargetDiskHints{MinSizeGigabytes: int64Ptr(2000)}, "/dev/sda"},
		{"maxSize excludes larger", &keziov1alpha1.TargetDiskHints{MaxSizeGigabytes: int64Ptr(2000)}, "/dev/nvme0n1"},
		{"rotational true", &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(true)}, "/dev/sda"},
		{"rotational false", &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(false)}, "/dev/nvme0n1"},
		{"pciePath", &keziov1alpha1.TargetDiskHints{PciePath: "0000:02:00.0"}, "/dev/sda"},
		{"hctl", &keziov1alpha1.TargetDiskHints{HCTL: "1:0:0:0"}, "/dev/sda"},
		{"slotNumber", &keziov1alpha1.TargetDiskHints{SlotNumber: int32Ptr(1)}, "/dev/nvme0n1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Match(disks, tc.hints)
			if err != nil {
				t.Fatalf("Match() error = %v, want nil", err)
			}
			if got.DeviceName != tc.wantDev {
				t.Fatalf("Match() device = %q, want %q", got.DeviceName, tc.wantDev)
			}
		})
	}
}

func TestMatch_ExactMatchIsCaseSensitive(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme()}

	_, err := Match(disks, &keziov1alpha1.TargetDiskHints{SerialNumber: "s6xanj0t123456"})
	if err == nil {
		t.Fatalf("Match() with wrong-case serial number should not match, got a disk")
	}
}

func TestMatch_ANDSemantics(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme(), hdd()}

	// Two fields that individually would each match a different disk in
	// isolation must combine with AND: only a disk matching both wins.
	hints := &keziov1alpha1.TargetDiskHints{
		Vendor:     "SAMSUNG",
		Rotational: boolPtr(false),
	}
	got, err := Match(disks, hints)
	if err != nil {
		t.Fatalf("Match() error = %v, want nil", err)
	}
	if got.DeviceName != "/dev/nvme0n1" {
		t.Fatalf("Match() device = %q, want /dev/nvme0n1", got.DeviceName)
	}

	// A combination no disk satisfies (SAMSUNG vendor + rotational) is
	// zero matches, not a partial match.
	contradiction := &keziov1alpha1.TargetDiskHints{
		Vendor:     "SAMSUNG",
		Rotational: boolPtr(true),
	}
	if _, err := Match(disks, contradiction); err == nil {
		t.Fatalf("Match() with a contradictory AND combination should error")
	}
}

func TestMatch_ZeroMatches(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme(), hdd()}

	_, err := Match(disks, &keziov1alpha1.TargetDiskHints{SerialNumber: "no-such-serial"})
	if err == nil {
		t.Fatalf("Match() expected an error for zero matches")
	}
	if !strings.Contains(err.Error(), "no disk matches the hints") {
		t.Fatalf("Match() error = %q, want it to mention no disk matches", err.Error())
	}
}

func TestMatch_ManyMatches(t *testing.T) {
	a := nvme()
	b := nvme()
	b.DeviceName = "/dev/nvme1n1"
	b.SerialNumber = "S6XANJ0T999999"
	disks := []keziov1alpha1.MachineHardwareDisk{a, b}

	// Both disks share the same rotational value and vendor is unset, so
	// the hint alone does not disambiguate them.
	_, err := Match(disks, &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(false)})
	if err == nil {
		t.Fatalf("Match() expected an error for two matches")
	}
	if !strings.Contains(err.Error(), "2 disks match the hints") {
		t.Fatalf("Match() error = %q, want it to mention 2 disks match", err.Error())
	}
	if !strings.Contains(err.Error(), "/dev/nvme0n1") || !strings.Contains(err.Error(), "/dev/nvme1n1") {
		t.Fatalf("Match() error = %q, want it to list the ambiguous devices", err.Error())
	}
}

func TestMatch_ManyMatchesMessageIsBounded(t *testing.T) {
	var disks []keziov1alpha1.MachineHardwareDisk
	for i := 0; i < 8; i++ {
		d := nvme()
		d.DeviceName = "/dev/nvme" + string(rune('0'+i)) + "n1"
		d.SerialNumber = ""
		disks = append(disks, d)
	}

	_, err := Match(disks, &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(false)})
	if err == nil {
		t.Fatalf("Match() expected an error for many matches")
	}
	if !strings.Contains(err.Error(), "and 3 more") {
		t.Fatalf("Match() error = %q, want the listing bounded with an '...and N more' tail", err.Error())
	}
}

func TestMatch_NoHintSingleDisk(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme()}

	got, err := Match(disks, nil)
	if err != nil {
		t.Fatalf("Match() error = %v, want nil", err)
	}
	if got.DeviceName != "/dev/nvme0n1" {
		t.Fatalf("Match() device = %q, want /dev/nvme0n1", got.DeviceName)
	}

	// An empty (non-nil) hints struct behaves the same as nil.
	got, err = Match(disks, &keziov1alpha1.TargetDiskHints{})
	if err != nil {
		t.Fatalf("Match() error = %v, want nil", err)
	}
	if got.DeviceName != "/dev/nvme0n1" {
		t.Fatalf("Match() device = %q, want /dev/nvme0n1", got.DeviceName)
	}
}

func TestMatch_NoHintMultipleDisks(t *testing.T) {
	disks := []keziov1alpha1.MachineHardwareDisk{nvme(), hdd()}

	_, err := Match(disks, nil)
	if err == nil {
		t.Fatalf("Match() expected an error when multiple disks and no hint")
	}
	if !strings.Contains(err.Error(), "targetDisk hint is required") {
		t.Fatalf("Match() error = %q, want it to ask for a hint", err.Error())
	}
}

func TestMatch_NoHintNoDisks(t *testing.T) {
	_, err := Match(nil, nil)
	if err == nil {
		t.Fatalf("Match() expected an error when there are no disks")
	}
}

func TestMatch_SizeRangeBoundaries(t *testing.T) {
	disk := keziov1alpha1.MachineHardwareDisk{
		DeviceName: "/dev/nvme0n1",
		SizeBytes:  500 * giB,
	}
	disks := []keziov1alpha1.MachineHardwareDisk{disk}

	cases := []struct {
		name      string
		hints     *keziov1alpha1.TargetDiskHints
		wantMatch bool
	}{
		{"min at exact boundary matches", &keziov1alpha1.TargetDiskHints{MinSizeGigabytes: int64Ptr(500)}, true},
		{"min one GiB over boundary excludes", &keziov1alpha1.TargetDiskHints{MinSizeGigabytes: int64Ptr(501)}, false},
		{"max at exact boundary matches", &keziov1alpha1.TargetDiskHints{MaxSizeGigabytes: int64Ptr(500)}, true},
		{"max one GiB under boundary excludes", &keziov1alpha1.TargetDiskHints{MaxSizeGigabytes: int64Ptr(499)}, false},
		{"in range matches", &keziov1alpha1.TargetDiskHints{MinSizeGigabytes: int64Ptr(400), MaxSizeGigabytes: int64Ptr(600)}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Match(disks, tc.hints)
			if tc.wantMatch && err != nil {
				t.Fatalf("Match() error = %v, want a match", err)
			}
			if !tc.wantMatch && err == nil {
				t.Fatalf("Match() expected no match, got one")
			}
		})
	}
}

func TestMatch_UnsetRotationalDiskDoesNotMatchEitherValue(t *testing.T) {
	disk := keziov1alpha1.MachineHardwareDisk{
		DeviceName: "/dev/nvme0n1",
		// Rotational left nil: the agent did not report it.
	}
	disks := []keziov1alpha1.MachineHardwareDisk{disk}

	if _, err := Match(disks, &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(false)}); err == nil {
		t.Fatalf("Match() expected no match against an unreported rotational field")
	}
	if _, err := Match(disks, &keziov1alpha1.TargetDiskHints{Rotational: boolPtr(true)}); err == nil {
		t.Fatalf("Match() expected no match against an unreported rotational field")
	}
}

func TestCheckDistinct_NoConflict(t *testing.T) {
	d1 := nvme()
	d2 := hdd()

	err := CheckDistinct([]Selection{
		{Label: "OS image", Disk: &d1},
		{Label: "dataImages[0]", Disk: &d2},
	})
	if err != nil {
		t.Fatalf("CheckDistinct() error = %v, want nil", err)
	}
}

func TestCheckDistinct_ConflictBySerial(t *testing.T) {
	d1 := nvme()
	d2 := nvme() // same serial number, same physical disk

	err := CheckDistinct([]Selection{
		{Label: "OS image", Disk: &d1},
		{Label: "dataImages[0]", Disk: &d2},
	})
	if err == nil {
		t.Fatalf("CheckDistinct() expected a conflict error")
	}
	if !strings.Contains(err.Error(), "OS image") || !strings.Contains(err.Error(), "dataImages[0]") {
		t.Fatalf("CheckDistinct() error = %q, want it to name both selections", err.Error())
	}
}

func TestCheckDistinct_ConflictByDeviceNameWhenNoSerial(t *testing.T) {
	d1 := keziov1alpha1.MachineHardwareDisk{DeviceName: "/dev/sdb"}
	d2 := keziov1alpha1.MachineHardwareDisk{DeviceName: "/dev/sdb"}

	err := CheckDistinct([]Selection{
		{Label: "OS image", Disk: &d1},
		{Label: "dataImages[0]", Disk: &d2},
	})
	if err == nil {
		t.Fatalf("CheckDistinct() expected a conflict error for disks with no serial sharing a device name")
	}
}

func TestCheckDistinct_ThreeWayConflictReportsFirstPair(t *testing.T) {
	d1 := nvme()
	d2 := nvme()
	d3 := nvme()

	err := CheckDistinct([]Selection{
		{Label: "OS image", Disk: &d1},
		{Label: "dataImages[0]", Disk: &d2},
		{Label: "dataImages[1]", Disk: &d3},
	})
	if err == nil {
		t.Fatalf("CheckDistinct() expected a conflict error")
	}
	if !strings.Contains(err.Error(), "OS image") || !strings.Contains(err.Error(), "dataImages[0]") {
		t.Fatalf("CheckDistinct() error = %q, want it to name the first conflicting pair", err.Error())
	}
}

func TestCheckDistinct_NilSelectionIgnored(t *testing.T) {
	d1 := nvme()

	err := CheckDistinct([]Selection{
		{Label: "OS image", Disk: &d1},
		{Label: "dataImages[0]", Disk: nil},
	})
	if err != nil {
		t.Fatalf("CheckDistinct() error = %v, want nil when one selection has no disk", err)
	}
}
