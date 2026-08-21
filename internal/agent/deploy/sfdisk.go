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
	"encoding/json"
	"fmt"
	"strings"
)

// DevicePartitionPath returns the kernel device path for partition number
// on disk, using the standard Linux partition-naming convention: a disk
// name ending in a digit (for example "nvme0n1", "mmcblk0", "loop0")
// gets a "p" infix before the partition number, so the number does not
// visually merge into the disk name's own trailing digit; every other
// disk name (for example "sda", "vda") does not. Mirrors
// internal/planbuild's own unexported partitionDevicePath: the manager
// computes this at plan-build time and the agent re-derives it here to
// verify the plan's own DeploySlot.Device fields (see Executor.validate),
// two different processes independently checking the same convention
// rather than importing one from the other.
func DevicePartitionPath(disk string, number int32) string {
	if disk == "" {
		return ""
	}
	sep := ""
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		sep = "p"
	}
	return fmt.Sprintf("%s%s%d", disk, sep, number)
}

// sfdiskJSONDump is the shape of `sfdisk --json`'s output that
// DeployPlan.SfdiskScript (and DeployDataImagePlan.SfdiskScript) carries
// verbatim. Only the fields this package needs to rebuild a restorable
// script are modeled.
type sfdiskJSONDump struct {
	PartitionTable sfdiskJSONTable `json:"partitiontable"`
}

type sfdiskJSONTable struct {
	// Label is "gpt" or "dos" (mbr).
	Label string `json:"label"`
	// ID is the GPT disk GUID, or the mbr disk signature ("0x...").
	ID string `json:"id,omitempty"`
	// SectorSize is in bytes; 0 means sfdisk omitted it (its own default
	// is 512).
	SectorSize int64                 `json:"sectorsize,omitempty"`
	Partitions []sfdiskJSONPartition `json:"partitions"`
}

type sfdiskJSONPartition struct {
	// Node is the source device's partition path, for example
	// "/dev/loop0p1" - not reused verbatim on restore (see SfdiskScript),
	// only its trailing digits (the partition number).
	Node string `json:"node"`
	// Start and Size are in sectors.
	Start int64 `json:"start"`
	Size  int64 `json:"size"`
	// Type is a GPT partition type GUID, or an mbr type as hex (e.g.
	// "83").
	Type string `json:"type"`
	// UUID is the GPT unique partition GUID (PARTUUID); absent for mbr.
	UUID string `json:"uuid,omitempty"`
	// Name is the GPT partition name (label); absent for mbr and for a
	// GPT partition sfdisk gave no name.
	Name string `json:"name,omitempty"`
	// Bootable is the MBR boot flag; meaningless for GPT.
	Bootable bool `json:"bootable,omitempty"`
}

// SfdiskScript converts an sfdisk --json dump into the sfdisk script
// (named-fields) input format sfdisk actually accepts on restore: sfdisk
// itself documents that it cannot take JSON as input (`sfdisk --help`'s
// description of -J/--json).
//
// Each partition line addresses its slot by device path
// (agentapi.DevicePartitionPath(targetDisk, number)) rather than by
// reusing dump.Node verbatim: the source device the dump was captured
// from essentially never shares a name with the real target disk this
// runs against, and named-fields format lets sfdisk resolve a partition's
// number from that leading device path either way.
func SfdiskScript(sfdiskJSON string, targetDisk string) (string, error) {
	var dump sfdiskJSONDump
	if err := json.Unmarshal([]byte(sfdiskJSON), &dump); err != nil {
		return "", fmt.Errorf("parse sfdisk JSON dump: %w", err)
	}

	table := dump.PartitionTable
	if table.Label == "" {
		return "", fmt.Errorf("sfdisk JSON dump has no partition table label")
	}
	if len(table.Partitions) == 0 {
		return "", fmt.Errorf("sfdisk JSON dump has no partitions")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "label: %s\n", table.Label)
	if table.ID != "" {
		fmt.Fprintf(&b, "label-id: %s\n", table.ID)
	}
	b.WriteString("unit: sectors\n")
	if table.SectorSize != 0 {
		fmt.Fprintf(&b, "sector-size: %d\n", table.SectorSize)
	}
	b.WriteString("\n")

	for i, p := range table.Partitions {
		number := sfdiskPartitionNumber(p.Node, i)
		device := DevicePartitionPath(targetDisk, number)

		fields := []string{
			fmt.Sprintf("start=%d", p.Start),
			fmt.Sprintf("size=%d", p.Size),
		}
		if p.Type != "" {
			fields = append(fields, fmt.Sprintf("type=%s", p.Type))
		}
		if p.UUID != "" {
			fields = append(fields, fmt.Sprintf("uuid=%s", p.UUID))
		}
		if p.Name != "" {
			fields = append(fields, fmt.Sprintf("name=%q", p.Name))
		}
		if p.Bootable {
			fields = append(fields, "bootable")
		}
		fmt.Fprintf(&b, "%s : %s\n", device, strings.Join(fields, ", "))
	}

	return b.String(), nil
}

// sfdiskPartitionNumber extracts the trailing decimal digits of an sfdisk
// node path (for example "/dev/loop0p3" -> 3, "/dev/sda1" -> 1), falling
// back to a 1-based list position if node has no trailing digits.
func sfdiskPartitionNumber(node string, listIndex int) int32 {
	end := len(node)
	start := end
	for start > 0 && node[start-1] >= '0' && node[start-1] <= '9' {
		start--
	}
	if start == end {
		return int32(listIndex + 1) //nolint:gosec // partition counts are always tiny
	}
	var n int32
	for _, c := range node[start:end] {
		n = n*10 + (c - '0')
	}
	return n
}

// MinimumDiskBytes returns the smallest device size (in bytes) that can
// hold every partition in sfdiskJSON: the end offset (start+size, both in
// sectors, converted with the dump's own sector size) of whichever
// partition ends furthest from the start of the disk. Executor uses this
// as the safety floor a target disk's actual size must meet before any
// destructive command runs against it.
func MinimumDiskBytes(sfdiskJSON string) (int64, error) {
	var dump sfdiskJSONDump
	if err := json.Unmarshal([]byte(sfdiskJSON), &dump); err != nil {
		return 0, fmt.Errorf("parse sfdisk JSON dump: %w", err)
	}
	sectorSize := dump.PartitionTable.SectorSize
	if sectorSize == 0 {
		sectorSize = 512
	}

	var maxEnd int64
	for _, p := range dump.PartitionTable.Partitions {
		end := (p.Start + p.Size) * sectorSize
		if end > maxEnd {
			maxEnd = end
		}
	}
	if maxEnd == 0 {
		return 0, fmt.Errorf("sfdisk JSON dump has no partitions")
	}
	return maxEnd, nil
}
