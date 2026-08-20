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

// Result is the JSON payload kezio-ingest writes to its container's
// termination message on exit (see cmd/ingest), success or failure. The
// Image controller reads it back from the completed Job's pod
// (containerStatuses[0].state.terminated.message) to create or find each
// partition's PartitionContent object, without ever mounting the ingest
// work volume itself.
//
// Kubernetes caps a termination message at 4KiB, so this payload is kept
// to a compact per-partition summary: it deliberately excludes the full
// extent lists and piece hashes (those stay in torrent.info inside each
// partition's scratch content directory) and the verbatim sfdisk JSON
// dump, which an unusually large partition table could grow past the cap
// on its own.
type Result struct {
	// Success is true when ingest completed and every field below is
	// populated. False means Error explains what went wrong and Disk is
	// nil.
	Success bool `json:"success"`
	// Error is a human-readable failure message, set only when Success
	// is false.
	Error string `json:"error,omitempty"`
	// Disk is the captured disk-level layout and per-partition summary,
	// set only when Success is true.
	Disk *ResultDisk `json:"disk,omitempty"`
}

// ResultDisk is the disk-level portion of a successful Result.
type ResultDisk struct {
	// SizeBytes is the total size of the source disk image (the raw
	// conversion's file size).
	SizeBytes int64 `json:"sizeBytes"`
	// PartitionTable is "gpt" or "mbr" (api/v1alpha2's
	// PartitionTableGPT / PartitionTableMBR).
	PartitionTable string `json:"partitionTable"`
	// Partitions lists every partition ingest processed, in partition
	// number order.
	Partitions []ResultPartition `json:"partitions"`
}

// ResultPartition is one partition's summary in a successful Result. The
// fields shared with api/v1alpha2.PartitionContentSpec (FSType,
// UsedBytes, SizeBytes, LastExtentEnd, PieceLength) let the Image
// controller copy them across with no translation; InfoHash names the
// PartitionContent object itself (see internal/store.ObjectName). A
// swap partition (Role == "swap") carries no content: FSType, UsedBytes,
// LastExtentEnd, PieceLength, and InfoHash are all left zero, and UUID
// holds the file-system UUID to restore by instead.
type ResultPartition struct {
	Number        int32  `json:"number"`
	Role          string `json:"role"`
	FSType        string `json:"fsType,omitempty"`
	UsedBytes     int64  `json:"usedBytes,omitempty"`
	SizeBytes     int64  `json:"sizeBytes,omitempty"`
	LastExtentEnd int64  `json:"lastExtentEnd,omitempty"`
	PieceLength   int64  `json:"pieceLength,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	InfoHash      string `json:"infoHash,omitempty"`
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
