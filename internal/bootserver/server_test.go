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

package bootserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// newTestServer builds a Server backed by a fake client seeded with
// machines, indexed the same way SetupFieldIndexer configures a real
// manager cache. artifactsDir may be "" for tests that never touch the
// artifacts endpoint.
func newTestServer(t *testing.T, artifactsDir string, machines ...*keziov1alpha1.Machine) (*Server, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha1.Machine{}, MachineBootMACIndexField, IndexMachineBootMAC).
		WithStatusSubresource(&keziov1alpha1.Machine{})
	for _, m := range machines {
		builder = builder.WithObjects(m)
	}
	c := builder.Build()

	s := New(c, Config{
		ArtifactsDir: artifactsDir,
		ServerURL:    "http://boot.example.test:8090",
	})
	return s, c
}

// testMAC is the boot MAC address every test machine uses. It stays
// fixed across all the handler tests below because they exercise
// Server's decision logic for a single machine at a time; the
// request-path tests that need to distinguish MACs (unknown, malformed)
// pass their own literal instead of going through newTestMachine.
const testMAC = "aa:bb:cc:dd:ee:01"

func newTestMachine(state string) *keziov1alpha1.Machine {
	const name = "node-01"
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keziov1alpha1.MachineSpec{
			BMC: keziov1alpha1.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: name + "-bmc"},
			},
			BootMACAddress: testMAC,
		},
		Status: keziov1alpha1.MachineStatus{
			State: state,
		},
	}
}

var tokenPattern = regexp.MustCompile(`kezio\.token=([0-9a-f]+)`)

func extractToken(t *testing.T, body string) string {
	t.Helper()
	m := tokenPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("response has no kezio.token=: %q", body)
	}
	return m[1]
}

func TestHandleGrubConfig_NetBootNeededMintsAndRotatesToken(t *testing.T) {
	machine := newTestMachine(keziov1alpha1.MachineStateInspecting)
	s, c := newTestServer(t, t.TempDir(), machine)
	handler := s.Handler()

	// First fetch: gets a net-boot config with kernel/initrd HTTP URLs
	// and a cmdline carrying kezio.server + a freshly minted kezio.token.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-AA:BB:CC:DD:EE:01", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !containsAll(body, "linux http://boot.example.test:8090/boot/artifacts/vmlinuz",
		"initrd http://boot.example.test:8090/boot/artifacts/initrd.img",
		"kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config missing expected content: %q", body)
	}
	firstToken := extractToken(t, body)

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: machine.Name}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot == nil || stored.Status.NetBoot.TokenHash == "" {
		t.Fatalf("status.netBoot.tokenHash was not persisted: %+v", stored.Status.NetBoot)
	}
	if stored.Status.NetBoot.TokenHash != hashToken(firstToken) {
		t.Fatalf("stored token hash does not match the minted token")
	}
	if !stored.Status.NetBoot.ExpiresAt.After(time.Now()) {
		t.Fatalf("stored token expiry is not in the future: %v", stored.Status.NetBoot.ExpiresAt)
	}

	// Second fetch (a PXE retry, or the firmware simply asking again):
	// rotates to a fresh token, invalidating the first.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-aa:bb:cc:dd:ee:01", nil))
	secondToken := extractToken(t, rec2.Body.String())
	if secondToken == firstToken {
		t.Fatalf("token did not rotate across fetches")
	}

	var stored2 keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: machine.Name}, &stored2); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored2.Status.NetBoot.TokenHash != hashToken(secondToken) {
		t.Fatalf("stored token hash was not rotated to the second token")
	}
	if stored2.Status.NetBoot.TokenHash == stored.Status.NetBoot.TokenHash {
		t.Fatalf("stored token hash did not change across fetches")
	}
}

func TestHandleGrubConfig_BootLocalCases(t *testing.T) {
	cases := []struct {
		name     string
		machines []*keziov1alpha1.Machine
		reqMAC   string
	}{
		{
			name:     "provisioned machine boots local disk",
			machines: []*keziov1alpha1.Machine{newTestMachine(keziov1alpha1.MachineStateProvisioned)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "available machine boots local disk",
			machines: []*keziov1alpha1.Machine{newTestMachine(keziov1alpha1.MachineStateAvailable)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "enrolling machine boots local disk",
			machines: []*keziov1alpha1.Machine{newTestMachine(keziov1alpha1.MachineStateEnrolling)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "unknown MAC boots local disk",
			machines: nil,
			reqMAC:   "11:22:33:44:55:66",
		},
		{
			name:     "malformed MAC boots local disk",
			machines: nil,
			reqMAC:   "not-a-mac",
		},
	}

	var referenceBody string
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, "", tc.machines...)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+tc.reqMAC, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if body != bootLocalConfig {
				t.Fatalf("body = %q, want the fixed bootLocalConfig", body)
			}
			// Every "not netbooting" case must be byte-identical: an
			// unknown MAC, a known-but-idle machine, and a malformed MAC
			// must not be distinguishable by response shape (no Machine
			// enumeration via response size/content).
			if i == 0 {
				referenceBody = body
			} else if body != referenceBody {
				t.Fatalf("response for %q differs from the reference boot-local response", tc.name)
			}
		})
	}
}

func TestHandleGrubConfig_ErrorStateOnlyNetBootsOnInspectOrProvisionFailure(t *testing.T) {
	cases := []struct {
		reason      string
		wantNetBoot bool
	}{
		{reason: "InspectFailed", wantNetBoot: true},
		{reason: "ProvisionFailed", wantNetBoot: true},
		{reason: "RegisterFailed", wantNetBoot: false},
		{reason: "", wantNetBoot: false},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			machine := newTestMachine(keziov1alpha1.MachineStateError)
			if tc.reason != "" {
				apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
					Type:    keziov1alpha1.ConditionReady,
					Status:  metav1.ConditionFalse,
					Reason:  tc.reason,
					Message: "test",
				})
			}
			s, _ := newTestServer(t, t.TempDir(), machine)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-aa:bb:cc:dd:ee:01", nil))
			isNetBoot := rec.Body.String() != bootLocalConfig
			if isNetBoot != tc.wantNetBoot {
				t.Fatalf("reason %q: netboot = %v, want %v (body: %q)", tc.reason, isNetBoot, tc.wantNetBoot, rec.Body.String())
			}
		})
	}
}

func TestArtifactsHandler_ServesFileAndRejectsTraversalAndListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vmlinuz"), []byte("kernel-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	s, _ := newTestServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// A normal artifact is served.
	resp, err := http.Get(srv.URL + "/boot/artifacts/vmlinuz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || string(body) != "kernel-bytes" {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}

	// A path traversal attempt must not escape ArtifactsDir.
	resp2, err := http.Get(srv.URL + "/boot/artifacts/../../../../etc/passwd")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode == http.StatusOK {
		t.Fatalf("traversal request unexpectedly succeeded with status 200")
	}

	// The directory itself must not be listed.
	resp3, err := http.Get(srv.URL + "/boot/artifacts/subdir/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode == http.StatusOK {
		t.Fatalf("directory listing unexpectedly succeeded with status 200")
	}

	// The artifacts root itself must not be listed either.
	resp4, err := http.Get(srv.URL + "/boot/artifacts/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp4.Body.Close() }()
	if resp4.StatusCode == http.StatusOK {
		t.Fatalf("root directory listing unexpectedly succeeded with status 200")
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
