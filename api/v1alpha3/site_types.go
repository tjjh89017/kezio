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

// SiteTracker configures this Site's local BitTorrent tracker.
// +kubebuilder:validation:XValidation:rule="!(has(self.ip) && has(self.externalURL))",message="tracker.ip and tracker.externalURL are mutually exclusive"
type SiteTracker struct {
	// IP is this Site's tracker address on its seeding Subnet (see
	// SiteSpec.SeederSubnetRef). Explicit and pinned, never
	// IPAM-allocated: the same reason Subnet.spec.bootdServerIP is -
	// this value is baked into every .torrent this Site's seeder
	// serves, so it must stay stable across tracker pod restarts.
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}$`
	// +optional
	IP string `json:"ip,omitempty"`
	// ExternalURL points at a tracker the operator already runs outside
	// this Site. kezio creates no tracker Deployment for this Site and
	// does not check the address.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	ExternalURL string `json:"externalURL,omitempty"`
}

// SiteSpec defines the desired state of Site.
//
// A Site is a maximal routable domain: every Subnet that belongs to one
// Site can route to every other Subnet of that Site, with no VRF,
// firewall, or other barrier between them. The user declares this; kezio
// never probes it. Two different Sites are NOT guaranteed to reach each
// other at all - that non-guarantee is why seeder and tracker placement
// are Site-scoped rather than cluster-wide.
type SiteSpec struct {
	// SeederSubnetRef names the Subnet of this Site that seeder pods and
	// this Site's tracker attach to. It may be a Subnet dedicated to the
	// data plane (no machines on it at all) or one shared with the
	// machines being provisioned; which one a Site uses is that Site's
	// own network design. A Site with no SeederSubnetRef runs no local
	// seeder and no tracker, which is correct for a Site whose Images
	// carry no torrent content.
	// +optional
	SeederSubnetRef *NameRef `json:"seederSubnetRef,omitempty"`
	// Tracker configures this Site's tracker: either a pinned local
	// address (IP) or a reference to a tracker the operator already runs
	// (ExternalURL). Meaningless without SeederSubnetRef, since a Site
	// with no seeder has nothing to announce for.
	// +optional
	Tracker SiteTracker `json:"tracker,omitempty"`
}

// Condition types set on SiteStatus.Conditions.
const (
	// SiteConditionReady reflects whether this Site's tracker Deployment
	// (when Tracker.IP is set) has become available. A Site with no
	// SeederSubnetRef, or one using Tracker.ExternalURL, has no tracker
	// Deployment of its own and is Ready once Valid.
	SiteConditionReady = "Ready"
	// SiteConditionValid reports whether this Site passed both
	// schema-level validation (enforced at admission, so always True for
	// an admitted object) and the SiteReconciler's own checks against its
	// referenced SeederSubnetRef. False names the first failing check as
	// Reason.
	SiteConditionValid = "Valid"
	// SiteConditionTrackerNetworkReady reports whether this Site's
	// tracker pod (when Tracker.IP is set) is actually attached to
	// SeederSubnetRef's seeder NAD at Tracker.IP, not just reporting
	// Kubernetes-Ready off its cluster interface alone - a pod can pass
	// that check with a Multus attachment that silently failed to come
	// up. Unknown until the tracker Deployment is Available; True for a
	// Site with no tracker Deployment of its own.
	SiteConditionTrackerNetworkReady = "TrackerNetworkReady"
)

// SiteStatus defines the observed state of Site.
type SiteStatus struct {
	// SubnetRefs lists the names of the Subnets in this Site's own
	// namespace whose spec.siteRef names this Site.
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	// +optional
	SubnetRefs []string `json:"subnetRefs,omitempty"`
	// SeederReady reports whether this Site's seeder placement (the
	// workload watching SeederSubnetRef) is healthy. Always false for a
	// Site with no SeederSubnetRef.
	// +optional
	SeederReady bool `json:"seederReady,omitempty"`
	// TrackerURL is the resolved tracker URL this Site's seeders
	// announce through: derived from Tracker.IP when set, echoed from
	// Tracker.ExternalURL otherwise, and empty for a Site with no
	// tracker at all.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	TrackerURL string `json:"trackerURL,omitempty"`
	// Conditions report the current state of the Site, including
	// SiteConditionReady and SiteConditionValid. Every write carries
	// observedGeneration (metav1.Condition's own field) set to the
	// generation the write observed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezs,site}
// +kubebuilder:printcolumn:name="SeederSubnet",type=string,JSONPath=`.spec.seederSubnetRef.name`
// +kubebuilder:printcolumn:name="TrackerURL",type=string,JSONPath=`.status.trackerURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Site is the Schema for the sites API. It is a maximal routable domain:
// every Subnet that belongs to one Site can route to every other Subnet
// of that Site, and two different Sites are NOT guaranteed to reach each
// other at all. See SiteSpec's doc comment for the full model.
type Site struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteSpec   `json:"spec,omitempty"`
	Status SiteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SiteList contains a list of Site.
type SiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Site `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Site{}, &SiteList{})
}
