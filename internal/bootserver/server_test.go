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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// newTestServer builds a Server backed by a fake client seeded with
// machines, indexed the same way SetupFieldIndexer configures a real
// manager cache. artifactsDir may be "" for tests that never touch the
// artifacts endpoint.
func newTestServer(t *testing.T, artifactsDir string, machines ...*keziov1alpha2.Machine) (*Server, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha2.Machine{}, MachineBootMACIndexField, IndexMachineBootMAC).
		WithStatusSubresource(&keziov1alpha2.Machine{})
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

func newTestMachine(state string) *keziov1alpha2.Machine {
	const name = "node-01"
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: name + "-bmc"},
			},
			BootMACAddress: testMAC,
			SubnetRef:      keziov1alpha2.NameRef{Name: name + "-subnet"},
		},
		Status: keziov1alpha2.MachineStatus{
			State: state,
		},
	}
}

// newTestSubnetWithBootPlane builds the Subnet newTestMachine's SubnetRef
// names, with a boot half (BootdServerIP set) at bootdIP.
func newTestSubnetWithBootPlane(bootdIP string) *keziov1alpha2.Subnet {
	return &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01-subnet"},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef:       keziov1alpha2.NameRef{Name: "site"},
			CIDR:          "192.0.2.0/24",
			BootdServerIP: bootdIP,
			BootdNetworkRef: &keziov1alpha2.NameRef{
				Name: "kezio-boot-network",
			},
			DHCP: &keziov1alpha2.SubnetDHCP{Mode: keziov1alpha2.SubnetDHCPModeProxy},
		},
	}
}

// newTestSubnetNoBootPlane builds the same Subnet name with only a seeder
// half declared - HasBootPlane() is false.
func newTestSubnetNoBootPlane() *keziov1alpha2.Subnet {
	return &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01-subnet"},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef: keziov1alpha2.NameRef{Name: "site"},
			CIDR:    "192.0.2.0/24",
			SeederNetworkRef: &keziov1alpha2.NameRef{
				Name: "kezio-seeder-network",
			},
		},
	}
}

// TestServer_NeedLeaderElectionIsFalse guards against a regression that
// broke boot config serving during a rolling update: since config/manager
// never releases a lease on termination, a Server gated on leader
// election would leave GET /boot/grub.cfg-<mac> unanswered for a whole
// rolling update, past the readiness probe passing.
func TestServer_NeedLeaderElectionIsFalse(t *testing.T) {
	s, _ := newTestServer(t, "")
	if s.NeedLeaderElection() {
		t.Fatal("Server.NeedLeaderElection() = true; want false so a rolling update does not leave grub.cfg unanswered until the old lease expires")
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

// TestHandleHTTPBootGrubSearch covers the config search a GRUB loaded
// over UEFI HTTP Boot performs against /boot/http/grub/: the MAC-keyed
// name must behave exactly like the colon-form route, and every other
// search name must 404 so the search proceeds to (or past) it.
func TestHandleHTTPBootGrubSearch(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
	s, _ := newTestServer(t, t.TempDir(), machine)
	handler := s.Handler()

	t.Run("dash MAC serves the per-machine config", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/http/grub/grub.cfg-01-aa-bb-cc-dd-ee-01", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "kezio.token=") {
			t.Fatalf("response is not the net-boot config: %q", body)
		}
	})

	t.Run("unknown dash MAC boots local, indistinguishable from known-idle", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/http/grub/grub.cfg-01-aa-bb-cc-dd-ee-99", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if body := rec.Body.String(); body != bootLocalConfig {
			t.Fatalf("body = %q, want the fixed boot-local config", body)
		}
	})

	t.Run("plain grub.cfg serves the fixed MAC-redirect stub", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grub/grub.cfg", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if body != grubSearchRedirectConfig {
			t.Fatalf("body = %q, want the fixed redirect stub", body)
		}
		// The stub must never vary by requester: it re-enters the search
		// with the machine's own MAC, and the per-machine decision happens
		// there, not here.
		if !strings.Contains(body, "grub.cfg-01-${net_default_mac}") {
			t.Fatalf("stub does not re-enter the MAC-keyed search: %q", body)
		}
	})

	// The UUID name is searched before the MAC one, so anything but 404
	// here would stop GRUB from ever reaching grub.cfg-01-<mac>; the ip
	// name after it carries no identity to decide a response by.
	for _, name := range []string{
		"grub.cfg-8a3f0b6e-0000-4000-8000-2f6a1c3d9b10",
		"grub.cfg-192.0.2.57",
		"something-else",
	} {
		t.Run("404: "+name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/http/grub/"+name, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 so the GRUB config search continues", rec.Code)
			}
		})
	}
}

func TestHandleGrubConfig_NetBootNeededMintsAndRotatesToken(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
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
	if !containsAll(body, "linux (http,boot.example.test:8090)/boot/artifacts/vmlinuz",
		"boot=live fetch=http://boot.example.test:8090/boot/artifacts/filesystem.squashfs",
		"initrd (http,boot.example.test:8090)/boot/artifacts/initrd.img",
		"kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config missing expected content: %q", body)
	}
	firstToken := extractToken(t, body)

	var stored keziov1alpha2.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: machine.Name}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot == nil || stored.Status.NetBoot.TokenHash == "" {
		t.Fatalf("status.netBoot.tokenHash was not persisted: %+v", stored.Status.NetBoot)
	}
	if stored.Status.NetBoot.TokenHash != hashToken(firstToken) {
		t.Fatalf("stored token hash does not match the minted token")
	}
	if stored.Status.NetBoot.TokenHash == firstToken {
		t.Fatalf("status.netBoot.tokenHash stored the plaintext token instead of its hash")
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

	var stored2 keziov1alpha2.Machine
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

// TestHandleGrubConfig_SubnetWithBootPlaneOverridesBaseURL proves the
// two-Site fix: a machine whose Subnet declares its own bootd address
// gets that address embedded for kernel/initrd/squashfs and kezio.server=
// instead of the manager-wide Config.ServerURL/AgentServerURL - the
// address a machine on an isolated Site can actually reach.
func TestHandleGrubConfig_SubnetWithBootPlaneOverridesBaseURL(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
	subnet := newTestSubnetWithBootPlane("192.0.2.2")
	s, _ := newTestServer(t, t.TempDir(), machine)
	if err := s.Client.Create(context.Background(), subnet); err != nil {
		t.Fatalf("Create Subnet: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !containsAll(body,
		"linux (http,192.0.2.2:80)/boot/artifacts/vmlinuz",
		"fetch=http://192.0.2.2:80/boot/artifacts/filesystem.squashfs",
		"kezio.server=http://192.0.2.2:80") {
		t.Fatalf("net-boot config did not use the Subnet's own bootd address: %q", body)
	}
	if containsAll(body, "boot.example.test") {
		t.Fatalf("net-boot config still carries the manager-wide ServerURL: %q", body)
	}
}

// TestHandleGrubConfig_SubnetWithoutBootPlaneFallsBackToManagerWideURL
// covers a Subnet that exists but declares no boot half (seeder-only): the
// override must not fire, and the manager-wide Config.ServerURL/
// AgentServerURL still apply, same as an unresolved SubnetRef.
func TestHandleGrubConfig_SubnetWithoutBootPlaneFallsBackToManagerWideURL(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
	subnet := newTestSubnetNoBootPlane()
	s, _ := newTestServer(t, t.TempDir(), machine)
	if err := s.Client.Create(context.Background(), subnet); err != nil {
		t.Fatalf("Create Subnet: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !containsAll(body, "kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config did not fall back to the manager-wide AgentServerURL: %q", body)
	}
}

// TestHandleGrubConfig_DanglingSubnetRefFallsBackToManagerWideURL pins the
// pre-existing behavior every other test in this file already relies on
// (newTestMachine's SubnetRef never resolves unless a test creates the
// Subnet): a Machine referencing a Subnet that does not exist must still
// net boot, using the manager-wide URL, not fail the request.
func TestHandleGrubConfig_DanglingSubnetRefFallsBackToManagerWideURL(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
	s, _ := newTestServer(t, t.TempDir(), machine)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !containsAll(body, "kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config did not fall back to the manager-wide AgentServerURL: %q", body)
	}
}

func TestHandleGrubConfig_BootLocalCases(t *testing.T) {
	cases := []struct {
		name     string
		machines []*keziov1alpha2.Machine
		reqMAC   string
	}{
		{
			name:     "provisioned machine boots local disk",
			machines: []*keziov1alpha2.Machine{newTestMachine(keziov1alpha2.MachineStateProvisioned)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "available machine boots local disk",
			machines: []*keziov1alpha2.Machine{newTestMachine(keziov1alpha2.MachineStateAvailable)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "enrolling machine boots local disk",
			machines: []*keziov1alpha2.Machine{newTestMachine(keziov1alpha2.MachineStateEnrolling)},
			reqMAC:   "aa:bb:cc:dd:ee:01",
		},
		{
			// A machine mid provisioning-failure retry stays State
			// Provisioning in v1alpha2 (see needsNetBoot's doc comment),
			// so this exercises the same "still net booting" branch as
			// MachineStateProvisioning without a separate error state to
			// construct.
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

// TestHandleGrubConfig_UnknownMACYieldsLocalBoot pins the fail-secure
// default directly: a MAC no Machine advertises must never receive a
// live-environment config or a token, regardless of what other machines
// exist.
func TestHandleGrubConfig_UnknownMACYieldsLocalBoot(t *testing.T) {
	machine := newTestMachine(keziov1alpha2.MachineStateInspecting)
	s, c := newTestServer(t, "", machine)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-11:22:33:44:55:66", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != bootLocalConfig || strings.Contains(body, "kezio.token=") {
		t.Fatalf("body = %q, want the fixed bootLocalConfig with no token", body)
	}

	var stored keziov1alpha2.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: machine.Name}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot != nil {
		t.Fatalf("an unrelated Machine's status.netBoot was written by an unknown-MAC request: %+v", stored.Status.NetBoot)
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
