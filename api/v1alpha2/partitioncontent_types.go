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

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PartitionContentSource records where a PartitionContent's bytes were
// first ingested from, for audit only. It is not a reference the
// controller resolves: the source Image or partition may since have
// changed or been deleted.
type PartitionContentSource struct {
	// ImageName is the name of the Image this content was captured from.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ImageName string `json:"imageName"`
	// PartitionNumber is the partition number within that Image this
	// content was captured from.
	// +kubebuilder:validation:Minimum=1
	PartitionNumber int32 `json:"partitionNumber"`
}

// PartitionContentSpec defines the desired state of PartitionContent. It
// is the immutable, content-addressed record of one partition's data (see
// the type-level XValidation rule): every field describes the bytes
// identified by the object's name, so none of them can change without
// changing what the name identifies.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type PartitionContentSpec struct {
	// FSType is the filesystem type of the partition, for example "ext4"
	// or "ntfs".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	FSType string `json:"fsType"`
	// UsedBytes is the number of bytes of real data in the partition, as
	// measured at capture.
	// +kubebuilder:validation:Minimum=0
	UsedBytes int64 `json:"usedBytes"`
	// SizeBytes is the partition's size at capture.
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64 `json:"sizeBytes"`
	// LastExtentEnd is the end offset of the highest extent written at
	// ingest. Extents write at absolute offsets, so a target partition
	// must be at least this large; Image slot-size validation reads this
	// field to enforce that.
	// +kubebuilder:validation:Minimum=0
	LastExtentEnd int64 `json:"lastExtentEnd"`
	// PieceLength is the BitTorrent piece length used to hash this
	// content. It is a pinned constant, recorded here only: a piece
	// length that varies between ingests of identical bytes would fork
	// them into different info hashes, breaking the content-addressed
	// dedupe this object exists for.
	// +kubebuilder:validation:Minimum=1
	PieceLength int64 `json:"pieceLength"`
	// Source records where this content was first ingested from, for
	// audit only.
	Source PartitionContentSource `json:"source"`
}

// PartitionContent state enum values for PartitionContentStatus.State.
const (
	// PartitionContentStatePending means the object was created and
	// publishing has not started.
	PartitionContentStatePending = "Pending"
	// PartitionContentStatePublishing means the .torrent is being built
	// and the content is being made available to seed.
	PartitionContentStatePublishing = "Publishing"
	// PartitionContentStateReady means the .torrent exists at
	// status.torrentPath and the content is seedable.
	PartitionContentStateReady = "Ready"
	// PartitionContentStateFailed means publishing failed.
	PartitionContentStateFailed = "Failed"
)

// Condition types set on PartitionContentStatus.Conditions.
const (
	// PartitionContentConditionReady mirrors
	// PartitionContentStatus.State == Ready: the .torrent exists at
	// status.torrentPath and the content is seedable. It never carries a
	// seeder's address - plan builders resolve the seeder Pod address at
	// plan time, not from this status.
	PartitionContentConditionReady = "Ready"
	// PartitionContentConditionValid reports whether the ingested content
	// passed validation (for example, that the recorded pieceLength
	// actually hashes to this object's name).
	PartitionContentConditionValid = "Valid"
	// PartitionContentConditionSeederDegraded is True when the content has
	// fewer seeders than the operator considers safe, without affecting
	// Ready.
	PartitionContentConditionSeederDegraded = "SeederDegraded"
	// PartitionContentConditionDeletionBlocked is True while this content
	// has a deletion timestamp but PartitionContentFinalizer has not been
	// removed yet, because an Image slot or an active DeployRun still
	// references it. Absent otherwise (not set False - a content with no
	// deletion in progress has nothing to report here).
	PartitionContentConditionDeletionBlocked = "DeletionBlocked"
)

// PartitionContentFinalizer blocks a PartitionContent's actual removal
// while an Image slot or an active DeployRun still references it - see
// the PartitionContentReconciler's onDelete.
const PartitionContentFinalizer = "kezio.kojuro.date/partitioncontent"

// PartitionContent operational annotations, read directly off
// ObjectMeta.Annotations - no defaulting or schema covers them.
const (
	// PartitionContentAnnotationSeedDemand, present with any value, asks
	// the controller to run a seeder Deployment for this content (once
	// Ready) on the default, site-unaware network - see
	// PartitionContentSeederSite's Site doc comment for the "default"
	// sentinel this stage reports. The e2e lane sets and clears it
	// directly around a leecher run; a later item derives demand from
	// Machine/DeployRun state instead, at which point this annotation
	// becomes one input among several rather than the only one.
	PartitionContentAnnotationSeedDemand = "kezio.kojuro.date/seed-demand"
)

// PartitionContentSeederSite counts seeders for this content at one site.
type PartitionContentSeederSite struct {
	// Site identifies the seeding site, for example a rack or DC name.
	// Until seeding is site-aware, the controller reports exactly one
	// entry here with Site set to the "default" sentinel
	// (internal/controller's defaultSeederSite) rather than a real site
	// name; a later item replaces that with one entry per site actually
	// serving this content.
	// +kubebuilder:validation:MaxLength=253
	Site string `json:"site"`
	// MachineCount is the number of machines seeding this content at
	// this site.
	// +kubebuilder:validation:Minimum=0
	MachineCount int32 `json:"machineCount"`
}

// PartitionContentStatus defines the observed state of PartitionContent.
type PartitionContentStatus struct {
	// State is the object's position in the publish workflow.
	// +kubebuilder:validation:Enum=Pending;Publishing;Ready;Failed
	// +optional
	State string `json:"state,omitempty"`
	// PVCRef names the PVC holding this content's bytes, in the same
	// namespace as this object.
	// +optional
	PVCRef *NameRef `json:"pvcRef,omitempty"`
	// TorrentPath is the path of the .torrent file within the PVC named
	// by PVCRef. Only meaningful once State is Ready.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	TorrentPath string `json:"torrentPath,omitempty"`
	// Seeders is the per-site seeder count for this content. It never
	// carries a seeder's network address - plan builders resolve the
	// seeder Pod address at plan time.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Seeders []PartitionContentSeederSite `json:"seeders,omitempty"`
	// Conditions report the current state of this content.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="FSType",type=string,JSONPath=`.spec.fsType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PartitionContent is the Schema for the partitioncontents API. Its name
// is the BitTorrent v1 info hash of its content, prefixed "pc-"; the
// PartitionContent webhook enforces that name shape.
type PartitionContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PartitionContentSpec   `json:"spec,omitempty"`
	Status PartitionContentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PartitionContentList contains a list of PartitionContent.
type PartitionContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PartitionContent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PartitionContent{}, &PartitionContentList{})
}
