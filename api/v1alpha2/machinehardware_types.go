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

// MachineHardwareDisk records one disk from the agent-reported inventory.
// Field set matches TargetDiskHints so a hint can be matched against an
// entry here with no unit conversion.
type MachineHardwareDisk struct {
	// DeviceName is the kernel device path, for example "/dev/nvme0n1".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	DeviceName string `json:"deviceName"`
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
	// SizeBytes is the disk's size, from blockdev --getsize64.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Rotational reports whether the disk is a spinning disk (true) or
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

// MachineHardwareNIC records one network interface from the
// agent-reported inventory.
type MachineHardwareNIC struct {
	// Name is the interface name, for example "eth0".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// MACAddress is the interface's MAC address.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	// +optional
	MACAddress string `json:"macAddress,omitempty"`
}

// MachineHardwareSpec is the hardware inventory the agent reports at
// registration. Every field is optional: a MachineHardware object starts
// empty and the controller fills it in once inspection completes.
type MachineHardwareSpec struct {
	// Disks lists the machine's disks.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Disks []MachineHardwareDisk `json:"disks,omitempty"`
	// Nics lists the machine's network interfaces.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Nics []MachineHardwareNIC `json:"nics,omitempty"`
	// MemoryBytes is the machine's total memory.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	// CPUCount is the machine's logical CPU count.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CPUCount int32 `json:"cpuCount,omitempty"`
}

// +kubebuilder:object:root=true

// MachineHardware is the Schema for the machinehardwares API. It carries
// no status: it is itself the observed hardware inventory, owned by and
// name-aligned with its Machine.
type MachineHardware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MachineHardwareSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// MachineHardwareList contains a list of MachineHardware.
type MachineHardwareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineHardware `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MachineHardware{}, &MachineHardwareList{})
}
