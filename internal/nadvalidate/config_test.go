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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestConfigFromUnstructured(t *testing.T) {
	t.Run("reads spec.config", func(t *testing.T) {
		nad := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata":   map[string]interface{}{"name": "kezio-boot-network"},
			"spec":       map[string]interface{}{"config": bootdStaticConfig},
		}}

		got, err := ConfigFromUnstructured(nad)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != bootdStaticConfig {
			t.Fatalf("config = %q, want %q", got, bootdStaticConfig)
		}
	})

	t.Run("errors when spec.config is absent", func(t *testing.T) {
		nad := &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "kezio-boot-network"},
			"spec":     map[string]interface{}{},
		}}

		if _, err := ConfigFromUnstructured(nad); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
