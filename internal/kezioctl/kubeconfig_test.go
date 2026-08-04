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

package kezioctl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
