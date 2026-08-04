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
// field falls back to the operator default. Use cases: small-RAM
// machines, CI, or a machine behind a slow link.
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
	// MaxUploads overrides the per-torrent maximum upload slot count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUploads *int32 `json:"maxUploads,omitempty"`
	// MaxConnections overrides the per-torrent maximum peer connection
	// count.
	// +kubebuilder:validation:Minimum=1
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
	// boot-orders this machine.
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
	// NetworkSite selects the network zone (the kezio-bootd instance) this
	// machine network boots through.
	// +optional
	NetworkSite string `json:"networkSite,omitempty"`
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

// MachineProvisionedImage records one deployed image and the disk it was
// written to.
type MachineProvisionedImage struct {
	// ImageRef names the Image that was deployed.
	ImageRef NameRef `json:"imageRef"`
	// TargetDisk is the resolved device path the image was written to.
	// +optional
	TargetDisk string `json:"targetDisk,omitempty"`
}

// MachineProvisioningStatus records what was actually deployed. A
// difference between spec.imageRef and status.provisioning.image (or
// between spec.dataImages and status.provisioning.dataImages) is the
// deployment trigger, compared purely by name: an Image is immutable
// (its spec is create-only), so no generation or content comparison is
// necessary. The controller only updates this status after a successful
// deployment.
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

// MachineStatus defines the observed state of Machine.
type MachineStatus struct {
	// Conditions report the current state of the machine.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// State is the machine's position in the state machine.
	// +kubebuilder:validation:Enum=Enrolling;Inspecting;Available;Provisioning;Provisioned;Error
	// +optional
	State string `json:"state,omitempty"`
	// Provisioning records what was actually deployed.
	// +optional
	Provisioning *MachineProvisioningStatus `json:"provisioning,omitempty"`
	// NetBoot records the live-boot token most recently minted for this
	// machine by the boot config server.
	// +optional
	NetBoot *MachineNetBootStatus `json:"netBoot,omitempty"`
	// Hardware is the inventory the agent reported at registration.
	// +optional
	Hardware *MachineHardwareStatus `json:"hardware,omitempty"`
	// PoweredOn reports the machine's last observed power state.
	// +optional
	PoweredOn *bool `json:"poweredOn,omitempty"`
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
// +kubebuilder:printcolumn:name="NetworkSite",type=string,JSONPath=`.spec.networkSite`
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
