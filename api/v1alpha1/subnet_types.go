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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IPv4AddressPattern is the validation pattern for a bare IPv4 address,
// for example "192.0.2.2".
const IPv4AddressPattern = `^(\d{1,3}\.){3}\d{1,3}$`

// IPv4CIDRPattern is the validation pattern for an IPv4 CIDR block, for
// example "192.0.2.0/24".
const IPv4CIDRPattern = `^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`

// DHCP mode enum values for SubnetDHCP.Mode.
const (
	// SubnetDHCPModeProxy runs bootd's dnsmasq as proxyDHCP only: the
	// segment's existing production DHCP server keeps sole ownership of
	// leases, and bootd answers only the PXE portion of the exchange.
	// Maps to a false BOOTD_LEASE_MODE.
	SubnetDHCPModeProxy = "proxy"
	// SubnetDHCPModeLease runs bootd's dnsmasq as the segment's own DHCP
	// lease authority, for a segment with no DHCP server of its own.
	// Maps to a true BOOTD_LEASE_MODE.
	SubnetDHCPModeLease = "lease"
)

// SubnetDHCP configures the DHCP behavior of the bootd instance this
// Subnet's BootdNetworkRef backs.
// +kubebuilder:validation:XValidation:rule="has(self.leaseRangeStart) == has(self.leaseRangeEnd)",message="leaseRangeStart and leaseRangeEnd must be set together"
type SubnetDHCP struct {
	// Mode selects proxyDHCP (defer to an existing DHCP server on the
	// segment) or lease (bootd's dnsmasq becomes the segment's own DHCP
	// authority).
	// +kubebuilder:validation:Enum=proxy;lease
	Mode string `json:"mode"`
	// LeaseRangeStart and LeaseRangeEnd optionally bound the DHCP lease
	// range rendered in SubnetDHCPModeLease. Leaving both unset
	// auto-derives the range from CIDR (its first and last host
	// addresses). Ignored unless Mode is SubnetDHCPModeLease.
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}$`
	// +optional
	LeaseRangeStart string `json:"leaseRangeStart,omitempty"`
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}$`
	// +optional
	LeaseRangeEnd string `json:"leaseRangeEnd,omitempty"`
}

// SubnetSpec defines the desired state of Subnet.
//
// A Subnet denotes one broadcast domain on the boot/provisioning plane:
// bootd binds to it directly (BootdNetworkRef), and BootdServerIP is the
// address firmware caches mid-boot as its PXE next-server/TFTP target on
// that segment. The data plane - the routed, cross-subnet network
// BitTorrent seeding needs - is a distinct concern expressed through
// SeederNetworkRef and resolved at the Site level, not the Subnet level.
//
// BootdServerIP is explicit and pinned, never allocated by an IPAM
// plugin: it must stay stable for the pod's whole lifetime, unlike a
// seeder pod's address, which BitTorrent's tracker-based peer discovery
// tolerates changing between seeder Deployments.
type SubnetSpec struct {
	// SiteRef names the Site this Subnet belongs to. The referenced Site
	// must in fact be able to route to this Subnet for the reference to
	// be meaningful. A dangling reference does not withhold this
	// Subnet's own bootd Deployment, but it does block Site-scoped
	// features that need the Site to exist.
	SiteRef NameRef `json:"siteRef"`
	// CIDR is this Subnet's IPv4 network, for example "192.0.2.0/24".
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`
	CIDR string `json:"cidr"`
	// BootdServerIP is bootd's own IPv4 address on this Subnet - the
	// address firmware reads back as the PXE boot server and (absent an
	// override) the TFTP next-server. Explicit and pinned, never
	// IPAM-allocated.
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}$`
	BootdServerIP string `json:"bootdServerIP"`
	// BootdNetworkRef names the NetworkAttachmentDefinition the bootd
	// Deployment for this Subnet attaches through. Its namespace must
	// carry pod-security.kubernetes.io/enforce=privileged, since bootd
	// needs NET_ADMIN.
	BootdNetworkRef NameRef `json:"bootdNetworkRef"`
	// SeederNetworkRef names the NetworkAttachmentDefinition seeder pods
	// attach through when this Subnet is a Site's designated seeder
	// Subnet (SiteSpec.SeederSubnetRef). Optional: absent means this
	// Subnet does not host seeders. It may name the same NAD as
	// BootdNetworkRef, but BootdServerIP must never fall inside that
	// NAD's IPAM range.
	// +optional
	SeederNetworkRef *NameRef `json:"seederNetworkRef,omitempty"`
	// NodeSelector constrains bootd (and, when this Subnet hosts
	// seeders, seeder) pods onto nodes actually attached to this
	// broadcast domain. Empty means unconstrained - correct for a
	// single-node lab, where every node is on every segment.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// DHCP configures bootd's DHCP behavior on this Subnet.
	DHCP SubnetDHCP `json:"dhcp"`
}

// SubnetStatus defines the observed state of Subnet.
type SubnetStatus struct {
	// Conditions report the current state of the Subnet, including
	// ConditionReady, which reflects whether this Subnet's bootd
	// Deployment has become available.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezn,subnet}
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=`.spec.siteRef.name`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.spec.cidr`
// +kubebuilder:printcolumn:name="BootdServerIP",type=string,JSONPath=`.spec.bootdServerIP`
// +kubebuilder:printcolumn:name="DHCPMode",type=string,JSONPath=`.spec.dhcp.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Subnet is the Schema for the subnets API.
type Subnet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubnetSpec   `json:"spec,omitempty"`
	Status SubnetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SubnetList contains a list of Subnet.
type SubnetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Subnet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Subnet{}, &SubnetList{})
}
