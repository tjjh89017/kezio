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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaimHardwareDiskSelector matches one disk of a candidate machine's
// MachineHardware. Every given field must match the same disk.
type ClaimHardwareDiskSelector struct {
	// Model matches the disk's reported model string exactly.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Model string `json:"model,omitempty"`
	// Vendor matches the disk's reported vendor string exactly.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Vendor string `json:"vendor,omitempty"`
	// MinSizeGigabytes rejects disks smaller than this size.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinSizeGigabytes *int64 `json:"minSizeGigabytes,omitempty"`
	// Rotational matches on whether the disk is a spinning disk (true) or
	// solid-state (false).
	// +optional
	Rotational *bool `json:"rotational,omitempty"`
}

// ClaimHardwareSelector matches a candidate machine's MachineHardware.
// Every field is a floor or an exact match, and all given fields must
// hold at the same time.
type ClaimHardwareSelector struct {
	// MinCPUCount rejects machines with fewer CPUs.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinCPUCount *int32 `json:"minCPUCount,omitempty"`
	// MinMemoryBytes rejects machines with less memory.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinMemoryBytes *int64 `json:"minMemoryBytes,omitempty"`
	// Disks lists per-disk requirements. Every entry must match a
	// different disk of the same machine.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Disks []ClaimHardwareDiskSelector `json:"disks,omitempty"`
}

// MachineClaimSelector chooses candidate machines by their labels and,
// optionally, their reported hardware.
type MachineClaimSelector struct {
	metav1.LabelSelector `json:",inline"`
	// Hardware matches the candidate's MachineHardware.
	// +optional
	Hardware *ClaimHardwareSelector `json:"hardware,omitempty"`
}

// MachineClaimSpec defines the desired state of MachineClaim: which
// machine the claim wants, and what to deploy on it.
// +kubebuilder:validation:XValidation:rule="!(has(self.machineName) && has(self.selector))",message="machineName and selector are mutually exclusive"
type MachineClaimSpec struct {
	// MachineName binds this claim to exactly one Machine by name. Give
	// this, or Selector, or neither - never both.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MachineName string `json:"machineName,omitempty"`
	// Selector chooses candidate machines by label and hardware, when
	// MachineName is not given.
	// +optional
	Selector *MachineClaimSelector `json:"selector,omitempty"`

	// ImageRef names the Image to deploy as the bound machine's OS. It
	// must be a bootable image. May be absent when the claim deploys
	// only DataImages and keeps the machine's existing OS.
	// +optional
	ImageRef *NameRef `json:"imageRef,omitempty"`
	// DataImages is an ordered list of additional, non-OS images
	// deployed alongside the OS image in the same live session (or by
	// themselves, when ImageRef is absent).
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DataImages []MachineDataImage `json:"dataImages,omitempty"`
	// TargetDisk selects the disk the OS image deploys to. Ignored when
	// ImageRef is absent.
	// +optional
	TargetDisk *TargetDiskHints `json:"targetDisk,omitempty"`
	// PostHookRefs is an ordered list of PostHook resources attached to
	// this claim's deployment.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
	// Params is schemaless input for the attached hooks' templating.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`
	// Ezio overrides the operator's cluster-wide default EZIO tuning for
	// the bound machine's leecher.
	// +optional
	Ezio *MachineEzioTuning `json:"ezio,omitempty"`
	// AfterDeploy selects what happens when a deployment finishes
	// without an OS image to reboot into. Defaults to Reboot.
	// +kubebuilder:validation:Enum=Reboot;PowerOff
	// +kubebuilder:default=Reboot
	// +optional
	AfterDeploy string `json:"afterDeploy,omitempty"`
}

// MachineClaim phase values for MachineClaimStatus.Phase.
const (
	// MachineClaimPhasePending means the claim has not bound to a
	// machine yet. It stays Pending while it waits for a candidate.
	MachineClaimPhasePending = "Pending"
	// MachineClaimPhaseBound means the claim is bound to the machine
	// named in status.machineName.
	MachineClaimPhaseBound = "Bound"
	// MachineClaimPhaseFailed means the claim cannot bind and must not
	// retry - for example, machineName names a machine that does not
	// exist.
	MachineClaimPhaseFailed = "Failed"
)

// Condition types set on MachineClaimStatus.Conditions.
const (
	// MachineClaimConditionBound reports whether a machine is bound to
	// this claim.
	MachineClaimConditionBound = "Bound"
	// MachineClaimConditionReady reports whether the bound machine
	// reached Provisioned for this claim's current intent.
	MachineClaimConditionReady = "Ready"
)

// MachineClaimFinalizer blocks MachineClaim deletion until the claim
// controller clears spec.claimRef on the bound machine.
const MachineClaimFinalizer = "kezio.kojuro.date/machineclaim"

// MachineClaimStatus defines the observed state of MachineClaim.
type MachineClaimStatus struct {
	// Phase is the claim's binding state.
	// +kubebuilder:validation:Enum=Pending;Bound;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// MachineName names the bound machine.
	// +optional
	MachineName string `json:"machineName,omitempty"`
	// BoundAt is when the claim bound to MachineName.
	// +optional
	BoundAt *metav1.Time `json:"boundAt,omitempty"`
	// CurrentRunRef names the DeployRun currently in flight for this
	// claim's intent.
	// +optional
	CurrentRunRef *NameRef `json:"currentRunRef,omitempty"`
	// LastSuccessfulRunRef names the most recent DeployRun that
	// completed successfully for this claim's intent.
	// +optional
	LastSuccessfulRunRef *NameRef `json:"lastSuccessfulRunRef,omitempty"`
	// Conditions report the current state of the claim.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Machine",type=string,JSONPath=`.status.machineName`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.imageRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MachineClaim is the Schema for the machineclaims API.
type MachineClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineClaimSpec   `json:"spec,omitempty"`
	Status MachineClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineClaimList contains a list of MachineClaim.
type MachineClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MachineClaim{}, &MachineClaimList{})
}
