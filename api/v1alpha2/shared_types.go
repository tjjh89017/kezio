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

// NameRef is a reference to a resource by name, optionally in another
// namespace. When Namespace is empty, the namespace of the referencing
// resource is used. Field names that hold a NameRef end in "Ref"; ordered
// lists of NameRef end in "Refs".
type NameRef struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Post-mutation admission always re-serialises this struct, so a
	// zero-value Name would otherwise satisfy "required"; MinLength closes
	// that gap. MaxLength mirrors the Kubernetes object name limit and
	// bounds the CEL cost of any pattern-matching XValidation rule a
	// field of this type carries (see ImageSlot.ContentRef).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SecretReference references a Secret by name, in the same namespace as
// the referencing resource.
type SecretReference struct {
	Name string `json:"name"`
}

// ConfigMapKeyRef references a specific key in a ConfigMap, in the same
// namespace as the referencing resource.
type ConfigMapKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// SecretKeyRef references a specific key in a Secret, in the same
// namespace as the referencing resource.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}
