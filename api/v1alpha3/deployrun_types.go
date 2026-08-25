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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeployRunResolvedDisk records one image and the disk resolved to receive
// it, at the start of the run - the OS image or one dataImages entry.
type DeployRunResolvedDisk struct {
	// ImageRef names the Image being deployed.
	ImageRef NameRef `json:"imageRef"`
	// TargetDisk is the resolved device path the image is written to.
	// +kubebuilder:validation:MaxLength=255
	TargetDisk string `json:"targetDisk"`
}

// DeployRunSpec is the resolved deployment snapshot for one attempt. The
// Machine reconciler writes it once at creation; every field is immutable
// afterward (see the type-level XValidation rule) since it is the historical
// record of what this attempt was resolved to run.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type DeployRunSpec struct {
	// MachineRef names the Machine this run deploys to.
	MachineRef NameRef `json:"machineRef"`
	// ImageRef names the OS Image resolved for this run, copied from
	// Machine.spec.imageRef. Absent for a dataImages-only run.
	// +optional
	ImageRef *NameRef `json:"imageRef,omitempty"`
	// DataImages is the resolved non-OS image list, copied from
	// Machine.spec.dataImages.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DataImages []MachineDataImage `json:"dataImages,omitempty"`
	// ResolvedDisks records which disk each image above resolved to,
	// against the hardware inventory known at creation. Excluded from the
	// provisioning trigger's intent comparison: device names drift across
	// boots, so a difference here alone must never start a new run.
	// +kubebuilder:validation:MaxItems=33
	// +optional
	ResolvedDisks []DeployRunResolvedDisk `json:"resolvedDisks,omitempty"`
	// HooksHash is a content hash of every resolved PostHook step for this
	// run (Machine.spec.postHookRefs plus the OS Image's own
	// postHookRefs), used by the provisioning trigger to detect a hook
	// change without storing the resolved hook content itself.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	HooksHash string `json:"hooksHash,omitempty"`
}

// DeployRun phase values for DeployRunStatus.Phase, the sub-steps one
// deployment attempt passes through.
const (
	// DeployRunPhasePending means the run was created and nothing has
	// started yet.
	DeployRunPhasePending = "Pending"
	// DeployRunPhasePartitioning means every image's partition table is
	// being written and re-read, before any partition's content is
	// touched.
	DeployRunPhasePartitioning = "Partitioning"
	// DeployRunPhaseWritingContent means every partition is being made or
	// restored: mkfs/mkswap for blank and swap partitions, the BitTorrent
	// transfer for content partitions. Status.Partitions carries the
	// finer-grained per-partition detail this phase name does not.
	DeployRunPhaseWritingContent = "WritingContent"
	// DeployRunPhaseRunningPostHook means every partition's content and
	// file system is in place and the resolved hooks are running, in
	// order.
	DeployRunPhaseRunningPostHook = "RunningPostHook"
	// DeployRunPhaseFinalizing means every partition and hook is done and
	// finalize actions (boot entry, growLastPartition) are running.
	DeployRunPhaseFinalizing = "Finalizing"
	// DeployRunPhaseSucceeded is the terminal success phase.
	DeployRunPhaseSucceeded = "Succeeded"
	// DeployRunPhaseFailed is the terminal failure phase.
	DeployRunPhaseFailed = "Failed"
)

// Condition types set on DeployRunStatus.Conditions.
const (
	// DeployRunConditionSucceeded reports whether this run finished
	// successfully. Absent while the run is in progress.
	DeployRunConditionSucceeded = "Succeeded"
)

// DeployRunPartitionProgress is one partition's current status within a
// running deploy.
type DeployRunPartitionProgress struct {
	// Number is the partition number, matching the target disk's
	// partition table.
	Number int32 `json:"number"`
	// Percent is the partition's completion percentage, 0-100.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Percent int32 `json:"percent,omitempty"`
	// BytesDone is the number of bytes written to this partition so far.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BytesDone int64 `json:"bytesDone,omitempty"`
}

// DeployRunPhaseTiming records when the run entered and left one phase.
type DeployRunPhaseTiming struct {
	// Phase is one of the DeployRunPhase* constants.
	Phase string `json:"phase"`
	// StartedAt is when the run entered this phase.
	StartedAt metav1.Time `json:"startedAt"`
	// FinishedAt is when the run left this phase. Absent while the phase
	// is still current.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// DeployRunStatus defines the observed state of DeployRun.
type DeployRunStatus struct {
	// Phase is the run's current sub-step.
	// +kubebuilder:validation:Enum=Pending;Partitioning;WritingContent;RunningPostHook;Finalizing;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// Partitions is one entry per partition this run writes to, across
	// the OS image and every dataImages entry.
	// +kubebuilder:validation:MaxItems=128
	// +optional
	Partitions []DeployRunPartitionProgress `json:"partitions,omitempty"`
	// PhaseTimings records the start and end time of every phase this run
	// has entered, in the order it entered them.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	PhaseTimings []DeployRunPhaseTiming `json:"phaseTimings,omitempty"`
	// LastProgressAt is when the manager last accepted a progress report
	// from the agent for this run, by the manager's own clock - unlike
	// PhaseTimings, which carry the agent's timestamps, so that staleness
	// decisions never depend on the live environment's clock being right.
	// Absent until the run's first report (or its Pending commit) lands.
	// +optional
	LastProgressAt *metav1.Time `json:"lastProgressAt,omitempty"`
	// Conditions report the current state of this run.
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
// +kubebuilder:printcolumn:name="Machine",type=string,JSONPath=`.spec.machineRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DeployRun is the Schema for the deployruns API.
type DeployRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DeployRunSpec   `json:"spec,omitempty"`
	Status DeployRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DeployRunList contains a list of DeployRun.
type DeployRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeployRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DeployRun{}, &DeployRunList{})
}
