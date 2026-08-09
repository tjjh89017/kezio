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

// SiteSpec defines the desired state of Site.
//
// A Site is a maximal routable domain: every Subnet that belongs to one
// Site is mutually routable with every other Subnet in that same Site,
// with no VRF, firewall, or other barrier breaking reachability between
// them. A Subnet joins a Site through SubnetSpec.SiteRef; at most one
// Subnet per Site is designated to host that Site's seeders, through
// SeederSubnetRef.
type SiteSpec struct {
	// SeederSubnetRef names the Subnet in this Site that seeder pods
	// attach to. Optional: a Site with none runs no local seeder, which
	// is fine for a Site whose Images carry no torrent content. A Site
	// whose Images do carry torrent content needs its own seeder Subnet -
	// leeching across a routed link from another Site's seeder is not
	// currently supported, so a Machine at such a Site would otherwise
	// wait forever. The referenced Subnet's SeederNetworkRef supplies the
	// actual NetworkAttachmentDefinition; that Subnet may be dedicated to
	// seeders or shared with machines being provisioned.
	// +optional
	SeederSubnetRef *NameRef `json:"seederSubnetRef,omitempty"`
}

// SiteStatus defines the observed state of Site. There is no Site
// reconciler, so nothing populates this today; a problem with
// SeederSubnetRef surfaces instead on the affected Image's status.
type SiteStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ezs,site}
// +kubebuilder:printcolumn:name="SeederSubnet",type=string,JSONPath=`.spec.seederSubnetRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Site is the Schema for the sites API. Its identity elsewhere in the
// API is "namespace/name": Site has no cluster-wide uniqueness guarantee
// on its name alone, so two Sites in different namespaces may share one.
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
