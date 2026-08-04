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
	"context"
	"encoding/json"
	"fmt"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// defaultSectorSize is the sector size sfdisk assumes when its JSON dump
// omits "sectorsize" (it only reports the field when the disk's logical
// sector size differs from the classic 512-byte default).
const defaultSectorSize = 512

// Sfdisk dumps a disk's partition table as JSON. The exec-backed
// production implementation (`sfdisk --json <path>`) lives in cmd/ingest;
// tests use a fake or a fixture file.
type Sfdisk interface {
	Dump(ctx context.Context, diskPath string) ([]byte, error)
}

// sfdiskDump is the shape of `sfdisk --json`'s output. Only the fields
// this package needs are modeled; sfdisk's JSON has more (bootable,
// firstlba/lastlba, ...) that ingest does not use.
type sfdiskDump struct {
	PartitionTable sfdiskPartitionTable `json:"partitiontable"`
}

type sfdiskPartitionTable struct {
	// Label is "gpt" or "dos" (mbr).
	Label string `json:"label"`
	// ID is the GPT disk GUID, or the mbr disk signature ("0x...");
	// only the gpt case maps to ImageLayout.DiskGUID.
	ID string `json:"id,omitempty"`
	// SectorSize is in bytes; 0 means sfdisk omitted it (see
	// defaultSectorSize).
	SectorSize int64             `json:"sectorsize,omitempty"`
	Partitions []sfdiskPartition `json:"partitions"`
}

type sfdiskPartition struct {
	Node string `json:"node"`
	// Start and Size are in sectors.
	Start int64 `json:"start"`
	Size  int64 `json:"size"`
	// Type is a GPT partition type GUID, or an mbr type as hex (e.g.
	// "83").
	Type string `json:"type"`
	// UUID is the GPT unique partition GUID (PARTUUID); absent for mbr.
	UUID string `json:"uuid,omitempty"`
}

// ParsedDisk is the pure-Go result of parsing an sfdisk JSON dump: the
// disk-level facts plus one ParsedPartition per partition, in the order
// sfdisk reported them (ascending start offset).
type ParsedDisk struct {
	PartitionTable string // keziov1alpha1.PartitionTableGPT or PartitionTableMBR
	DiskGUID       string
	SectorSize     int64
	Partitions     []ParsedPartition
}

// ParsedPartition is one partition's identity fields, already converted
// to bytes.
type ParsedPartition struct {
	// Number is the 1-based partition number, taken from position in
	// the sfdisk dump (sfdisk lists partitions in table order, and a
	// disk with a gap - a deleted middle partition - still numbers by
	// slot, not by list position; ParseSfdiskJSON derives Number from
	// Node's trailing digits when present, falling back to list
	// position).
	Number     int32
	StartBytes int64
	SizeBytes  int64
	TypeGUID   string
	PartUUID   string
}

// ParseSfdiskJSON parses sfdisk --json output into a ParsedDisk. It is
// pure and side-effect free, so it is the part of the sfdisk step this
// package unit tests directly against fixture JSON.
func ParseSfdiskJSON(data []byte) (*ParsedDisk, error) {
	var dump sfdiskDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, fmt.Errorf("parse sfdisk JSON: %w", err)
	}

	table := dump.PartitionTable
	var partitionTable string
	switch strings.ToLower(table.Label) {
	case "gpt":
		partitionTable = keziov1alpha1.PartitionTableGPT
	case "dos", "mbr":
		partitionTable = keziov1alpha1.PartitionTableMBR
	default:
		return nil, fmt.Errorf("unsupported partition table label %q", table.Label)
	}

	sectorSize := table.SectorSize
	if sectorSize == 0 {
		sectorSize = defaultSectorSize
	}

	disk := &ParsedDisk{
		PartitionTable: partitionTable,
		SectorSize:     sectorSize,
	}
	if partitionTable == keziov1alpha1.PartitionTableGPT {
		disk.DiskGUID = table.ID
	}

	for i, p := range table.Partitions {
		disk.Partitions = append(disk.Partitions, ParsedPartition{
			Number:     partitionNumber(p.Node, i),
			StartBytes: p.Start * sectorSize,
			SizeBytes:  p.Size * sectorSize,
			TypeGUID:   strings.ToLower(p.Type),
			PartUUID:   p.UUID,
		})
	}

	return disk, nil
}

// partitionNumber extracts the trailing decimal digits of an sfdisk node
// path (for example "/dev/loop0p3" -> 3, "/dev/sda1" -> 1). It falls back
// to a 1-based list position if node has no trailing digits, which keeps
// this total even against an sfdisk build that omits "node".
func partitionNumber(node string, listIndex int) int32 {
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
