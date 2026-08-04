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
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Well-known partition type identifiers used to classify a slot's role.
// GPT types are GUIDs (the "Discoverable Partitions Specification"
// values); the mbr type is the classic two-hex-digit EFI System
// Partition code. All comparisons are case-insensitive; ParseSfdiskJSON
// already lowercases TypeGUID, and these constants are declared lowercase
// to match.
const (
	espTypeGUIDGPT = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	espTypeMBR     = "ef"
	msrTypeGUIDGPT = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
)

// blkidSwapType is the file-system type blkid reports for a Linux swap
// partition.
const blkidSwapType = "swap"

// classifyRole assigns an OS-neutral role (api/v1alpha1's PartitionRole*
// constants) to a partition, from its type identifier and detected file
// system. Swap is detected by file system, not by partition type GUID,
// because a partition can carry the Linux-swap type GUID yet (rarely)
// hold something else; blkid's signature detection is the more direct
// source of truth, and it is what determines whether ingest treats the
// partition as swap (no content, UUID only) in the first place.
func classifyRole(typeGUID, fsType string) string {
	normalizedType := strings.ToLower(typeGUID)
	switch normalizedType {
	case espTypeGUIDGPT, espTypeMBR:
		return keziov1alpha1.PartitionRoleESP
	case msrTypeGUIDGPT:
		return keziov1alpha1.PartitionRoleMSR
	}
	if strings.EqualFold(fsType, blkidSwapType) {
		return keziov1alpha1.PartitionRoleSwap
	}
	return keziov1alpha1.PartitionRoleData
}
