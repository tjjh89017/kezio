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

package nadvalidate

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConfigFromUnstructured reads a NetworkAttachmentDefinition's
// spec.config field out of nad. kezio has no typed
// k8s.cni.cncf.io/v1.NetworkAttachmentDefinition client - the module
// that provides one pulls in its own generated clientset and informers
// to read a single JSON string field, and nothing else in this repo
// reads a NAD as a Go object (config/*/networkattachmentdefinition*.yaml
// and .github/actions/create-provisioning-nads apply NADs as raw YAML,
// never through client-go). unstructured.Unstructured, backed by the
// apimachinery dependency this repo already has, reads that one field
// without adding a dependency whose only use is this.
func ConfigFromUnstructured(nad *unstructured.Unstructured) (string, error) {
	config, found, err := unstructured.NestedString(nad.Object, "spec", "config")
	if err != nil {
		return "", fmt.Errorf("nad %s: spec.config: %w", nad.GetName(), err)
	}
	if !found {
		return "", fmt.Errorf("nad %s: spec.config not set", nad.GetName())
	}
	return config, nil
}
