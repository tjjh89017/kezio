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

package planbuild

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func TestMergeParams_MachineOverridesImage(t *testing.T) {
	image := &apiextensionsv1.JSON{Raw: []byte(`{"a":"image-a","b":"image-b"}`)}
	machine := &apiextensionsv1.JSON{Raw: []byte(`{"b":"machine-b"}`)}

	got, err := mergeParams(image, machine)
	if err != nil {
		t.Fatalf("mergeParams: %v", err)
	}
	if got["a"] != "image-a" || got["b"] != "machine-b" {
		t.Fatalf("mergeParams = %+v, want a=image-a b=machine-b", got)
	}
}

func TestMergeParams_NilBothIsEmpty(t *testing.T) {
	got, err := mergeParams(nil, nil)
	if err != nil {
		t.Fatalf("mergeParams: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("mergeParams(nil, nil) = %+v, want empty", got)
	}
}

func TestMergeParams_InvalidJSONErrors(t *testing.T) {
	bad := &apiextensionsv1.JSON{Raw: []byte(`not json`)}
	if _, err := mergeParams(bad, nil); err == nil {
		t.Fatal("mergeParams with invalid image params: want error, got nil")
	}
	if _, err := mergeParams(nil, bad); err == nil {
		t.Fatal("mergeParams with invalid machine params: want error, got nil")
	}
}

func TestPartitionDevicePath(t *testing.T) {
	cases := []struct {
		disk   string
		number int32
		want   string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/vda", 2, "/dev/vda2"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/mmcblk0", 3, "/dev/mmcblk0p3"},
	}
	for _, c := range cases {
		if got := partitionDevicePath(c.disk, c.number); got != c.want {
			t.Errorf("partitionDevicePath(%q, %d) = %q, want %q", c.disk, c.number, got, c.want)
		}
	}
}

func TestHooksHash_DeterministicAndSensitiveToContent(t *testing.T) {
	a := []agentapi.ResolvedHook{{Name: "h", Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeBuiltin, Builtin: "mkswap"}}}}
	b := []agentapi.ResolvedHook{{Name: "h", Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeBuiltin, Builtin: "efibootmgr"}}}}

	h1, err := hooksHash(a)
	if err != nil {
		t.Fatalf("hooksHash: %v", err)
	}
	h1Again, err := hooksHash(a)
	if err != nil {
		t.Fatalf("hooksHash: %v", err)
	}
	if h1 != h1Again {
		t.Fatalf("hooksHash is not deterministic: %q != %q", h1, h1Again)
	}

	h2, err := hooksHash(b)
	if err != nil {
		t.Fatalf("hooksHash: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("hooksHash did not change with different hook content")
	}
}

func TestRenderTemplate_MissingKeyErrors(t *testing.T) {
	if _, err := renderTemplate("{{ .undeclared }}", map[string]any{}); err == nil {
		t.Fatal("renderTemplate with an undeclared placeholder: want error, got nil")
	}
}

func TestRenderTemplate_ResolvesDeclaredKey(t *testing.T) {
	got, err := renderTemplate("hello {{ .name }}", map[string]any{"name": "kezio"})
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	if got != "hello kezio" {
		t.Fatalf("renderTemplate = %q, want %q", got, "hello kezio")
	}
}

func TestResolveNamespace(t *testing.T) {
	if got := resolveNamespace(keziov1alpha2.NameRef{Name: "n"}, "default"); got != "default" {
		t.Errorf("resolveNamespace with empty ref namespace = %q, want default", got)
	}
	if got := resolveNamespace(keziov1alpha2.NameRef{Namespace: "other", Name: "n"}, "default"); got != "other" {
		t.Errorf("resolveNamespace with set ref namespace = %q, want other", got)
	}
}
