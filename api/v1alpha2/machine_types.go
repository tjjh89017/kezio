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
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	BootMACAddress string `json:"bootMACAddress"`
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
	// postHookRefs run first, then this list; within this list, the
	// given order holds.
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
	// ClaimRef names the MachineClaim this machine is bound to. It lives
	// in spec, not status, so the exactly-one-binding arbiter survives a
	// status loss. Written by the claim controller and cleared by an
	// operator during reclaim; nothing else touches it.
	// +optional
	ClaimRef *NameRef `json:"claimRef,omitempty"`
}

// MachineStatus defines the observed state of Machine.
type MachineStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
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
