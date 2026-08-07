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

// Package diskmatch resolves a Machine's target-disk hints against the
// agent-reported hardware inventory. It is pure logic: no Kubernetes
// client, no envtest dependency, so the matching and distinct-disk rules
// are unit-testable on their own.
package diskmatch

import (
	"fmt"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// giB is one gigabyte in bytes, used to compare MinSizeGigabytes and
// MaxSizeGigabytes hints against a disk's SizeBytes.
const giB = int64(1) << 30

// maxListedDisks bounds how many device names an ambiguous-match error
// message lists, so a machine with many disks does not produce an
// unbounded error string.
const maxListedDisks = 5

// Match resolves hints against disks: logical AND over set fields, unset
// fields unconstrained. deviceName/serialNumber/wwn/pciePath/hctl compare
// byte-for-byte (machine-generated identifiers; whitespace should not
// widen a match); model/vendor compare after trimming whitespace
// (firmware-padded free text). All comparisons are case-sensitive.
//
// Nil/all-unset hints falls back to "use the only disk reported": exactly
// one disk resolves, zero is an error, more than one requires a hint.
// Otherwise exactly one disk must satisfy the hints; zero or 2+ matches
// are errors.
func Match(disks []keziov1alpha1.MachineHardwareDisk, hints *keziov1alpha1.TargetDiskHints) (*keziov1alpha1.MachineHardwareDisk, error) {
	if !hasAnyHint(hints) {
		switch len(disks) {
		case 0:
			return nil, fmt.Errorf("no disk matches the hints")
		case 1:
			d := disks[0]
			return &d, nil
		default:
			return nil, fmt.Errorf("multiple disks present; a targetDisk hint is required")
		}
	}

	var matches []keziov1alpha1.MachineHardwareDisk
	for _, d := range disks {
		if hintsMatch(d, hints) {
			matches = append(matches, d)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no disk matches the hints")
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%d disks match the hints; refine them: %s", len(matches), describeDisks(matches))
	}
}

// hasAnyHint reports whether at least one field of hints is set.
func hasAnyHint(h *keziov1alpha1.TargetDiskHints) bool {
	if h == nil {
		return false
	}
	return h.DeviceName != "" ||
		h.SerialNumber != "" ||
		h.WWN != "" ||
		h.Model != "" ||
		h.Vendor != "" ||
		h.MinSizeGigabytes != nil ||
		h.MaxSizeGigabytes != nil ||
		h.Rotational != nil ||
		h.PciePath != "" ||
		h.HCTL != "" ||
		h.SlotNumber != nil
}

// hintsMatch reports whether d satisfies every set field of h.
func hintsMatch(d keziov1alpha1.MachineHardwareDisk, h *keziov1alpha1.TargetDiskHints) bool {
	if h.DeviceName != "" && d.DeviceName != h.DeviceName {
		return false
	}
	if h.SerialNumber != "" && d.SerialNumber != h.SerialNumber {
		return false
	}
	if h.WWN != "" && d.WWN != h.WWN {
		return false
	}
	if h.Model != "" && strings.TrimSpace(d.Model) != strings.TrimSpace(h.Model) {
		return false
	}
	if h.Vendor != "" && strings.TrimSpace(d.Vendor) != strings.TrimSpace(h.Vendor) {
		return false
	}
	if h.MinSizeGigabytes != nil && d.SizeBytes < *h.MinSizeGigabytes*giB {
		return false
	}
	if h.MaxSizeGigabytes != nil && d.SizeBytes > *h.MaxSizeGigabytes*giB {
		return false
	}
	if h.Rotational != nil && (d.Rotational == nil || *d.Rotational != *h.Rotational) {
		return false
	}
	if h.PciePath != "" && d.PciePath != h.PciePath {
		return false
	}
	if h.HCTL != "" && d.HCTL != h.HCTL {
		return false
	}
	if h.SlotNumber != nil && (d.SlotNumber == nil || *d.SlotNumber != *h.SlotNumber) {
		return false
	}
	return true
}

// describeDisks renders a bounded, human-readable list of disk device
// names for an ambiguous-match error message.
func describeDisks(disks []keziov1alpha1.MachineHardwareDisk) string {
	names := make([]string, 0, len(disks))
	for i, d := range disks {
		if i >= maxListedDisks {
			names = append(names, fmt.Sprintf("and %d more", len(disks)-maxListedDisks))
			break
		}
		name := d.DeviceName
		if name == "" {
			name = "(unnamed device)"
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// Selection names one resolved disk within a Machine, for CheckDistinct's
// conflict messages: for example "OS image" or "dataImages[0]".
type Selection struct {
	Label string
	Disk  *keziov1alpha1.MachineHardwareDisk
}

// CheckDistinct reports an error when two or more selections resolved to
// the same physical disk. Disk identity is the serial number when the
// disk reports one, since a serial number is stable across boots and
// uniquely names the physical device; a disk with no reported serial
// falls back to its device name. A nil Disk in a selection is ignored
// (the caller had nothing to resolve for that slot).
func CheckDistinct(selections []Selection) error {
	firstLabel := make(map[string]string, len(selections))
	for _, s := range selections {
		if s.Disk == nil {
			continue
		}
		id := diskIdentity(*s.Disk)
		if prev, ok := firstLabel[id]; ok {
			return fmt.Errorf("target disk conflict: %s and %s both resolve to disk %s",
				prev, s.Label, describeIdentity(*s.Disk))
		}
		firstLabel[id] = s.Label
	}
	return nil
}

// diskIdentity returns the stable key CheckDistinct groups selections by:
// the disk's serial number when reported, otherwise its device name.
func diskIdentity(d keziov1alpha1.MachineHardwareDisk) string {
	if d.SerialNumber != "" {
		return d.SerialNumber
	}
	return d.DeviceName
}

// describeIdentity renders a disk for a conflict error message.
func describeIdentity(d keziov1alpha1.MachineHardwareDisk) string {
	if d.SerialNumber != "" {
		return fmt.Sprintf("%s (serial %s)", d.DeviceName, d.SerialNumber)
	}
	return d.DeviceName
}
