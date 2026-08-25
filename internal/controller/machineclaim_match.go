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
	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// machineClaimGiB is one gigabyte in bytes, matching internal/diskmatch's
// own conversion factor for MinSizeGigabytes-style hints.
const machineClaimGiB = int64(1) << 30

// hardwareMatchesSelector reports whether hw satisfies every field of sel:
// a floor on CPU count and memory, and a distinct-disk match for every
// entry in sel.Disks. A Machine with no MachineHardware at all never
// reaches this - the caller treats that as no match, per the claim
// layer's rule that a hardware selector never matches a machine that
// hasn't reported hardware.
func hardwareMatchesSelector(hw *keziov1alpha3.MachineHardwareSpec, sel *keziov1alpha3.ClaimHardwareSelector) bool {
	if sel.MinCPUCount != nil && hw.CPUCount < *sel.MinCPUCount {
		return false
	}
	if sel.MinMemoryBytes != nil && hw.MemoryBytes < *sel.MinMemoryBytes {
		return false
	}
	return disksSatisfy(hw.Disks, sel.Disks)
}

// disksSatisfy reports whether every entry in reqs matches a distinct
// disk in disks - a bipartite matching (Kuhn's algorithm), not a
// per-entry independent search: two requirements that both fit only the
// same one disk must not both be reported satisfied.
func disksSatisfy(disks []keziov1alpha3.MachineHardwareDisk, reqs []keziov1alpha3.ClaimHardwareDiskSelector) bool {
	if len(reqs) == 0 {
		return true
	}
	if len(reqs) > len(disks) {
		return false
	}

	adj := make([][]int, len(reqs))
	for i, req := range reqs {
		for j, disk := range disks {
			if diskMatchesRequirement(disk, req) {
				adj[i] = append(adj[i], j)
			}
		}
	}

	matchOfDisk := make([]int, len(disks))
	for i := range matchOfDisk {
		matchOfDisk[i] = -1
	}
	for i := range reqs {
		visited := make([]bool, len(disks))
		if !tryAugmentDiskMatch(i, adj, visited, matchOfDisk) {
			return false
		}
	}
	return true
}

// tryAugmentDiskMatch is Kuhn's algorithm's augmenting-path step: it
// tries to find req a disk, displacing whichever earlier requirement
// currently holds a candidate disk if that requirement has another
// option.
func tryAugmentDiskMatch(req int, adj [][]int, visited []bool, matchOfDisk []int) bool {
	for _, disk := range adj[req] {
		if visited[disk] {
			continue
		}
		visited[disk] = true
		if matchOfDisk[disk] == -1 || tryAugmentDiskMatch(matchOfDisk[disk], adj, visited, matchOfDisk) {
			matchOfDisk[disk] = req
			return true
		}
	}
	return false
}

// diskMatchesRequirement reports whether disk satisfies every set field
// of req. Model and vendor compare exactly, matching
// ClaimHardwareDiskSelector's own doc comment.
func diskMatchesRequirement(disk keziov1alpha3.MachineHardwareDisk, req keziov1alpha3.ClaimHardwareDiskSelector) bool {
	if req.Model != "" && req.Model != disk.Model {
		return false
	}
	if req.Vendor != "" && req.Vendor != disk.Vendor {
		return false
	}
	if req.MinSizeGigabytes != nil && disk.SizeBytes < *req.MinSizeGigabytes*machineClaimGiB {
		return false
	}
	if req.Rotational != nil && (disk.Rotational == nil || *disk.Rotational != *req.Rotational) {
		return false
	}
	return true
}
