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

// This file defines the content-addressed naming contract: every
// PartitionContent gets its own PVC (no shared multi-content store root),
// so naming reduces to deriving the object and PVC names from an InfoHash.
// The filesystem layout inside that PVC - torrent.info, a content/ data
// subdirectory, content.torrent - is defined in layout.go and is a plain
// path contract with no Kubernetes API involved.

const (
	// objectNamePrefix is the fixed prefix every PartitionContent object
	// name carries before its info hash (also enforced by the
	// PartitionContent webhook).
	objectNamePrefix = "pc-"
	// pvcContentSuffix is the fixed suffix on the PVC name a
	// PartitionContent owns, appended to the object name.
	pvcContentSuffix = "-content"
	// ContentTorrentFileName is the generated .torrent file name inside a
	// content's own PVC.
	ContentTorrentFileName = "content.torrent"
)

// ObjectName returns the PartitionContent object name for hash:
// "pc-<info-hash-hex>".
func ObjectName(hash InfoHash) string {
	return objectNamePrefix + hash.String()
}

// PVCName returns the name of the PVC that holds hash's content bytes,
// owned by the PartitionContent named ObjectName(hash).
func PVCName(hash InfoHash) string {
	return ObjectName(hash) + pvcContentSuffix
}
