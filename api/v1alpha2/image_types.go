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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OS family enum values for ImageSpec.OSFamily.
const (
	// OSFamilyLinux is a Linux target. The default when OSFamily is absent.
	OSFamilyLinux = "Linux"
	// OSFamilyWindows is a Windows target.
	OSFamilyWindows = "Windows"
	// OSFamilyFreeBSD is a FreeBSD target.
	OSFamilyFreeBSD = "FreeBSD"
	// OSFamilyOther is any target OS without dedicated builtin support.
	OSFamilyOther = "Other"
)

// Partition table enum values for ImageDiskLayout.PartitionTable.
const (
	// PartitionTableGPT is a GUID Partition Table.
	PartitionTableGPT = "gpt"
	// PartitionTableMBR is a legacy Master Boot Record partition table.
	PartitionTableMBR = "mbr"
)

// Partition role enum values for ImageSlot.Role. Roles are OS-neutral: a
// composed Image assigns them by hand, and an ingested one records what it
// found, rather than either assuming a fixed role list per OS.
const (
	// PartitionRoleESP is the EFI System Partition.
	PartitionRoleESP = "esp"
	// PartitionRoleData is a general-purpose data partition; FSType (for a
	// blank slot) or the referenced content records its file system.
	PartitionRoleData = "data"
	// PartitionRoleSwap is a swap partition, restored by UUID rather than
	// by content.
	PartitionRoleSwap = "swap"
	// PartitionRoleMSR is a Microsoft Reserved partition: no file system,
	// cloned sector-for-sector.
	PartitionRoleMSR = "msr"
)

// ImageSlot is one position in the partition table: a partition number
// plus, optionally, either the content that fills it or the fsType to
// create fresh at deploy time. TypeGUID, PartUUID, and SizeBytes carry no
// restore semantics of their own; they exist so a future adopt mode can
// match this slot against partitions already present on a target disk.
// +kubebuilder:validation:XValidation:rule="!(has(self.contentRef) && has(self.fsType))",message="a slot cannot set both contentRef and fsType: content already carries its own file system"
type ImageSlot struct {
	// Number is the partition number in the partition table (1-based, as
	// sfdisk and the kernel report it). Unique within a Image's slots (see
	// the list-map key on ImageDiskLayout.Slots).
	// +kubebuilder:validation:Minimum=1
	Number int32 `json:"number"`
	// Role is the OS-neutral role of this slot.
	// +kubebuilder:validation:Enum=esp;data;swap;msr
	Role string `json:"role"`
	// ContentRef names the PartitionContent this slot restores. Absent
	// marks this a blank slot: nothing is restored here; the deploy agent
	// creates the file system fresh (mkfs, using FSType) or, for a swap
	// slot, runs mkswap using UUID. ContentRef.Namespace, if set, must
	// equal this Image's own namespace - the webhook denies any other
	// value, since PartitionContent's deletion finalizer only ever looks
	// for a referencing Image within the content's own namespace.
	// +optional
	ContentRef *NameRef `json:"contentRef,omitempty"`
	// FSType is the file system to create fresh at deploy time, for a
	// blank slot with Role == data. Mutually exclusive with ContentRef:
	// restored content already carries its own file system.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	FSType string `json:"fsType,omitempty"`
	// UUID is the file-system UUID to restore by, for a swap slot
	// (Role == swap) that has no content to restore.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	UUID string `json:"uuid,omitempty"`
	// TypeGUID is the partition type identifier this slot is written
	// with: a GPT partition type GUID, or an MBR partition type as two
	// lowercase hex digits (for example "ef" for an EFI System
	// Partition).
	// +kubebuilder:validation:MaxLength=64
	// +optional
	TypeGUID string `json:"typeGUID,omitempty"`
	// PartUUID is the GPT unique partition GUID this slot is written
	// with; empty for an mbr partition table, which has no per-partition
	// UUID.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	PartUUID string `json:"partUUID,omitempty"`
	// SizeBytes is this slot's size, when known ahead of deploy-time
	// resolution.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// ImageDiskLayout is an Image's disk-level partition table plus its
// ordered list of slots. There is no separate layout kind: the sfdisk JSON
// dump and the slot list that annotates it live inline on the Image that
// owns them.
type ImageDiskLayout struct {
	// PartitionTable is the partition table type this layout writes.
	// +kubebuilder:validation:Enum=gpt;mbr
	PartitionTable string `json:"partitionTable"`
	// SfdiskJSON is the sfdisk JSON dump describing the partition table:
	// the same format `sfdisk --dump --json` produces, and `sfdisk` accepts
	// back to recreate the table.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	SfdiskJSON string `json:"sfdiskJSON"`
	// Slots is the ordered list of partition slots, in partition-table
	// order.
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=number
	Slots []ImageSlot `json:"slots"`
}

// ImageSpec defines the desired state of Image: a disk layout of ordered
// slots, each optionally bound to a PartitionContent. The whole spec is
// immutable once created (see the type-level XValidation rule): a slot's
// ContentRef binds into a BitTorrent swarm, and allowing the layout to
// change afterward would let an in-use Image silently start describing
// different content. To publish a different layout or content set, create
// an Image with a new name.
//
// An Image never ingests anything itself: it is always a declaration over
// PartitionContent objects that already exist. An ImageImport is what
// runs partclone, captures those objects, and creates the Image once it
// knows the whole layout - so this spec is complete the moment it is
// created and never has to be patched afterwards.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type ImageSpec struct {
	// OSFamily gates OS-specific validation and builtins (for example
	// mkswap, which only applies to Linux). Defaults to Linux.
	// +kubebuilder:validation:Enum=Linux;Windows;FreeBSD;Other
	// +kubebuilder:default=Linux
	// +optional
	OSFamily string `json:"osFamily,omitempty"`
	// Bootable marks this image as a bootable OS image: deploy expects an
	// ESP slot and finalize creates a boot entry. Set to false for a
	// non-bootable image (a data disk, a scratch layout). Defaults to
	// true.
	// +kubebuilder:default=true
	// +optional
	Bootable *bool `json:"bootable,omitempty"`
	// Layout is this image's disk-level partition table and ordered slot
	// list.
	Layout ImageDiskLayout `json:"layout"`
	// Params is schemaless input for the attached hooks' templating.
	// Merge order: a PostHook's own declared param defaults first, then
	// this field, then the deploying Machine's own spec.params; later
	// entries override earlier ones.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`
	// PostHookRefs is an ordered list of PostHook resources this image's
	// own content needs. Namespace, if set, must equal this Image's own
	// namespace - the webhook denies any other value, the same rule
	// ImageSlot.ContentRef's own namespace restriction gives. Execution
	// order: these run first, ahead of the deploying Machine's own
	// postHookRefs (see MachineSpec.PostHookRefs's doc comment).
	// +kubebuilder:validation:MaxItems=64
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
}

// EffectiveBootable returns whether the image is bootable, treating a nil
// Bootable field as true (the default).
func (s ImageSpec) EffectiveBootable() bool {
	if s.Bootable == nil {
		return true
	}
	return *s.Bootable
}

// EffectiveOSFamily returns the OS family to use, treating an empty
// OSFamily field as OSFamilyLinux (the default).
func (s ImageSpec) EffectiveOSFamily() string {
	if s.OSFamily == "" {
		return OSFamilyLinux
	}
	return s.OSFamily
}

// Image state enum values for ImageStatus.State.
const (
	// ImageStatePending is the initial state: referenced content has not
	// yet been checked, or is not all Ready.
	ImageStatePending = "Pending"
	// ImageStateReady means every slot's referenced content (if any) is
	// Ready.
	ImageStateReady = "Ready"
	// ImageStateFailed means referenced-content validation failed; see the
	// Ready condition's message for details.
	ImageStateFailed = "Failed"
)

// Condition types set on ImageStatus.Conditions.
const (
	// ImageConditionReady mirrors ImageStatus.State == Ready.
	ImageConditionReady = "Ready"
	// ImageConditionValid reports whether the image's own schema-level
	// validation passed (for example, that every slot's referenced
	// content resolves and its lastExtentEnd fits the slot).
	ImageConditionValid = "Valid"
	// ImageConditionSeederDegraded reports a problem with this image's
	// per-Site seeder Deployment placement that would otherwise surface
	// nowhere: a Site with demand but no seederSubnetRef, a seeder
	// Deployment name occupied by an object this Image does not control,
	// or a demanded seeder Deployment whose pod never became Ready.
	// Absent entirely (never set False) when nothing is wrong.
	ImageConditionSeederDegraded = "SeederDegraded"
)

// ImageStatus defines the observed state of Image.
//
// The Image reconciler is thin: it aggregates the readiness of this
// image's referenced PartitionContent objects and drives the per-Site
// seeder Deployments those contents are served from. It runs no ingest of
// its own - that belongs to ImageImport - so an Image is create-only
// metadata over content that already exists.
type ImageStatus struct {
	// State is the object's position in the readiness workflow.
	// +kubebuilder:validation:Enum=Pending;Ready;Failed
	// +optional
	State string `json:"state,omitempty"`
	// Conditions report the current state of the image, including
	// ImageConditionReady and ImageConditionValid. Every write carries
	// observedGeneration (metav1.Condition's own field) set to the
	// generation the write observed.
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
// +kubebuilder:printcolumn:name="Bootable",type=boolean,JSONPath=`.spec.bootable`
// +kubebuilder:printcolumn:name="OSFamily",type=string,JSONPath=`.spec.osFamily`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Image is the Schema for the images API. It binds a disk layout to
// ordered slots that reference PartitionContent objects. It is created
// either by an ImageImport, once that import knows the whole layout, or
// by a user composing existing content by name.
type Image struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageSpec   `json:"spec,omitempty"`
	Status ImageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageList contains a list of Image.
type ImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Image `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Image{}, &ImageList{})
}
