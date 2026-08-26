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

package kezioctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// writeKubeconfig writes a minimal valid kubeconfig file naming a single
// context "ctx" in namespace ns, pointing at a fake server, and returns
// its path.
func writeKubeconfig(t *testing.T, dir, filename, ns string) string {
	t.Helper()
	const tmpl = `
apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: https://example.invalid:6443
contexts:
- name: ctx
  context:
    cluster: cluster
    namespace: %s
current-context: ctx
users: []
`
	path := filepath.Join(dir, filename)
	content := []byte(fmt.Sprintf(tmpl, ns))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestLoadRESTConfig_ExplicitPathTakesPrecedenceOverEnv(t *testing.T) {
	dir := t.TempDir()
	envConfig := writeKubeconfig(t, dir, "env-config", "env-namespace")
	explicitConfig := writeKubeconfig(t, dir, "explicit-config", "explicit-namespace")

	t.Setenv("KUBECONFIG", envConfig)

	_, ns, err := LoadRESTConfig(explicitConfig)
	if err != nil {
		t.Fatalf("LoadRESTConfig() error = %v", err)
	}
	if ns != "explicit-namespace" {
		t.Errorf("namespace = %q, want %q (explicit --kubeconfig should win over KUBECONFIG)", ns, "explicit-namespace")
	}
}

func TestLoadRESTConfig_FallsBackToKUBECONFIGEnv(t *testing.T) {
	dir := t.TempDir()
	envConfig := writeKubeconfig(t, dir, "env-config", "env-namespace")

	t.Setenv("KUBECONFIG", envConfig)

	_, ns, err := LoadRESTConfig("")
	if err != nil {
		t.Fatalf("LoadRESTConfig() error = %v", err)
	}
	if ns != "env-namespace" {
		t.Errorf("namespace = %q, want %q", ns, "env-namespace")
	}
}

func TestLoadRESTConfig_InstallsAWarningHandler(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", writeKubeconfig(t, dir, "config", "ns"))

	cfg, _, err := LoadRESTConfig("")
	if err != nil {
		t.Fatalf("LoadRESTConfig() error = %v", err)
	}
	// Leaving this nil is what makes controller-runtime install its own
	// logger-backed handler, which drops the warning and dumps a stack.
	if cfg.WarningHandlerWithContext == nil {
		t.Fatal("LoadRESTConfig() left WarningHandlerWithContext nil")
	}
}

// TestRESTMapperResolvesEveryKnownKindWithoutDiscovery guards the fix for
// kezioctl depending on cluster API discovery at all: restMapper must
// resolve every real Kind Scheme registers, with no server to talk to
// (there is none in this test) and regardless of what API versions -
// v1alpha2, stale or otherwise - a real cluster happens to still
// advertise for the kezio.kojuro.date group. It reads the Kind list off
// Scheme itself (the same source restMapper is built from, and the same
// filter newRESTMapperFromScheme applies) so a Kind added to
// api/v1alpha3 is covered automatically, with no test to update.
func TestRESTMapperResolvesEveryKnownKindWithoutDiscovery(t *testing.T) {
	tested := 0
	for gvk := range Scheme.AllKnownTypes() {
		if gvk.GroupVersion() != keziov1alpha3.GroupVersion {
			continue
		}
		if gvk.Kind == "WatchEvent" || strings.HasSuffix(gvk.Kind, "List") || strings.HasSuffix(gvk.Kind, "Options") {
			continue
		}
		tested++
		t.Run(gvk.Kind, func(t *testing.T) {
			mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				t.Fatalf("RESTMapping(%s) error = %v", gvk.Kind, err)
			}
			if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
				t.Errorf("RESTMapping(%s).Scope = %v, want namespace-scoped", gvk.Kind, mapping.Scope.Name())
			}
		})
	}
	if tested == 0 {
		t.Fatal("found no Kind under keziov1alpha3.GroupVersion in Scheme - the filter is too strict")
	}
}

// TestRESTMapperIgnoresUnrelatedGroupVersions confirms the mapper never
// contacts a server: asking for a Kind or version it does not know about
// returns a plain NoMatch error rather than reaching for discovery (and
// so can never surface an unrelated group's discovery failure).
func TestRESTMapperIgnoresUnrelatedGroupVersions(t *testing.T) {
	_, err := restMapper.RESTMapping(
		keziov1alpha3.GroupVersion.WithKind("Image").GroupKind(), "v1alpha2")
	if !meta.IsNoMatchError(err) {
		t.Fatalf("RESTMapping(Image, v1alpha2) error = %v, want a NoMatchError", err)
	}
}

func TestAPIWarningHandlerPrintsOnlyServerWarnings(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		text string
		want string
	}{
		{
			name: "server warning is printed",
			code: 299,
			text: `slot 1 references PartitionContent "pc-x", which does not exist yet`,
			want: "Warning: slot 1 references PartitionContent \"pc-x\", which does not exist yet\n",
		},
		{name: "another code is ignored", code: 199, text: "stale response", want: ""},
		{name: "empty text is ignored", code: 299, text: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			apiWarningHandler{w: &buf}.HandleWarningHeaderWithContext(context.Background(), tc.code, "", tc.text)
			if got := buf.String(); got != tc.want {
				t.Fatalf("handler wrote %q, want %q", got, tc.want)
			}
		})
	}
}
