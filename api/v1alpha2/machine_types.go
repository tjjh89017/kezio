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

// MachineBMC identifies the board management controller that powers and
// boot-orders the machine, and the credentials used to reach it.
type MachineBMC struct {
	// Address is the BMC endpoint URL. Its scheme selects the driver, for
	// example "redfish://" or "ipmi://".
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9+.-]*://.+`
	Address string `json:"address"`
	// CredentialsSecretRef names the Secret holding the BMC username and
	// password.
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef"`
}

// TargetDiskHints selects the disk a deployment writes to. All given
// fields must match the same disk (logical AND); the controller matches
// these hints against the agent-reported disk inventory and requires
// exactly one match before any write.
// +kubebuilder:validation:XValidation:rule="!has(self.minSizeGigabytes) || !has(self.maxSizeGigabytes) || self.minSizeGigabytes <= self.maxSizeGigabytes",message="minSizeGigabytes must be less than or equal to maxSizeGigabytes"
type TargetDiskHints struct {
	// DeviceName is the kernel device path, for example "/dev/nvme0n1".
	// This is the last-choice hint: device names are not stable across
	// boots.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	DeviceName string `json:"deviceName,omitempty"`
	// SerialNumber is the disk's serial number, read from
	// /sys/block/*/device/serial or udev.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`
	// WWN is the disk's World Wide Name, read from udev's ID_WWN.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	WWN string `json:"wwn,omitempty"`
	// Model is the disk's reported model string.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Model string `json:"model,omitempty"`
	// Vendor is the disk's reported vendor string.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Vendor string `json:"vendor,omitempty"`
	// MinSizeGigabytes rejects disks smaller than this size.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinSizeGigabytes *int64 `json:"minSizeGigabytes,omitempty"`
	// MaxSizeGigabytes rejects disks larger than this size.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxSizeGigabytes *int64 `json:"maxSizeGigabytes,omitempty"`
	// Rotational matches on whether the disk is a spinning disk (true) or
	// solid-state (false), read from /sys/block/*/queue/rotational.
	// +optional
	Rotational *bool `json:"rotational,omitempty"`
	// PciePath is the disk's PCIe address, read by resolving
	// /sys/block/<dev>.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	PciePath string `json:"pciePath,omitempty"`
	// HCTL is the disk's SCSI host:channel:target:lun address, for example
	// "0:0:0:0".
	// +kubebuilder:validation:Pattern=`^[0-9]+:[0-9]+:[0-9]+:[0-9]+$`
	// +optional
	HCTL string `json:"hctl,omitempty"`
	// SlotNumber is the disk's NVMe namespace or PCIe slot number, read
	// best-effort from DMI.
	// +optional
	SlotNumber *int32 `json:"slotNumber,omitempty"`
}

// MachineDataImage is one additional, non-OS image deployed alongside (or
// instead of) the machine's OS image in the same live session. Each entry
// resolves against its own TargetDisk hints; all resolved disks across a
// Machine's OS image and its DataImages must be distinct.
type MachineDataImage struct {
	// ImageRef names the Image to deploy. It is typically a non-bootable
	// image (a data disk or a scratch layout), since only the OS image
	// gets a boot entry.
	ImageRef NameRef `json:"imageRef"`
	// TargetDisk selects the disk this image deploys to.
	// +optional
	TargetDisk *TargetDiskHints `json:"targetDisk,omitempty"`
}

// MachineEzioTuning overrides the operator's cluster-wide default EZIO
// tuning for this machine's leecher. All fields are optional; an absent
// field falls back to the operator default.
type MachineEzioTuning struct {
	// CacheSizeMB overrides the EZIO daemon's unified cache size, in
	// megabytes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	CacheSizeMB *int32 `json:"cacheSizeMB,omitempty"`
	// AioThreads overrides the EZIO daemon's async I/O thread count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	AioThreads *int32 `json:"aioThreads,omitempty"`
	// MaxUploads overrides the per-torrent maximum upload slot count
	// (ezio's AddTorrent max_uploads).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +optional
	MaxUploads *int32 `json:"maxUploads,omitempty"`
	// MaxConnections overrides the per-torrent maximum peer connection
	// count (ezio's AddTorrent max_connections).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// Port overrides the EZIO daemon's listen port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`
}

// After-deploy enum values for MachineSpec.AfterDeploy. These apply only
// when the deployment finishes without an OS image to reboot into (a
// dataImages-only deployment on a machine that keeps its existing OS).
const (
	// AfterDeployReboot reboots the machine back to its existing OS. This
	// is the default when AfterDeploy is absent.
	AfterDeployReboot = "Reboot"
	// AfterDeployPowerOff powers the machine off instead of rebooting it.
	AfterDeployPowerOff = "PowerOff"
)

// MachineSpec defines the desired state of Machine. There is no power
// intent field here: power follows the deploy lifecycle and the
// AfterDeploy mechanism only.
type MachineSpec struct {
	// BMC identifies the board management controller that powers and
	// boot-orders this machine. The controller uses it to power the
	// machine on for inspection and deployment, and to set its power
	// state afterward.
	BMC MachineBMC `json:"bmc"`
	// BootMACAddress is the MAC address of the NIC the machine network
	// boots from. The boot config server uses it to find this machine.
	// Normally discovered by inspection; it is mandatory only when the
	// inspect-disable annotation skips inspection, a rule the Machine
	// webhook enforces.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	// +optional
	BootMACAddress string `json:"bootMACAddress,omitempty"`
	// ImageRef names the Image to deploy as this machine's OS. It must be
	// a bootable image. May be absent when the machine deploys only
	// DataImages and keeps its existing OS.
	// +optional
	ImageRef *NameRef `json:"imageRef,omitempty"`
	// DataImages is an ordered list of additional, non-OS images deployed
	// alongside the OS image in the same live session (or by themselves,
	// when ImageRef is absent).
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DataImages []MachineDataImage `json:"dataImages,omitempty"`
	// TargetDisk selects the disk the OS image deploys to. Ignored when
	// ImageRef is absent.
	// +optional
	TargetDisk *TargetDiskHints `json:"targetDisk,omitempty"`
	// SubnetRef names the Subnet this machine network boots through -
	// the broadcast domain whose bootd instance answers this machine's
	// PXE boot. Required: a Machine with no Subnet cannot be
	// network-booted.
	SubnetRef NameRef `json:"subnetRef"`
	// PostHookRefs is an ordered list of PostHook resources attached to
	// this machine. Execution order: the attached Image's own
	// postHookRefs (ImageSpec.PostHookRefs) run first, then this list;
	// within this list, the given order holds. The shipped default hook
	// (kezio-default-finalize) is substituted only when both lists are
	// empty and this machine deploys an OS image.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
	// Params is schemaless input for the attached hooks' templating.
	// Merge order: the deployed Image's own params first, then this
	// field; later entries override earlier ones.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`
	// Ezio overrides the operator's cluster-wide default EZIO tuning for
	// this machine's leecher.
	// +optional
	Ezio *MachineEzioTuning `json:"ezio,omitempty"`
	// AfterDeploy selects what happens when a deployment finishes without
	// an OS image to reboot into. Defaults to Reboot.
	// +kubebuilder:validation:Enum=Reboot;PowerOff
	// +kubebuilder:default=Reboot
	// +optional
	AfterDeploy string `json:"afterDeploy,omitempty"`
}

// MachineFinalizer blocks Machine deletion until the controller's cleanup
// finishes. Today onDelete has no extra cleanup to do: MachineHardware and
// DeployRun are garbage collected through their owner references.
const MachineFinalizer = "kezio.kojuro.date/machine"

// MachineCredentialsSecretLabel marks a Secret as a Machine's BMC
// credentials Secret ("true" is the only value used). The controller sets
// it the first time it claims the Secret, so operators and label selectors
// can find these Secrets without reading every Machine spec.
const MachineCredentialsSecretLabel = "kezio.kojuro.date/bmc-credentials-secret"

// MachineCredentialsSecretFinalizer blocks deletion of a BMC credentials
// Secret while a Machine still references it. The controller adds it the
// first time it claims the Secret and removes it when the Machine is
// deleted. It cannot reuse MachineFinalizer with a "/secret" suffix: a
// Kubernetes finalizer allows only one "/" (an optional prefix before the
// name), and MachineFinalizer already has one.
const MachineCredentialsSecretFinalizer = "kezio.kojuro.date/bmc-credentials"

// Machine operational annotations, read directly off ObjectMeta.Annotations
// - no defaulting or schema covers them.
const (
	// MachineAnnotationPaused, when present with any value, makes the
	// reconciler return immediately: no status writes, no deployer calls,
	// no requeue. This is absolute - it also blocks the delete walk while
	// the Machine has a deletion timestamp.
	MachineAnnotationPaused = "kezio.kojuro.date/paused"
	// MachineAnnotationDetached, when present with any value, stops the
	// controller from calling the deployer (no inspect, provision,
	// deprovision, power, or reboot action) while it keeps updating
	// status: operationalStatus reports MachineOperationalStatusDetached
	// until the annotation is removed, at which point normal operation
	// resumes.
	MachineAnnotationDetached = "kezio.kojuro.date/detached"
	// MachineAnnotationRebootPrefix is the reboot annotation key: either
	// exactly this value (the suffixless, controller-owned form) or this
	// value plus a "-<client>" suffix (a client-held form, for example
	// "kezio.kojuro.date/reboot-fleetctl"). The value is JSON
	// {"mode":"hard"|"soft"}; an empty value or invalid JSON is treated as
	// a soft reboot request for that holder. Any number of
	// suffixless/suffixed reboot annotations may be present at once - the
	// machine reboots while any one is present, hard wins if any holder
	// asks for hard, and the controller clears only the suffixless form
	// once it has acted; suffixed forms are cleared by the client that set
	// them.
	MachineAnnotationRebootPrefix = "kezio.kojuro.date/reboot"
	// MachineAnnotationInspectDisable, set to exactly "true", skips
	// hardware inspection: the controller trusts spec.bootMACAddress
	// instead of discovering it from an inspection boot, so the Machine
	// webhook requires that field non-empty whenever this annotation is
	// "true".
	MachineAnnotationInspectDisable = "kezio.kojuro.date/inspect-disable"
	// MachineAnnotationReInspect, present with any value, asks the
	// controller to redo hardware inspection. Consume-then-delete: the
	// controller removes this annotation and emits a Kubernetes Event
	// reporting what it did (accepted) or why not (refused) on every
	// reconcile that observes it, exactly once. On acceptance the existing
	// MachineHardware is deleted (never patched) and a fresh one is
	// created by the normal inspection walk.
	MachineAnnotationReInspect = "kezio.kojuro.date/re-inspect"
	// MachineAnnotationConfirmStatusLoss, present with any value, releases
	// the status-loss hold (MachineConditionStatusLossHold): an operator
	// has checked the machine and confirms it is safe to resume the normal
	// state walk from Enrolling. Consume-then-delete, like
	// MachineAnnotationReInspect - a stale copy left on the object must
	// never silently wave through a future, unrelated status loss.
	MachineAnnotationConfirmStatusLoss = "kezio.kojuro.date/confirm-status-loss"
)

// MachineRebootMode is the JSON "mode" value inside a reboot annotation.
type MachineRebootMode string

const (
	// MachineRebootModeSoft requests a graceful reboot. It is the default
	// when a reboot annotation's value is empty or fails to parse as JSON.
	MachineRebootModeSoft MachineRebootMode = "soft"
	// MachineRebootModeHard requests a forced reboot. It wins over a soft
	// request from any other holder.
	MachineRebootModeHard MachineRebootMode = "hard"
)

// MachineRebootAnnotationArguments is the JSON payload of one reboot
// annotation.
type MachineRebootAnnotationArguments struct {
	// +optional
	Mode MachineRebootMode `json:"mode,omitempty"`
}

// Machine state enum values for MachineStatus.State. These are the
// top-level states only; provisioning sub-steps are reported through
// conditions, not as top-level states. There is no Error state: an error
// is reported on the OperationalStatus axis instead, so a failure never
// erases where the machine was in the workflow.
const (
	// MachineStateEnrolling is the initial state: the machine is
	// registered but not yet inspected.
	MachineStateEnrolling = "Enrolling"
	// MachineStateInspecting means the controller is powering on the
	// machine to collect its hardware inventory.
	MachineStateInspecting = "Inspecting"
	// MachineStateAvailable means inspection succeeded and the machine is
	// idle, ready for a deployment.
	MachineStateAvailable = "Available"
	// MachineStateProvisioning means a deployment is in progress.
	MachineStateProvisioning = "Provisioning"
	// MachineStateProvisioned means the deployment succeeded and the
	// machine has booted the deployed OS (or, for a dataImages-only
	// deployment, reached its post-deployment power state).
	MachineStateProvisioned = "Provisioned"
	// MachineStateDeprovisioning means the machine has a deletion
	// timestamp and the controller is walking backwards through
	// deprovisioning before it powers the machine off and releases it.
	MachineStateDeprovisioning = "Deprovisioning"
	// MachineStatePoweringOff means the machine has a deletion timestamp,
	// deprovisioning finished (or gave up), and the controller is powering
	// the machine off before releasing it.
	MachineStatePoweringOff = "PoweringOff"
)

// MachineOperationalStatus enum values for MachineStatus.OperationalStatus,
// the axis orthogonal to State: an error, a delay, or a detach never
// changes State.
const (
	// MachineOperationalStatusOK means nothing is wrong.
	MachineOperationalStatusOK = "OK"
	// MachineOperationalStatusError means the machine hit an error in its
	// current state. ErrorType, ErrorCount, and ErrorMessage carry the
	// detail.
	MachineOperationalStatusError = "error"
	// MachineOperationalStatusDelayed means progress is intentionally
	// held back (for example, an unresolved reference), not failed.
	MachineOperationalStatusDelayed = "delayed"
	// MachineOperationalStatusDetached means the detached annotation is
	// stopping the controller from acting on this machine.
	MachineOperationalStatusDetached = "detached"
)

// MachineErrorType classifies the most recent phase failure recorded on
// MachineStatus.ErrorType. The reconciler maps it to the restartOnFailure
// argument passed into the deployer's next Inspect/Provision call for this
// Machine: MachineErrorTypeRestart means the deployer must discard any
// in-progress step state and start the step over; MachineErrorTypeTransient
// means the deployer resumes the step where it left off. A recorded value
// outside this set is possible (a stale value from an older controller
// version, or corrupt data) and must not be assumed to match either
// constant.
type MachineErrorType string

const (
	// MachineErrorTypeTransient means the failure does not invalidate any
	// in-progress step state; the deployer resumes in place.
	MachineErrorTypeTransient MachineErrorType = "Transient"
	// MachineErrorTypeRestart means any in-progress step state may be
	// invalid; the deployer must start the step over from its beginning.
	MachineErrorTypeRestart MachineErrorType = "Restart"
)

// Condition types set on MachineStatus.Conditions.
const (
	// MachineConditionReady summarizes whether the machine is in a
	// stable, usable state.
	MachineConditionReady = "Ready"
	// MachineConditionProgressing reports whether the machine is moving
	// toward a goal state (inspecting or provisioning) right now.
	MachineConditionProgressing = "Progressing"
	// MachineConditionAgentCompatible reports whether the kezio-agent
	// currently polling for a deploy plan advertises a schema version the
	// server accepts.
	MachineConditionAgentCompatible = "AgentCompatible"
	// MachineConditionAgentRegistered reports whether kezio-agent has
	// registered from the live boot environment and reported its
	// hardware inventory (internal/agentserver's POST /agent/register).
	// True is the signal the deployer polls for before it acts on this
	// machine's status.hardware.
	MachineConditionAgentRegistered = "AgentRegistered"
	// MachineConditionStatusLossHold is True while the controller refuses
	// to provision a Machine it found with status never written
	// (status.lastUpdated absent) but spec.imageRef/dataImages already set
	// and DeployRuns already on record for it - the shape a status
	// restore/reset leaves behind on a machine that may already be running
	// a deployed workload. Cleared once MachineAnnotationConfirmStatusLoss
	// is observed.
	MachineConditionStatusLossHold = "StatusLossHold"
)

// MachineCredentialsStatus records one BMC Secret observation: the Secret
// referenced and its resourceVersion at that observation. A resourceVersion
// mismatch against the live Secret means the credentials changed since this
// status was recorded.
type MachineCredentialsStatus struct {
	// SecretRef names the observed BMC credentials Secret.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`
	// ResourceVersion is the named Secret's resourceVersion at the time of
	// this observation.
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// MachineNetBootStatus records the live-boot token most recently minted
// for this machine by the boot config server (GET /boot/grub.cfg-<mac>).
// The token itself is never stored, only its SHA-256 hash and expiry, so
// a leaked status object cannot be replayed to impersonate the machine at
// registration. Each grub.cfg fetch for a machine that currently needs to
// net boot mints a fresh token and overwrites this field, invalidating
// whatever token was here before.
type MachineNetBootStatus struct {
	// TokenHash is the SHA-256 hex digest of the current boot token.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`
	// ExpiresAt is when the current boot token stops being accepted.
	// +optional
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`
}

// MachineAgentSessionStatus records the session credential most recently
// minted for kezio-agent at registration, distinct from the single-use
// net-boot token: the boot token is consumed by registration itself, so
// the agent needs a separate credential to present on every subsequent
// poll. Only the SHA-256 hash and expiry are ever persisted, for the same
// reason as MachineNetBootStatus.
type MachineAgentSessionStatus struct {
	// TokenHash is the SHA-256 hex digest of the current session token.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`
	// ExpiresAt is when the current session token stops being accepted.
	// +optional
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`
}

// MachineStatus defines the observed state of Machine.
type MachineStatus struct {
	// State is the machine's position in the state machine.
	// Deprovisioning/PoweringOff are the delete-only states: the
	// controller enters them only once a deletion timestamp is set, and
	// they never appear on the forward walk.
	// +kubebuilder:validation:Enum=Enrolling;Inspecting;Available;Provisioning;Provisioned;Deprovisioning;PoweringOff
	// +optional
	State string `json:"state,omitempty"`
	// OperationalStatus is the axis orthogonal to State: it reports an
	// error, a delay, or a detach without disturbing State.
	// +kubebuilder:validation:Enum=OK;error;delayed;detached
	// +optional
	OperationalStatus string `json:"operationalStatus,omitempty"`
	// ErrorType classifies the most recent error, when OperationalStatus
	// is "error".
	// +optional
	ErrorType MachineErrorType `json:"errorType,omitempty"`
	// ErrorCount counts consecutive errors since the last success in the
	// current state. The controller uses it to compute backoff.
	// +optional
	ErrorCount int32 `json:"errorCount,omitempty"`
	// ErrorMessage describes the most recent error, when OperationalStatus
	// is "error".
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
	// GoodCredentials records the last BMC Secret observation the
	// controller successfully authenticated with.
	// +optional
	GoodCredentials MachineCredentialsStatus `json:"goodCredentials,omitempty"`
	// TriedCredentials records the BMC Secret observation of the most
	// recent connection attempt, successful or not.
	// +optional
	TriedCredentials MachineCredentialsStatus `json:"triedCredentials,omitempty"`
	// CurrentRunRef names the DeployRun for a deployment in progress.
	// +optional
	CurrentRunRef *NameRef `json:"currentRunRef,omitempty"`
	// LastSuccessfulRunRef names the most recent DeployRun that completed
	// successfully. The provisioning trigger compares the current deploy
	// intent against this run's resolved snapshot.
	// +optional
	LastSuccessfulRunRef *NameRef `json:"lastSuccessfulRunRef,omitempty"`
	// PoweredOn reports the machine's last observed power state.
	// +optional
	PoweredOn *bool `json:"poweredOn,omitempty"`
	// LastUpdated is when this status was last written. Its absence
	// despite a set spec.imageRef is the signal that status was lost
	// (rather than never provisioned).
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// NetBoot records the live-boot token most recently minted for this
	// machine by the boot config server.
	// +optional
	NetBoot *MachineNetBootStatus `json:"netBoot,omitempty"`
	// AgentSession records the session credential most recently minted
	// for kezio-agent at registration, presented on every subsequent
	// poll.
	// +optional
	AgentSession *MachineAgentSessionStatus `json:"agentSession,omitempty"`
	// Conditions report the current state of the machine.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Machine is the Schema for the machines API.
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec,omitempty"`
	Status MachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineList contains a list of Machine.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Machine{}, &MachineList{})
}
