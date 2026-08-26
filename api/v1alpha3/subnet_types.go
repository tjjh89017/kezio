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

// DHCP mode enum values for SubnetDHCP.Mode.
const (
	// SubnetDHCPModeProxy runs bootd's dnsmasq as proxyDHCP only: the
	// segment's existing production DHCP server keeps sole ownership of
	// leases, and bootd answers only the PXE portion of the exchange.
	SubnetDHCPModeProxy = "proxy"
	// SubnetDHCPModeLease runs bootd's dnsmasq as the segment's own DHCP
	// lease authority, for a segment with no DHCP server of its own.
	SubnetDHCPModeLease = "lease"
)

// SubnetDHCP configures the DHCP behavior of the bootd instance this
// Subnet's BootdNetworkRef backs.
// +kubebuilder:validation:XValidation:rule="has(self.leaseRangeStart) == has(self.leaseRangeEnd)",message="leaseRangeStart and leaseRangeEnd must be set together"
// size(), not a comparison against an empty string literal: the quoting
// that would need survives neither this marker's own quotes nor an
// editor with smart quotes turned on.
// +kubebuilder:validation:XValidation:rule="self.mode == 'lease' ? has(self.gateway) : (!has(self.gateway) || size(self.gateway) == 0)",message="gateway is required in lease mode (use an empty string for a segment with no exit); in proxy mode it must be absent or empty, because bootd is not the DHCP server there and cannot hand out a router option the segment's own DHCP server owns"
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
	// Gateway is the router option (DHCP option 3) bootd hands out when
	// bootd is itself the segment's DHCP server. An IPv4 address is the
	// segment's router - set it whenever the Site's seeder or tracker
	// sits on another Subnet, since it is the only thing that tells a
	// machine how to get there. The empty string hands out no router
	// option, for a segment with no exit.
	//
	// Required in SubnetDHCPModeLease, where bootd owns the lease. Being
	// required is the point: left optional, dnsmasq fills the gap by
	// advertising bootd's own address, and bootd is a pod that forwards
	// nothing, so machines would silently receive a default route into a
	// black hole - a defect that only surfaces once a Site has a second
	// segment, far from its cause. An empty string is a decision; an
	// absent field was an oversight, and lease mode no longer admits one.
	//
	// In SubnetDHCPModeProxy bootd is not the DHCP server, so it hands
	// out no router option either way: absent and empty are both accepted
	// and mean the same thing here, while a non-empty address is rejected
	// rather than ignored. Ignoring it would let an operator write an
	// address, believe machines receive it, and discover otherwise much
	// later.
	// +kubebuilder:validation:Pattern=`^$|^(\d{1,3}\.){3}\d{1,3}$`
	// +optional
	Gateway *string `json:"gateway,omitempty"`
	// LeaseTime is the DHCP lease duration bootd hands out in
	// SubnetDHCPModeLease, rendered as dnsmasq's dhcp-range lease time (for
	// example "30m" or "1h"). Ignored in SubnetDHCPModeProxy, where the
	// segment's own DHCP server owns lease lifetime.
	//
	// Left unset defaults to 30 minutes. Set at least 2 minutes: dnsmasq
	// renews well before expiry, but a shorter lease turns any bootd outage
	// past its duration into lost addresses for every machine mid-deploy
	// (see the Subnet doc's operational note).
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="self >= duration('2m')",message="leaseTime must be at least 2 minutes"
	// +optional
	LeaseTime *metav1.Duration `json:"leaseTime,omitempty"`
}

// SubnetSpec defines the desired state of Subnet.
//
// A Subnet denotes one broadcast domain. Its boot half (BootdServerIP,
// BootdNetworkRef, DHCP) is optional as a group: either all three are set,
// giving this Subnet a bootd Deployment - bootd binds to it directly
// (BootdNetworkRef), and BootdServerIP is the address firmware caches
// mid-boot as its PXE next-server/TFTP target on that segment - or none of
// them are, and this Subnet carries no machines at all. The data plane -
// the routed, cross-subnet network BitTorrent seeding needs - is a
// distinct concern expressed through SeederNetworkRef, and is what a
// boot-half-less Subnet must carry instead: a Subnet declaring neither
// half hosts nothing and is rejected at admission.
//
// BootdServerIP is explicit and pinned, never allocated by an IPAM plugin:
// it must stay stable for the pod's whole lifetime, unlike a seeder pod's
// address, which BitTorrent's tracker-based peer discovery tolerates
// changing between seeder Deployments.
// +kubebuilder:validation:XValidation:rule="has(self.bootdServerIP) == has(self.bootdNetworkRef) && has(self.bootdNetworkRef) == has(self.dhcp)",message="bootdServerIP, bootdNetworkRef and dhcp must be set together or all left unset"
// +kubebuilder:validation:XValidation:rule="has(self.bootdServerIP) || has(self.seederNetworkRef)",message="a Subnet must declare a boot half (bootdServerIP/bootdNetworkRef/dhcp) or seederNetworkRef; one with neither hosts nothing"
type SubnetSpec struct {
	// SiteRef names the Site this Subnet belongs to. The referenced Site
	// must in fact be able to route to this Subnet for the reference to
	// be meaningful. A dangling reference does not withhold this Subnet's
	// own bootd Deployment, but it does block Site-scoped features that
	// need the Site to exist.
	SiteRef NameRef `json:"siteRef"`
	// CIDR is this Subnet's IPv4 network, for example "192.0.2.0/24".
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`
	CIDR string `json:"cidr"`
	// BootdServerIP is bootd's own IPv4 address on this Subnet - the
	// address firmware reads back as the PXE boot server and (absent an
	// override) the TFTP next-server. Explicit and pinned, never
	// IPAM-allocated. Part of the boot half: set together with
	// BootdNetworkRef and DHCP, or left unset with both of them - a
	// Subnet with none of the three hosts no machines and gets no bootd
	// Deployment.
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}$`
	// +optional
	BootdServerIP string `json:"bootdServerIP,omitempty"`
	// BootdNetworkRef names the NetworkAttachmentDefinition the bootd
	// Deployment for this Subnet attaches through. Its namespace must
	// carry pod-security.kubernetes.io/enforce=privileged, since bootd
	// needs NET_ADMIN. Part of the boot half; see BootdServerIP.
	// +optional
	BootdNetworkRef *NameRef `json:"bootdNetworkRef,omitempty"`
	// SeederNetworkRef names the NetworkAttachmentDefinition seeder pods
	// attach through when this Subnet is a Site's designated seeder
	// Subnet. Optional: absent means this Subnet does not host seeders.
	// It may name the same NAD as BootdNetworkRef, but BootdServerIP must
	// never fall inside that NAD's IPAM range. A Subnet with no boot half
	// must set this field - it is the only thing such a Subnet can host.
	// +optional
	SeederNetworkRef *NameRef `json:"seederNetworkRef,omitempty"`
	// NodeSelector constrains bootd (and, when this Subnet hosts seeders,
	// seeder) pods onto nodes actually attached to this broadcast domain.
	// Empty means unconstrained - correct for a single-node lab, where
	// every node is on every segment.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// DHCP configures bootd's DHCP behavior on this Subnet. Part of the
	// boot half; see BootdServerIP.
	// +optional
	DHCP *SubnetDHCP `json:"dhcp,omitempty"`
}

// HasBootPlane reports whether this Subnet declares the boot half
// (BootdServerIP/BootdNetworkRef/DHCP). The CRD schema's XValidation rule
// guarantees the group is all-or-nothing, so checking DHCP alone is
// sufficient once the object is admitted.
func (s SubnetSpec) HasBootPlane() bool {
	return s.DHCP != nil
}

// Condition types set on SubnetStatus.Conditions.
const (
	// SubnetConditionReady reflects whether this Subnet's bootd
	// Deployment has become available.
	SubnetConditionReady = "Ready"
	// SubnetConditionValid reports whether this Subnet passed both
	// schema-level validation (enforced at admission, so always True for
	// an admitted object) and the SubnetReconciler's own checks against
	// its referenced NetworkAttachmentDefinitions and DHCP configuration
	// (internal/nadvalidate, internal/subnetvalidate). False names the
	// first failing check as Reason.
	SubnetConditionValid = "Valid"
	// SubnetConditionDHCPPoolExhausted is True while this Subnet's
	// lease-mode DHCP address pool has no free address left for a new
	// reservation (internal/subnetdhcp.Reserve returning
	// ErrPoolExhausted). Always False for a proxy-mode Subnet, or a
	// lease-mode Subnet with a free address. A Machine that hit
	// exhaustion is held at deployer.Delayed until an address frees up -
	// through a completed deploy, a deleted Machine, or a widened lease
	// range - and this condition is what an operator reads to tell that
	// apart from an ordinary transient delay.
	SubnetConditionDHCPPoolExhausted = "DHCPPoolExhausted"
)

// DHCPReservation is one boot-scoped DHCP address reservation: a fixed
// address handed to exactly one Machine's boot MAC for as long as that
// Machine is net-booting or deployed, in a lease-mode Subnet. Allocated
// by the deployer at net-boot arm time (the same moment it mints that
// boot's registration token) and released once the deploy step
// completes, the Machine is deleted, or its bootMACAddress/subnetRef
// changes - see internal/subnetdhcp.
type DHCPReservation struct {
	// Address is the reserved IPv4 address, inside the Subnet's lease
	// range.
	Address string `json:"address"`
	// Machine names the Machine this reservation was allocated for, in
	// the Subnet's own namespace.
	Machine string `json:"machine"`
	// MAC is the Machine's normalized boot MAC address at the moment
	// this reservation was allocated.
	MAC string `json:"mac"`
	// Since is when this reservation was allocated.
	Since metav1.Time `json:"since"`
}

// SubnetDHCPStatus reports the boot-scoped DHCP address reservations
// bootd's dnsmasq hostsfile renders for this Subnet, in lease mode.
// Always empty for a proxy-mode Subnet: proxyDHCP never assigns
// addresses, so there is nothing to reserve.
type SubnetDHCPStatus struct {
	// Reservations is the current address reservation table. The manager
	// (internal/deployer, internal/subnetdhcp) is its only writer;
	// bootd only ever reads it.
	// +optional
	// +listType=atomic
	Reservations []DHCPReservation `json:"reservations,omitempty"`
	// Revision changes whenever Reservations changes - a digest of the
	// sorted table (internal/subnetdhcp.Revision), not a counter, so two
	// managers computing it from the same table always agree. bootd
	// compares its own AppliedRevision against this field to decide
	// whether its rendered hostsfile is stale.
	// +optional
	Revision string `json:"revision,omitempty"`
	// AppliedRevision is the Revision bootd's dnsmasq hostsfile last
	// actually rendered and SIGHUPed dnsmasq for - written by bootd
	// itself, never by the manager. A Machine's deployer waits for
	// AppliedRevision to catch up to Revision before it powers the
	// machine on for net boot, so it never races dnsmasq's own reload.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`
}

// SubnetStatus defines the observed state of Subnet.
type SubnetStatus struct {
	// Conditions report the current state of the Subnet, including
	// SubnetConditionReady and SubnetConditionValid. Every write carries
	// observedGeneration (metav1.Condition's own field) set to the
	// generation the write observed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// DHCP reports the boot-scoped DHCP address reservations bootd
	// renders for this Subnet. Absent for a Subnet that has never held a
	// reservation (every proxy-mode Subnet, and a lease-mode Subnet with
	// none allocated yet).
	// +optional
	DHCP *SubnetDHCPStatus `json:"dhcp,omitempty"`
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
