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
	"encoding/json"
	"fmt"
	"os"
)

// ImageLayoutSchemaVersion is the schema version this package writes to
// layout.json. Bump it, and branch on it in LoadImageLayout, the day the
// shape of ImageLayout needs a backward-incompatible change.
const ImageLayoutSchemaVersion = 1

// ImageLayout is the schema of images/<name>/layout.json (see
// ImageLayoutPath): an Image's disk-level layout plus its ordered list of
// slots, matching the design's "partition content model" (a slot is a
// position on the target disk; a partition content is the immutable,
// content-addressed payload a slot may reference; the two are decoupled so
// more than one Image can reference the same content). This package only
// defines and (de)serializes the schema; ingest populates it and the Image
// reconciler reads it back into ImageStatus.
type ImageLayout struct {
	// SchemaVersion is ImageLayoutSchemaVersion at the time this file was
	// written.
	SchemaVersion int `json:"schemaVersion"`
	// PartitionTable is the partition table type found on the source
	// disk: "gpt" or "mbr" (mirrors ImageDiskStatus.PartitionTable's
	// enum in api/v1alpha1).
	PartitionTable string `json:"partitionTable"`
	// DiskGUID is the GPT disk GUID ("id" in sfdisk's JSON dump), empty
	// for an mbr partition table.
	DiskGUID string `json:"diskGUID,omitempty"`
	// SizeBytes is the total size of the source disk image.
	SizeBytes int64 `json:"sizeBytes"`
	// SectorSize is the sector size sfdisk used to compute Start/Size
	// for every slot, in bytes. Slots below already carry byte offsets
	// (StartBytes/SizeBytes), so this field is informational (it lets a
	// consumer cross-check against the sfdisk JSON dump); it is not
	// needed to interpret a slot.
	SectorSize int64 `json:"sectorSize,omitempty"`
	// Slots is the ordered list of partition slots, in partition-table
	// order.
	Slots []ImageLayoutSlot `json:"slots"`
}

// ImageLayoutSlot is one position on the target disk. Its identity fields
// (TypeGUID, PartUUID, StartBytes, SizeBytes) let the deploy step write
// controlled PARTUUIDs and give a future adopt-mode the data to match
// existing partitions, independent of whatever content (if any) it
// references.
type ImageLayoutSlot struct {
	// Number is the partition number on the source disk (1-based, as
	// sfdisk and the kernel report it).
	Number int32 `json:"number"`
	// StartBytes is this slot's byte offset on the disk.
	StartBytes int64 `json:"startBytes"`
	// SizeBytes is this slot's size in bytes.
	SizeBytes int64 `json:"sizeBytes"`
	// TypeGUID is the partition type identifier: a GPT partition type
	// GUID, or an MBR partition type as two lowercase hex digits (for
	// example "ef" for an EFI System Partition). Empty is never valid
	// for a real slot; ingest always records what it finds.
	TypeGUID string `json:"typeGUID,omitempty"`
	// PartUUID is the source partition's PARTUUID (GPT unique partition
	// GUID; empty for mbr, which has no per-partition UUID).
	PartUUID string `json:"partUUID,omitempty"`
	// Role is the OS-neutral role ingest assigned this slot (see the
	// PartitionRole* constants in api/v1alpha1).
	Role string `json:"role"`
	// FSType is the file system found on this slot, when applicable.
	// Empty for a role without a file system (msr) or a slot restored
	// without one (swap, by UUID).
	FSType string `json:"fsType,omitempty"`
	// ContentInfoHash is the BitTorrent v1 info hash of this slot's
	// partition content (see ContentDir), present once ingest has
	// stored one. Empty together with an empty SwapUUID marks this a
	// blank slot: no content is restored here, and the agent creates
	// the file system fresh (mkfs) at deploy time using FSType. Ingest
	// itself never produces a blank slot (it always captures what it
	// finds); blank slots are how a later hand-authored or composed
	// Image can add an empty data partition.
	ContentInfoHash string `json:"contentInfoHash,omitempty"`
	// SwapUUID is the source swap partition's file-system UUID. Set
	// only for Role == swap: swap has no content to restore, so the
	// agent runs mkswap with this UUID instead of resolving
	// ContentInfoHash.
	SwapUUID string `json:"swapUUID,omitempty"`
}

// IsBlank reports whether slot has nothing to restore: no partition
// content and no swap UUID. A blank slot with a non-empty FSType still
// tells the agent to create that file system fresh at deploy time; a blank
// slot with an empty FSType is unformatted space.
func (s ImageLayoutSlot) IsBlank() bool {
	return s.ContentInfoHash == "" && s.SwapUUID == ""
}

// WriteImageLayout writes layout to <root>/images/<imageName>/layout.json,
// creating the image directory if necessary. The write is atomic (a temp
// file renamed into place), so a reader never observes a partially written
// file. imageName must already be a validated Kubernetes object name (see
// ImageDir).
func WriteImageLayout(root, imageName string, layout *ImageLayout) error {
	dir := ImageDir(root, imageName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create image dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(layout, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image layout for %s: %w", imageName, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ImageLayoutFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp layout file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp layout file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp layout file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, ImageLayoutPath(root, imageName)); err != nil {
		return fmt.Errorf("finalize layout file for %s: %w", imageName, err)
	}
	succeeded = true
	return nil
}

// LoadImageLayout reads and parses <root>/images/<imageName>/layout.json.
func LoadImageLayout(root, imageName string) (*ImageLayout, error) {
	path := ImageLayoutPath(root, imageName)
	data, err := os.ReadFile(path) //nolint:gosec // root/imageName are operator-controlled store paths, not user input
	if err != nil {
		return nil, fmt.Errorf("read image layout %s: %w", path, err)
	}
	layout := &ImageLayout{}
	if err := json.Unmarshal(data, layout); err != nil {
		return nil, fmt.Errorf("parse image layout %s: %w", path, err)
	}
	return layout, nil
}
