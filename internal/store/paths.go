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

import "fmt"

// This file defines the naming contract: every PartitionContent gets its
// own PVC (no shared multi-content store root), so naming reduces to
// deriving the PVC name from the object name. The object name itself is
// chosen by the user at import time, not derived from the content's info
// hash - the info hash only exists after partclone has run, and making it
// the identity forced a second partclone run outside the cluster just to
// learn the names. The filesystem layout inside that PVC - torrent.info,
// a content/ data subdirectory, content.torrent - is defined in layout.go
// and is a plain path contract with no Kubernetes API involved.

const (
	// pvcContentSuffix is the fixed suffix on the PVC name a
	// PartitionContent owns, appended to the object name.
	pvcContentSuffix = "-content"
	// partitionInfix separates an import's content prefix from the
	// partition number in a generated content name.
	partitionInfix = "-p"
	// k8sObjectNameMaxLength is the Kubernetes object name limit (RFC 1123
	// subdomain).
	k8sObjectNameMaxLength = 253
)

// MaxContentNameLength is the longest a PartitionContent name may be. It
// is shorter than the Kubernetes object name limit because that content's
// PVC name is the object name plus pvcContentSuffix, and the PVC has to
// fit the same limit.
const MaxContentNameLength = k8sObjectNameMaxLength - len(pvcContentSuffix)

// ContentName returns the PartitionContent object name an import gives
// partition number within the import's content prefix:
// "<prefix>-p<number>". A user who wants a different name creates the
// Image slot against a content they named themselves instead.
func ContentName(prefix string, partitionNumber int32) string {
	return fmt.Sprintf("%s%s%d", prefix, partitionInfix, partitionNumber)
}

// PVCName returns the name of the PVC that holds contentName's bytes,
// owned by the PartitionContent named contentName.
func PVCName(contentName string) string {
	return contentName + pvcContentSuffix
}
