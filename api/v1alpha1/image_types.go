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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Image source format values for ImageSource.Format.
const (
	// ImageFormatRaw is an uncompressed raw disk image.
	ImageFormatRaw = "raw"
	// ImageFormatQCOW2 is a QEMU copy-on-write v2 disk image.
	ImageFormatQCOW2 = "qcow2"
	// ImageFormatVMDK is a VMware virtual disk image.
	ImageFormatVMDK = "vmdk"
)

// ImageSource describes where the source disk image comes from and how to
// verify it.
type ImageSource struct {
	// URL is the location the ingest job fetches the source image from. It
	// may be absent when the image is uploaded directly (for example
	// through kezioctl), in which case the ingest job waits for the upload
	// instead of fetching a URL.
	// +optional
	URL string `json:"url,omitempty"`
	// Format is the on-disk format of the source image.
	// +kubebuilder:validation:Enum=raw;qcow2;vmdk
	Format string `json:"format"`
	// Checksum verifies the fetched or uploaded source image before ingest
	// converts it. The expected form is "<algorithm>:<hex digest>", for
	// example "sha256:...".
	// +optional
	Checksum string `json:"checksum,omitempty"`
}

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

// ImageSpec defines the desired state of Image. It is create-only: the
// apiserver rejects any update to it. An Image's partitions are
// distributed by BitTorrent, and a torrent's info hash binds to fixed
// content; allowing spec to change after creation would let an in-use
// info hash silently point at different content and break every swarm
// seeding it. To publish new content, create an Image with a new name;
// to retry a failed ingest, delete the Image and create it again.
//
// Immutability is enforced per field below (self == oldSelf on each of
// Source, Bootable, OSFamily, PostHookRefs, and Params) rather than once
// on the whole struct. A single whole-object rule here previously read
// "self == oldSelf" directly on ImageSpec; it is deliberately not used
// because Params is schemaless (x-kubernetes-preserve-unknown-fields):
// CEL's equality over a struct that embeds an unstructured/schemaless
// field is markedly more fragile across apiserver versions and object
// shapes than equality over that struct's individually-typed fields, and
// a spurious inequality anywhere in the struct fails the whole update.
// Splitting the rule per field means a quirk isolated to Params cannot
// block updates that never touch Source, Bootable, OSFamily, or
// PostHookRefs, and vice versa.
type ImageSpec struct {
	// Source describes where the source disk image comes from and how to
	// verify it.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.source is immutable once the Image is created; create a new Image to publish different content"
	Source ImageSource `json:"source"`
	// Bootable marks this image as a bootable OS image: ingest expects an
	// ESP and finalize creates a boot entry. Set to false for a
	// non-bootable image (a data disk, a scratch layout, a pre-formatted
	// store), for which ingest does not require an ESP and finalize skips
	// the boot loader step. Defaults to true.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.bootable is immutable once the Image is created; create a new Image to publish different content"
	// +kubebuilder:default=true
	// +optional
	Bootable *bool `json:"bootable,omitempty"`
	// OSFamily gates OS-specific validation and builtins (for example
	// mkswap, which only applies to Linux). Defaults to Linux.
	// +kubebuilder:validation:Enum=Linux;Windows;FreeBSD;Other
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.osFamily is immutable once the Image is created; create a new Image to publish different content"
	// +kubebuilder:default=Linux
	// +optional
	OSFamily string `json:"osFamily,omitempty"`
	// PostHookRefs is an ordered list of PostHook resources attached to
	// this image. Execution order: this list runs before the attaching
	// Machine's own postHookRefs; within this list, the given order holds.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.postHookRefs is immutable once the Image is created; create a new Image to publish different content"
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
	// Params is schemaless input for the attached hooks' templating. Merge
	// order: this field first, then the deploying Machine's own params;
	// later entries override earlier ones.
	//
	// This is a pointer, not a value: apiextensionsv1.JSON.MarshalJSON
	// always emits a literal "null" for its zero value, and Go's
	// encoding/json "omitempty" does not suppress a zero-value struct (it
	// only suppresses nil pointers, nil maps/slices, and empty
	// strings/numbers). A value field would therefore round-trip an absent
	// Params as an explicit JSON null on every re-marshal - notably inside
	// the defaulting webhook, which re-marshals the whole object to
	// compute its mutation patch even when Default is a no-op, turning
	// "params never set" into a stray "add /spec/params null" patch on
	// every Image create or update. A pointer field is nil (and correctly
	// omitted) until something actually sets Params.
	//
	// No XValidation transition rule guards this field: a schemaless
	// (x-kubernetes-preserve-unknown-fields) value has no structural
	// schema for CEL to key equality off, which is the same fragility
	// this type's doc comment describes for the whole-struct rule this
	// replaced. Params immutability is instead enforced in Go, by
	// ImageCustomValidator.ValidateUpdate (see internal/webhook/v1alpha1),
	// which can compare the raw JSON directly.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`
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
	// ImageStatePending is the initial state: no ingest has started.
	ImageStatePending = "Pending"
	// ImageStateIngesting means the ingest job is fetching, converting, or
	// slicing the source image.
	ImageStateIngesting = "Ingesting"
	// ImageStateReady means the image has a disk layout, partitions, and
	// torrent metadata, and is available for deployment.
	ImageStateReady = "Ready"
	// ImageStateFailed means ingest failed; see the Ready condition's
	// message for details.
	ImageStateFailed = "Failed"
)

// Partition table enum values for ImageDiskStatus.PartitionTable.
const (
	// PartitionTableGPT is a GUID Partition Table.
	PartitionTableGPT = "gpt"
	// PartitionTableMBR is a legacy Master Boot Record partition table.
	PartitionTableMBR = "mbr"
)

// ImageDiskStatus records the disk-level layout captured during ingest.
type ImageDiskStatus struct {
	// SizeBytes is the total size of the source disk image.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// PartitionTable is the partition table type found on the source disk.
	// +kubebuilder:validation:Enum=gpt;mbr
	// +optional
	PartitionTable string `json:"partitionTable,omitempty"`
	// LayoutRef names the ConfigMap holding the sfdisk JSON dump of the
	// disk layout. This dump carries the GPT disk GUID and partition type
	// GUIDs so a restored disk matches the source exactly.
	// +optional
	LayoutRef *NameRef `json:"layoutRef,omitempty"`
}

// Partition role enum values for ImagePartitionStatus.Role. Roles are
// OS-neutral; ingest records what it finds rather than assuming a fixed
// role list per OS.
const (
	// PartitionRoleESP is the EFI System Partition.
	PartitionRoleESP = "esp"
	// PartitionRoleData is a general-purpose data partition; FSType
	// records its file system.
	PartitionRoleData = "data"
	// PartitionRoleSwap is a swap partition. Its exact handling at
	// finalize is per-OS.
	PartitionRoleSwap = "swap"
	// PartitionRoleMSR is a Microsoft Reserved partition: no file system,
	// cloned sector-for-sector.
	PartitionRoleMSR = "msr"
)

// ImagePartitionStatus records one partition captured during ingest.
type ImagePartitionStatus struct {
	// Number is the partition number on the source disk.
	Number int32 `json:"number"`
	// Role is the OS-neutral role of this partition.
	// +kubebuilder:validation:Enum=esp;data;swap;msr
	Role string `json:"role"`
	// FSType is the file system found on this partition, when applicable
	// (for example "vfat", "ext4"). Empty for roles without a file system
	// (msr) or handled without one (a swap partition restored by UUID).
	// +optional
	FSType string `json:"fsType,omitempty"`
	// UsedBytes is the amount of data ingest captured for this partition.
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
	// UUID is the source partition's file-system UUID, recorded for
	// partitions (such as swap) that are restored by UUID instead of by
	// torrent content.
	// +optional
	UUID string `json:"uuid,omitempty"`
	// InfoHash is the BitTorrent v1 info hash of this partition's content,
	// present once ingest has produced torrent metadata for it. Its
	// .torrent file lives alongside the content itself in this
	// partition's own PVC (see internal/controller/image_publish.go),
	// never in status: torrent.info scales with piece count, and status
	// carrying it either inline or as the ingest Job's termination
	// message (which is capped at 4KiB) silently failed to parse for an
	// Image with enough partitions (see internal/ingest.ResultPartition).
	// A deploy plan resolves this to a fetchable URL at plan-build time
	// instead (see internal/agentserver's buildPlanPartition and
	// agentapi.PlanPartition.TorrentURL).
	// +optional
	InfoHash string `json:"infoHash,omitempty"`
}

// ImageSeederSiteStatus records how many Machines at Site currently hold
// this Image's per-Image seeder Deployment demanded there. See
// internal/controller's machineHoldsSeederReference for which Machine
// states count, and its reconcileSeederDeployments for how this is
// computed and kept in sync with the Deployment(s) it describes.
// MachineCount can be zero: a site whose demand just dropped keeps its
// entry (and its Deployment) through the teardown grace period, so
// "still up with zero demand, about to drain" stays visible here rather
// than disappearing the instant the count changes.
type ImageSeederSiteStatus struct {
	// Site is the Machine's spec.networkSite value this count applies to
	// (the empty string is itself a valid, distinct site: every Machine
	// that never sets networkSite).
	Site string `json:"site"`
	// MachineCount is how many Machines at Site currently hold a seeder
	// reference to this Image.
	MachineCount int32 `json:"machineCount"`
}

// ImageStatus defines the observed state of Image.
type ImageStatus struct {
	// Conditions report the current state of the image, including
	// ConditionReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// State is the ingest state machine position.
	// +kubebuilder:validation:Enum=Pending;Ingesting;Ready;Failed
	// +optional
	State string `json:"state,omitempty"`
	// Disk records the disk-level layout captured during ingest.
	// +optional
	Disk *ImageDiskStatus `json:"disk,omitempty"`
	// Partitions records each partition captured during ingest.
	// +optional
	Partitions []ImagePartitionStatus `json:"partitions,omitempty"`
	// Seeders records, per site, how many Machines currently hold this
	// Image's seeder Deployment demanded there - the derived reference
	// count that decides whether that Deployment exists (see
	// ImageSeederSiteStatus's doc comment). Absent or empty means no
	// site currently demands a seeder Deployment for this Image.
	// +optional
	Seeders []ImageSeederSiteStatus `json:"seeders,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezi,image}
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Bootable",type=boolean,JSONPath=`.spec.bootable`
// +kubebuilder:printcolumn:name="OSFamily",type=string,JSONPath=`.spec.osFamily`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Image is the Schema for the images API.
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
