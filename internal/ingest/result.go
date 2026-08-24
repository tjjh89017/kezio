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
	"encoding/json"
	"fmt"
)

// TerminationMessageLimit is the size Kubernetes truncates a container
// termination message at. A Result over this never reaches the
// controller intact, so cmd/ingest fails the run with a clear message
// instead of emitting a payload that would parse as garbage on the other
// side.
const TerminationMessageLimit = 4096

// Result is the JSON payload kezio-ingest writes to its container's
// termination message on exit (see cmd/ingest), success or failure. The
// ImageImport controller reads it back from the completed Job's pod
// (containerStatuses[0].state.terminated.message) to create each
// partition's PartitionContent object and the Image binding them,
// without ever mounting the ingest work volume itself.
//
// The termination message is capped at TerminationMessageLimit, so this
// payload is kept to a compact per-partition summary plus the sfdisk JSON
// dump: it deliberately excludes the extent lists and piece hashes, which
// stay in torrent.info inside each partition's scratch content directory.
// The sfdisk dump has to travel here because the created Image's
// spec.layout.sfdiskJSON is what recreates the partition table at deploy
// time, and this is the only channel back from the ingest Job.
type Result struct {
	// Success is true when the run completed and every field below is
	// populated. False means Error explains what went wrong and the
	// payload fields are nil.
	Success bool `json:"success"`
	// Error is a human-readable failure message, set only when Success
	// is false.
	Error string `json:"error,omitempty"`
	// Disk is the captured disk-level layout and per-partition summary,
	// set only by a successful ingest-mode run.
	Disk *ResultDisk `json:"disk,omitempty"`
	// Publish is the published content's summary, set only by a
	// successful publish-mode run.
	Publish *ResultPublish `json:"publish,omitempty"`
}

// ResultDisk is the disk-level portion of a successful ingest Result.
type ResultDisk struct {
	// SizeBytes is the total size of the source disk image (the raw
	// conversion's file size).
	SizeBytes int64 `json:"sizeBytes"`
	// PartitionTable is "gpt" or "mbr" (api/v1alpha2's
	// PartitionTableGPT / PartitionTableMBR).
	PartitionTable string `json:"partitionTable"`
	// SfdiskJSON is the verbatim `sfdisk --dump --json` output for the
	// converted raw disk, compacted. It becomes the created Image's
	// spec.layout.sfdiskJSON.
	SfdiskJSON string `json:"sfdiskJSON"`
	// Partitions lists every partition ingest processed, in partition
	// number order.
	Partitions []ResultPartition `json:"partitions"`
}

// ResultPartition is one partition's summary in a successful ingest
// Result. The fields shared with api/v1alpha2.PartitionContentSpec
// (FSType, UsedBytes, SizeBytes, LastExtentEnd, PieceLength) let the
// ImageImport controller copy them across with no translation; Role,
// TypeGUID, PartUUID, UUID and SizeBytes fill in the Image slot that
// partition becomes. A swap partition (Role == "swap") carries no
// content: FSType, UsedBytes, LastExtentEnd and PieceLength are left
// zero, and UUID holds the file-system UUID to restore by instead.
type ResultPartition struct {
	Number        int32  `json:"number"`
	Role          string `json:"role"`
	FSType        string `json:"fsType,omitempty"`
	UsedBytes     int64  `json:"usedBytes,omitempty"`
	SizeBytes     int64  `json:"sizeBytes,omitempty"`
	LastExtentEnd int64  `json:"lastExtentEnd,omitempty"`
	PieceLength   int64  `json:"pieceLength,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	TypeGUID      string `json:"typeGUID,omitempty"`
	PartUUID      string `json:"partUUID,omitempty"`
}

// ResultPublish is the payload of a successful publish-mode run: the
// BitTorrent v1 info hash of the content now sitting in the
// PartitionContent's own PVC. It is computed from the copy that landed
// there, not from the scratch source, so it describes what a leecher will
// actually be served.
type ResultPublish struct {
	InfoHash string `json:"infoHash"`
}

// MarshalResult serializes r as compact JSON, suitable for writing to a
// container termination message.
func MarshalResult(r Result) ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal ingest result: %w", err)
	}
	return data, nil
}

// UnmarshalResult parses a Result previously written by MarshalResult.
func UnmarshalResult(data []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return Result{}, fmt.Errorf("parse ingest result: %w", err)
	}
	return r, nil
}

// FailureResult builds a Result reporting a failure with err's message.
func FailureResult(err error) Result {
	return Result{Success: false, Error: err.Error()}
}
