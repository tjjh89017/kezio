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

package v1alpha3

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImportSource describes where to fetch and how to verify the source disk
// image an ImageImport captures.
type ImportSource struct {
	// URL is the location the ingest Job fetches the source image from.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	URL string `json:"url"`
	// Checksum verifies the fetched source image before ingest converts
	// it: "sha256:<hex digest>".
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Checksum string `json:"checksum"`
}

// ImageImportSpec defines the desired state of ImageImport: one source
// disk image, the names to give what comes out of it, and the Image-level
// settings to stamp onto the Image this import creates.
//
// The spec is immutable once created (see the type-level XValidation
// rule). An import runs partclone exactly once, in the cluster: the
// partition table, every partition's role and file system, and every
// content's size are things only that run can know, so nothing here
// describes them.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type ImageImportSpec struct {
	// Source is the disk image to import.
	Source ImportSource `json:"source"`
	// ImageName is the name of the Image this import creates once ingest
	// has captured the whole layout, in this ImageImport's own namespace.
	// The import fails if that name is already taken.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ImageName string `json:"imageName"`
	// ContentPrefix names the PartitionContent objects this import
	// captures: partition N becomes "<contentPrefix>-p<N>"
	// (internal/store.ContentName). The import fails if any of those names
	// is already taken - content is immutable, so an import never writes
	// over one. MaxLength leaves room for the "-p<N>" suffix and the
	// "-content" suffix each content's own PVC name carries
	// (internal/store.MaxContentNameLength).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=240
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ContentPrefix string `json:"contentPrefix"`
	// OSFamily is copied onto the created Image's spec.osFamily.
	// +kubebuilder:validation:Enum=Linux;Windows;FreeBSD;Other
	// +kubebuilder:default=Linux
	// +optional
	OSFamily string `json:"osFamily,omitempty"`
	// Bootable is copied onto the created Image's spec.bootable.
	// +kubebuilder:default=true
	// +optional
	Bootable *bool `json:"bootable,omitempty"`
	// Params is copied onto the created Image's spec.params.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`
	// PostHookRefs is copied onto the created Image's spec.postHookRefs.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
	// ScratchSize overrides the ingest scratch PVC's requested size. Left
	// unset, the manager computes it from the source image's discovered
	// size; set it when that computation is unavailable or wrong for this
	// source (for example, a source URL the manager cannot size ahead of
	// time).
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="scratchSize must be positive"
	// +optional
	ScratchSize *resource.Quantity `json:"scratchSize,omitempty"`
}

// ImageImport state enum values for ImageImportStatus.State.
const (
	// ImageImportStatePending means the import is waiting on manager-side
	// configuration before an ingest Job can be dispatched.
	ImageImportStatePending = "Pending"
	// ImageImportStateIngesting means the ingest Job is fetching,
	// converting, or slicing the source image.
	ImageImportStateIngesting = "Ingesting"
	// ImageImportStateReady means ingest finished and both the
	// PartitionContent objects and the Image exist.
	ImageImportStateReady = "Ready"
	// ImageImportStateFailed means ingest failed, or a name this import
	// had to create was already taken.
	ImageImportStateFailed = "Failed"
)

// Condition types set on ImageImportStatus.Conditions.
const (
	// ImageImportConditionReady mirrors ImageImportStatus.State == Ready.
	ImageImportConditionReady = "Ready"
)

// ImageImportStatus defines the observed state of ImageImport.
type ImageImportStatus struct {
	// State is the object's position in the import workflow.
	// +kubebuilder:validation:Enum=Pending;Ingesting;Ready;Failed
	// +optional
	State string `json:"state,omitempty"`
	// ImageRef names the Image this import created, once it exists.
	// +optional
	ImageRef *NameRef `json:"imageRef,omitempty"`
	// ContentRefs names every PartitionContent this import created, in
	// partition order.
	// +kubebuilder:validation:MaxItems=128
	// +optional
	ContentRefs []NameRef `json:"contentRefs,omitempty"`
	// Conditions report the current state of the import. Every write
	// carries observedGeneration set to the generation the write observed.
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
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.imageRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageImport is the Schema for the imageimports API. It is the request
// to turn one source disk image into a set of PartitionContent objects
// plus the Image that binds them, with exactly one partclone run - in the
// cluster - producing all of it.
type ImageImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageImportSpec   `json:"spec,omitempty"`
	Status ImageImportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageImportList contains a list of ImageImport.
type ImageImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageImport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageImport{}, &ImageImportList{})
}
