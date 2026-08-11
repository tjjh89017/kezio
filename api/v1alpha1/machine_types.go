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

// MACAddressPattern is the validation pattern for a colon-separated MAC
// address, for example "aa:bb:cc:dd:ee:01".
const MACAddressPattern = `^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`

// BMCAddressPattern is the validation pattern for a BMC address: a URL
// whose scheme selects the driver, for example
// "redfish://10.0.0.10/redfish/v1/Systems/1".
const BMCAddressPattern = `^[a-zA-Z][a-zA-Z0-9+.-]*://.+`

// MachineBMC identifies the board management controller that powers and
// boot-orders the machine, and the credentials used to reach it. Every
// Machine requires one: the controller uses it to power the machine on
// for inspection and deployment, and to control its post-deploy power
// state.
type MachineBMC struct {
	// Address is the BMC endpoint URL. Its scheme selects the driver, for
	// example "redfish://" or "ipmi://".
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9+.-]*://.+`
	Address string `json:"address"`
	// CredentialsSecretRef names the Secret holding the BMC username and
	// password.
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef"`
}

// TargetDiskHints selects the disk a deployment writes to. All fields are
// optional; all given fields must match the same disk (logical AND). The
// controller matches these hints against the agent-reported disk
// inventory; exactly one disk must match, or the deployment stops with an
// error before any write.
type TargetDiskHints struct {
	// DeviceName is the kernel device path, for example "/dev/nvme0n1".
	// This is the last-choice hint: device names are not stable across
	// boots.
	// +optional
	DeviceName string `json:"deviceName,omitempty"`
	// SerialNumber is the disk's serial number, read from
	// /sys/block/*/device/serial or udev.
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`
	// WWN is the disk's World Wide Name, read from udev's ID_WWN.
	// +optional
	WWN string `json:"wwn,omitempty"`
	// Model is the disk's reported model string.
	// +optional
	Model string `json:"model,omitempty"`
	// Vendor is the disk's reported vendor string.
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
	// +optional
	PciePath string `json:"pciePath,omitempty"`
	// HCTL is the disk's SCSI host:channel:target:lun address, for example
	// "0:0:0:0".
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
// field falls back to the operator default (see ResolveEzioTuning). Use
// cases: small-RAM machines, CI, or a machine behind a slow link.
//
// EZIO's BitTorrent transport is plain TCP with no peer connection
// encryption and no uTP: that is fixed by ezio's own contract (it has no
// gRPC field to toggle either), not a tuning choice KEZIO makes, so
// there is no field here for it. Operators who need encrypted transport
// between sites provide it at the network layer instead (see
// config/seeder/README.md's cross-network section).
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
	// (ezio's AddTorrent max_uploads). The operator default is tuned for
	// a routed, cross-site swarm rather than one LAN segment; see
	// config/seeder/README.md's "WAN swarm tuning" section.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +optional
	MaxUploads *int32 `json:"maxUploads,omitempty"`
	// MaxConnections overrides the per-torrent maximum peer connection
	// count (ezio's AddTorrent max_connections). A WAN-routed swarm
	// stays healthy with a small per-torrent cap (around 8-10): unlike a
	// single LAN segment, a large fan-out here does not find more useful
	// peers, only more idle connections across the routed links. The
	// operator default already reflects this; raise it only for a
	// same-LAN deployment with many machines that benefit from wider
	// fan-out.
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

// MachineSpec defines the desired state of Machine.
type MachineSpec struct {
	// BMC identifies the board management controller that powers and
	// boot-orders this machine. The controller uses it to power the
	// machine on for inspection and deployment, and to set its power
	// state afterward.
	BMC MachineBMC `json:"bmc"`
	// BootMACAddress is the MAC address of the NIC the machine network
	// boots from. The boot config server uses it to find this machine.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	BootMACAddress string `json:"bootMACAddress"`
	// Online is the desired power state: true keeps the machine powered
	// on and available for provisioning; false powers it down. Defaults
	// to true.
	// +kubebuilder:default=true
	// +optional
	Online *bool `json:"online,omitempty"`
	// ImageRef names the Image to deploy as this machine's OS. It must be
	// a bootable image. May be absent when the machine deploys only
	// DataImages and keeps its existing OS.
	// +optional
	ImageRef *NameRef `json:"imageRef,omitempty"`
	// DataImages is an ordered list of additional, non-OS images deployed
	// alongside the OS image in the same live session (or by themselves,
	// when ImageRef is absent).
	// +optional
	DataImages []MachineDataImage `json:"dataImages,omitempty"`
	// TargetDisk selects the disk the OS image deploys to. Ignored when
	// ImageRef is absent.
	// +optional
	TargetDisk *TargetDiskHints `json:"targetDisk,omitempty"`
	// SubnetRef names the Subnet this machine network boots through - the
	// broadcast domain whose bootd instance answers this machine's PXE
	// boot. The Subnet's own SiteRef derives which Site the machine
	// belongs to (that Site's namespace/name is its stable identity -
	// see Site's doc comment); this field never names a Site directly, so
	// there is no way for a machine to declare a site inconsistent with
	// the segment it actually boots from. It does not select a seeder:
	// the deploy plan always announces at the single central tracker
	// (SEEDER_TRACKER_URL), and BitTorrent's own peer discovery through
	// that shared tracker is what lets this machine's leecher find a
	// site-local seeder when one is deployed - no per-site routing field
	// or lookup is needed for that. See config/seeder/README.md's
	// "Per-site seeders" section.
	//
	// Required: a Machine with no Subnet cannot be network-booted.
	SubnetRef NameRef `json:"subnetRef"`
	// PostHookRefs is an ordered list of PostHook resources attached to
	// this machine. Execution order: the attached Image's own
	// postHookRefs run first, then this list; within this list, the given
	// order holds.
	// +optional
	PostHookRefs []NameRef `json:"postHookRefs,omitempty"`
	// Params is schemaless input for the attached hooks' templating.
	// Merge order: the deployed Image's own params first, then this
	// field; later entries override earlier ones.
	//
	// A pointer, not a value: see ImageSpec.Params's doc comment (same
	// field shape, same reason) for why a value-typed
	// apiextensionsv1.JSON round-trips an absent Params as an explicit
	// JSON null through the defaulting webhook.
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

// MergeEzioTuning resolves the EZIO tuning precedence: cluster-wide
// default first, then this machine's own spec.ezio override. Every field
// override sets falls back to the matching field in base; a field
// neither sets stays nil, meaning "use ezio's/the launcher's own
// built-in default" at the point that consumes it (see
// internal/seeder.ResolveMaxUploads/ResolveMaxConnections for the
// per-torrent knobs). Either argument may be nil; a nil base with a nil
// override returns nil, meaning nothing to apply.
func MergeEzioTuning(base, override *MachineEzioTuning) *MachineEzioTuning {
	if base == nil && override == nil {
		return nil
	}
	merged := &MachineEzioTuning{}
	if base != nil {
		*merged = *base
	}
	if override != nil {
		if override.CacheSizeMB != nil {
			merged.CacheSizeMB = override.CacheSizeMB
		}
		if override.AioThreads != nil {
			merged.AioThreads = override.AioThreads
		}
		if override.MaxUploads != nil {
			merged.MaxUploads = override.MaxUploads
		}
		if override.MaxConnections != nil {
			merged.MaxConnections = override.MaxConnections
		}
		if override.Port != nil {
			merged.Port = override.Port
		}
	}
	return merged
}

// EffectiveOnline returns the machine's desired power state, treating a
// nil Online field as true (the default).
func (s MachineSpec) EffectiveOnline() bool {
	if s.Online == nil {
		return true
	}
	return *s.Online
}

// EffectiveAfterDeploy returns the after-deploy action to take, treating
// an empty AfterDeploy field as AfterDeployReboot (the default).
func (s MachineSpec) EffectiveAfterDeploy() string {
	if s.AfterDeploy == "" {
		return AfterDeployReboot
	}
	return s.AfterDeploy
}

// MachineConditionAgentRegistered reports whether the kezio-agent running
// on the machine's live boot environment has registered for the current
// inspection cycle and delivered its hardware inventory. Status=True once
// a registration has landed since the machine most recently entered the
// Inspecting state; Status=False (or the condition being absent) means no
// registration has landed yet for this cycle, which is the signal the
// agent-driven Deployer's Inspect phase polls on. A real Deployer's
// Register phase resets this condition to False (and clears
// status.hardware) every time it runs, so a stale registration from a
// previous inspection cycle can never be mistaken for a fresh one.
const MachineConditionAgentRegistered = "AgentRegistered"

// MachineConditionProvisioningProgress reports the live status of a
// deploy in progress: kezio-agent posts a snapshot of it to
// POST /agent/machines/<name>/progress (internal/agentserver,
// internal/agentapi.ProgressRequest) as it writes each partition and
// finalizes the deploy, and the controller mirrors the latest snapshot
// here as Reason/Message rather than a structured per-partition field, to
// keep this cheap to store and to watch with `kubectl get -w`. Reason is
// one of the agentapi.DeployStep* values (WritingPartitionTable,
// WritingContent, Finalizing, and the terminal RebootingToDisk that
// means the deploy succeeded) when the agent reports one, or falls back
// to an agentapi.PartitionPhase* value describing the furthest-along
// partition currently being worked, for an older agent or a report sent
// before the whole-plan step machine starts. Message is a short
// per-partition summary, or the agent's own step detail when it supplied
// one. Status is True while a deploy is in progress and is otherwise
// left alone (there is nothing meaningful to reset it to between
// deploys; the next deploy's first progress report overwrites Message
// and Reason anyway).
const MachineConditionProvisioningProgress = "ProvisioningProgress"

// MachineConditionAgentCompatible reports whether the kezio-agent
// currently polling GET /agent/machines/<name>/next (internal/agentserver)
// advertises a plan schema version (agentapi.AgentSchemaVersionHeader)
// matching the server's own agentapi.PlanSchemaVersion. Status=False
// (reason "AgentIncompatible") means the server is withholding a
// DeployPlan from an agent whose advertised version is absent (every
// agent built before this header existed) or does not match - the
// Message names both versions - so the Machine holds instead of handing
// a plan to an agent that would misread it; this is the guard against
// the exact silent-data-destruction failure a wire-shape change without
// a version bump caused once (an old agent decoded a new plan, found no
// legacy inline torrent bytes, and mkfs'd every content partition).
// Status=True means the current poller's version matches and plan
// handout proceeds normally. Absent means no agent has polled yet.
const MachineConditionAgentCompatible = "AgentCompatible"

// Machine state enum values for MachineStatus.State. These are the
// top-level states only; provisioning sub-steps (powering on, net
// booting, writing the image, and so on) are reported through a
// condition's Reason, not as top-level states.
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
	// MachineStateError means the machine hit an error that stopped its
	// current step. status.errorMessage has the detail; the controller
	// retries with exponential backoff and jitter.
	MachineStateError = "Error"
)

// MachineProvisionedImage records one image and the disk resolved to
// receive it: the disk matched from this image's targetDisk hints against
// the reported hardware inventory. The controller records this before the
// image is written, so TargetDisk names the intended disk from the start
// of Provisioning, not only once the deployment finishes.
type MachineProvisionedImage struct {
	// ImageRef names the Image being deployed.
	ImageRef NameRef `json:"imageRef"`
	// TargetDisk is the resolved device path the image is written to.
	// +optional
	TargetDisk string `json:"targetDisk,omitempty"`
}

// MachineProvisioningStatus records the resolved deployment: which image
// goes to which disk. The controller writes it once, at the start of the
// Provisioning phase, after it resolves each image's target-disk hints
// against the reported hardware inventory and before it attempts any
// deploy action; a successful deployment reconfirms the same disks. A
// difference between spec.imageRef and status.provisioning.image (or
// between spec.dataImages and status.provisioning.dataImages) is the
// deployment trigger, compared purely by name: an Image is immutable
// (its spec is create-only), so no generation or content comparison is
// necessary.
type MachineProvisioningStatus struct {
	// Image records the deployed OS image and its target disk.
	// +optional
	Image *MachineProvisionedImage `json:"image,omitempty"`
	// DataImages records each deployed non-OS image and its target disk.
	// +optional
	DataImages []MachineProvisionedImage `json:"dataImages,omitempty"`
}

// MachineHardwareDisk records one disk from the agent-reported inventory.
type MachineHardwareDisk struct {
	// DeviceName is the kernel device path, for example "/dev/nvme0n1".
	DeviceName string `json:"deviceName"`
	// SerialNumber is the disk's serial number.
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`
	// WWN is the disk's World Wide Name.
	// +optional
	WWN string `json:"wwn,omitempty"`
	// Model is the disk's reported model string.
	// +optional
	Model string `json:"model,omitempty"`
	// Vendor is the disk's reported vendor string.
	// +optional
	Vendor string `json:"vendor,omitempty"`
	// SizeBytes is the disk's size, from blockdev --getsize64.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Rotational reports whether the disk is a spinning disk.
	// +optional
	Rotational *bool `json:"rotational,omitempty"`
	// PciePath is the disk's PCIe address.
	// +optional
	PciePath string `json:"pciePath,omitempty"`
	// HCTL is the disk's SCSI host:channel:target:lun address.
	// +optional
	HCTL string `json:"hctl,omitempty"`
	// SlotNumber is the disk's NVMe namespace or PCIe slot number.
	// +optional
	SlotNumber *int32 `json:"slotNumber,omitempty"`
}

// MachineHardwareNIC records one network interface from the
// agent-reported inventory.
type MachineHardwareNIC struct {
	// Name is the interface name, for example "eth0".
	Name string `json:"name"`
	// MACAddress is the interface's MAC address.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	// +optional
	MACAddress string `json:"macAddress,omitempty"`
}

// MachineHardwareStatus is the hardware inventory the agent reports at
// registration.
type MachineHardwareStatus struct {
	// Disks lists the machine's disks.
	// +optional
	Disks []MachineHardwareDisk `json:"disks,omitempty"`
	// Nics lists the machine's network interfaces.
	// +optional
	Nics []MachineHardwareNIC `json:"nics,omitempty"`
	// MemoryBytes is the machine's total memory.
	// +optional
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	// CPUCount is the machine's logical CPU count.
	// +optional
	CPUCount int32 `json:"cpuCount,omitempty"`
}

// MachineNetBootStatus records the live-boot token most recently minted
// for this machine by the boot config server (see
// GET /boot/grub.cfg-<mac>). The token itself is never stored, only its
// SHA-256 hash and expiry: a leaked status object (or an etcd backup)
// then cannot be replayed to impersonate the machine when it registers.
// Each grub.cfg fetch for a machine that currently needs to net boot
// mints a fresh token and overwrites this field, invalidating whatever
// token was here before - firmware PXE/HTTP fetches retry on their own,
// and only the token embedded in the config the firmware actually used
// needs to stay valid.
type MachineNetBootStatus struct {
	// TokenHash is the SHA-256 hex digest of the current boot token.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`
	// ExpiresAt is when the current boot token stops being accepted.
	// +optional
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`
}

// MachineAgentSessionStatus records the session credential most recently
// minted for kezio-agent by POST /agent/register (internal/agentserver),
// distinct from the single-use net-boot token: the boot token is
// consumed by registration itself, so once registration succeeds the
// agent needs a separate credential to present on every subsequent GET
// .../next poll. The same never-store-the-plaintext discipline as
// MachineNetBootStatus applies here and for the same reason: only the
// SHA-256 hash and expiry are ever persisted, so a leaked status object
// cannot be replayed to poll as the machine.
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
	// Conditions report the current state of the machine.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// State is the machine's position in the state machine.
	// +kubebuilder:validation:Enum=Enrolling;Inspecting;Available;Provisioning;Provisioned;Error
	// +optional
	State string `json:"state,omitempty"`
	// InspectingSince records when the machine most recently entered the
	// Inspecting state (set alongside State transitioning to Inspecting).
	// The controller uses it to detect a machine stuck waiting for
	// kezio-agent to register - past a threshold, it reports the machine
	// as failed (MachineStateError) rather than continuing to wait
	// silently or taking an automatic recovery action, since nothing
	// observable at that layer can tell a machine that never booted
	// apart from one whose agent is alive and still working. It is only
	// meaningful while State is Inspecting.
	// +optional
	InspectingSince *metav1.Time `json:"inspectingSince,omitempty"`
	// Provisioning records what was actually deployed.
	// +optional
	Provisioning *MachineProvisioningStatus `json:"provisioning,omitempty"`
	// NetBoot records the live-boot token most recently minted for this
	// machine by the boot config server.
	// +optional
	NetBoot *MachineNetBootStatus `json:"netBoot,omitempty"`
	// AgentSession records the session credential most recently minted
	// for kezio-agent at registration, presented on every subsequent
	// GET .../next poll.
	// +optional
	AgentSession *MachineAgentSessionStatus `json:"agentSession,omitempty"`
	// Hardware is the inventory the agent reported at registration.
	// +optional
	Hardware *MachineHardwareStatus `json:"hardware,omitempty"`
	// PoweredOn reports the machine's last observed power state.
	// +optional
	PoweredOn *bool `json:"poweredOn,omitempty"`
	// GracefulPowerOffSince records when the controller first issued a
	// graceful power-off that the BMC accepted while the machine kept
	// reporting itself powered on. It survives across reconciles because
	// nothing else can tell a shutdown still in progress apart from one
	// nothing will ever act on: a machine parked in its firmware boot menu
	// runs no OS to receive the ACPI request, so it acknowledges the
	// command and stays on indefinitely. Past a threshold the controller
	// escalates to a forced power-off (see reconcilePower). Cleared as
	// soon as the observed power state agrees with spec.online again.
	// +optional
	GracefulPowerOffSince *metav1.Time `json:"gracefulPowerOffSince,omitempty"`
	// ErrorCount counts consecutive errors since the last success. The
	// controller uses it to compute exponential backoff with jitter.
	// +optional
	ErrorCount int32 `json:"errorCount,omitempty"`
	// ErrorMessage describes the most recent error, when ErrorCount > 0.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezm,machine}
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Online",type=boolean,JSONPath=`.spec.online`
// +kubebuilder:printcolumn:name="PoweredOn",type=boolean,JSONPath=`.status.poweredOn`
// +kubebuilder:printcolumn:name="BootMAC",type=string,JSONPath=`.spec.bootMACAddress`
// +kubebuilder:printcolumn:name="Subnet",type=string,JSONPath=`.spec.subnetRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
