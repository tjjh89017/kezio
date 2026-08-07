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

// ImageLayoutSpec defines the desired state of ImageLayout: the verbatim
// sfdisk dump of an Image's source disk layout.
type ImageLayoutSpec struct {
	// SfdiskJSON is the verbatim `sfdisk --json` dump of the source
	// disk's partition table, captured once during ingest. It carries
	// the GPT disk GUID and partition type GUIDs so a restored disk
	// matches the source exactly.
	//
	// This is deliberately a plain string, not typed fields: the agent
	// feeds this straight back to sfdisk to replay the layout onto a
	// target disk, and sfdisk's own JSON schema drifts across versions.
	// Modeling partitions as typed fields here would require reinterpreting
	// that schema instead of replaying it verbatim, and would need to
	// track every sfdisk version kezio-ingest might run against.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:XValidation:rule="self.startsWith('{')",message="spec.sfdiskJSON must be a JSON object (the verbatim `sfdisk --json` dump)"
	SfdiskJSON string `json:"sfdiskJSON"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName={ezl,imagelayout}
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageLayout is the Schema for the imagelayouts API. One ImageLayout is
// written once, by the ingest Job, for each Image that reaches Ready with
// a disk layout - see internal/ingest.WriteImageLayout and
// ImageDiskStatus.LayoutRef. It carries no Status: nothing updates it
// after that single write.
type ImageLayout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ImageLayoutSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ImageLayoutList contains a list of ImageLayout.
type ImageLayoutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageLayout `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageLayout{}, &ImageLayoutList{})
}
