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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// TestGrubNetPath pins the one syntax GRUB's network stack resolves:
// "(http,host:port)/path" - a bare URL like "http://host/path" is instead
// opened as a relative path on $root and fails.
func TestGrubNetPath(t *testing.T) {
	for name, tc := range map[string]struct {
		serverURL string
		filePath  string
		want      string
		wantErr   bool
	}{
		"host and port": {
			serverURL: "http://192.0.2.1:8090",
			filePath:  "/boot/artifacts/vmlinuz",
			want:      "(http,192.0.2.1:8090)/boot/artifacts/vmlinuz",
		},
		"hostname without port": {
			serverURL: "http://boot.example.test",
			filePath:  "/boot/grub.cfg",
			want:      "(http,boot.example.test)/boot/grub.cfg",
		},
		"base path on the URL is preserved": {
			serverURL: "http://192.0.2.1:8090/base/",
			filePath:  "file",
			want:      "(http,192.0.2.1:8090)/base/file",
		},
		"https rejected: GRUB netboot has no TLS stack": {
			serverURL: "https://192.0.2.1:8090",
			filePath:  "/x",
			wantErr:   true,
		},
		"missing host rejected": {
			serverURL: "http://",
			filePath:  "/x",
			wantErr:   true,
		},
		"garbage rejected": {
			serverURL: "not a url at all",
			filePath:  "/x",
			wantErr:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := GrubNetPath(tc.serverURL, tc.filePath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GrubNetPath(%q, %q) = %q, want error", tc.serverURL, tc.filePath, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("GrubNetPath(%q, %q): %v", tc.serverURL, tc.filePath, err)
			}
			if got != tc.want {
				t.Fatalf("GrubNetPath(%q, %q) = %q, want %q", tc.serverURL, tc.filePath, got, tc.want)
			}
		})
	}
}

// TestBootLocalConfig_ChainloadsTheImageContractLoader pins what
// "boot local" must actually do: load the deployed disk's own bootloader
// itself. "exit" alone cannot do that - GRUB exits with EFI_SUCCESS, the
// firmware records the net boot option as a successful boot, stops walking
// BootOrder, and lands in its setup application, so the disk entry the
// finalize hook wrote in NVRAM is never tried.
func TestBootLocalConfig_ChainloadsTheImageContractLoader(t *testing.T) {
	// The exact path kezio's image contract guarantees on a bootable
	// Image's ESP (see internal/agent/deploy.efiRemovableLoaderPath).
	const fallbackLoader = "/EFI/BOOT/BOOTX64.EFI"

	for _, want := range []string{
		// Find the ESP by the one file the contract guarantees, into a
		// variable of kezio's own: a failed "search" leaves $root at the
		// net boot device, so $root cannot report whether it matched.
		"search --no-floppy --file --set=kezio_esp " + fallbackLoader,
		"set root=${kezio_esp}",
		"chainloader " + fallbackLoader,
		"boot\n",
		// Last resort only, for a machine that carries no local system at
		// all.
		"\nexit\n",
	} {
		if !strings.Contains(bootLocalConfig, want) {
			t.Fatalf("bootLocalConfig does not contain %q:\n%s", want, bootLocalConfig)
		}
	}

	if strings.Index(bootLocalConfig, "\nexit\n") < strings.Index(bootLocalConfig, "chainloader") {
		t.Fatalf("exit precedes the chainloader, so the local system is never tried:\n%s", bootLocalConfig)
	}
}

// TestBootLocalConfig_EveryOutcomeNamesItself: a machine that fails to
// boot locally lands in the firmware's setup application, where the only
// other symptom is kezio's agent-connect timeout expiring in silence.
// Each of the three outcomes must say which one it was.
func TestBootLocalConfig_EveryOutcomeNamesItself(t *testing.T) {
	after := strings.Index(bootLocalConfig, "  boot\n")
	if after < 0 {
		t.Fatalf("bootLocalConfig has no boot command:\n%s", bootLocalConfig)
	}

	if !strings.Contains(bootLocalConfig[:after], "echo ") {
		t.Errorf("bootLocalConfig says nothing before handing control to the local loader:\n%s", bootLocalConfig)
	}
	// "boot" returning at all means the loader never took over. GRUB's own
	// report for that is "error: unknown error."
	if !strings.Contains(bootLocalConfig[after:], "echo ") {
		t.Errorf("bootLocalConfig says nothing when the local loader fails to start:\n%s", bootLocalConfig)
	}
	if !strings.Contains(bootLocalConfig, "else\n  echo ") {
		t.Errorf("bootLocalConfig says nothing when no disk carries the fallback loader:\n%s", bootLocalConfig)
	}
}

// TestSubnetBootBaseURL pins the derivation subnetBootBaseURL performs -
// bootd.DefaultProxyPort on the Subnet's own BootdServerIP - and every
// case that must fall back instead (no boot half, nil Subnet).
func TestSubnetBootBaseURL(t *testing.T) {
	t.Run("boot plane present", func(t *testing.T) {
		subnet := &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "s"},
			Spec: keziov1alpha3.SubnetSpec{
				DHCP:          &keziov1alpha3.SubnetDHCP{Mode: keziov1alpha3.SubnetDHCPModeProxy},
				BootdServerIP: "192.0.2.2",
			},
		}
		got, ok := subnetBootBaseURL(subnet)
		if !ok {
			t.Fatalf("subnetBootBaseURL() ok = false, want true")
		}
		if want := "http://192.0.2.2:80"; got != want {
			t.Fatalf("subnetBootBaseURL() = %q, want %q", got, want)
		}
	})

	t.Run("no boot half", func(t *testing.T) {
		subnet := &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "s"},
			Spec:       keziov1alpha3.SubnetSpec{},
		}
		if _, ok := subnetBootBaseURL(subnet); ok {
			t.Fatalf("subnetBootBaseURL() ok = true for a Subnet with no boot half, want false")
		}
	})

	t.Run("nil Subnet", func(t *testing.T) {
		if _, ok := subnetBootBaseURL(nil); ok {
			t.Fatalf("subnetBootBaseURL(nil) ok = true, want false")
		}
	})
}

// TestRenderNetBootConfig_FetchParam pins the live-boot cmdline contract:
// boot=live and fetch=<squashfs URL> appear together on the "linux" line,
// ahead of a separate "initrd" line. linux/initrd use GRUB's network path
// syntax; fetch= and kezio.server= stay real URLs (live-boot's initrd and
// the agent consume those, not GRUB).
func TestRenderNetBootConfig_FetchParam(t *testing.T) {
	cfg := Config{
		ServerURL: "http://boot.example.test:8090",
	}.withDefaults()

	got, err := renderNetBootConfig(cfg, "deadbeef")
	if err != nil {
		t.Fatalf("renderNetBootConfig: %v", err)
	}

	want := "linux (http,boot.example.test:8090)/boot/artifacts/vmlinuz " +
		"boot=live fetch=http://boot.example.test:8090/boot/artifacts/filesystem.squashfs " +
		"kezio.server=http://boot.example.test:8090 kezio.token=deadbeef"
	if !containsAll(got, want) {
		t.Fatalf("net-boot config missing expected linux line: got %q, want substring %q", got, want)
	}
	if !containsAll(got, "initrd (http,boot.example.test:8090)/boot/artifacts/initrd.img") {
		t.Fatalf("net-boot config missing expected initrd line: %q", got)
	}
}

// TestRenderNetBootConfig_KezioServerUsesAgentServerURL: kezio.server=
// must come from Config.AgentServerURL, not ServerURL (used only for the
// kernel/initrd/squashfs paths) - collapsing the two would send every
// agent registration to a server that never mounts the agent server's
// routes.
func TestRenderNetBootConfig_KezioServerUsesAgentServerURL(t *testing.T) {
	cfg := Config{
		ServerURL:      "http://boot.example.test:8090",
		AgentServerURL: "http://agent.example.test:8091",
	}.withDefaults()

	got, err := renderNetBootConfig(cfg, "deadbeef")
	if err != nil {
		t.Fatalf("renderNetBootConfig: %v", err)
	}

	if !containsAll(got, "kezio.server=http://agent.example.test:8091 kezio.token=deadbeef") {
		t.Fatalf("net-boot config kezio.server= did not use AgentServerURL: %q", got)
	}
	if containsAll(got, "kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config kezio.server= incorrectly used ServerURL: %q", got)
	}
	if !containsAll(got, "linux (http,boot.example.test:8090)/boot/artifacts/vmlinuz") {
		t.Fatalf("net-boot config kernel path should still use ServerURL: %q", got)
	}
}

// TestRenderNetBootConfig_CustomSquashfsPath proves fetch= tracks
// Config.SquashfsPath rather than being hardcoded to the default
// filename, the same way KernelPath and InitrdPath already do.
func TestRenderNetBootConfig_CustomSquashfsPath(t *testing.T) {
	cfg := Config{
		ServerURL:    "http://boot.example.test:8090",
		SquashfsPath: "rootfs.squashfs",
	}.withDefaults()

	got, err := renderNetBootConfig(cfg, "deadbeef")
	if err != nil {
		t.Fatalf("renderNetBootConfig: %v", err)
	}

	if !containsAll(got, "fetch=http://boot.example.test:8090/boot/artifacts/rootfs.squashfs") {
		t.Fatalf("net-boot config did not honor custom SquashfsPath: %q", got)
	}
}

// TestRenderNetBootConfig_MalformedServerURL proves a ServerURL GRUB
// could never fetch from surfaces as an error (the caller's fail-secure
// boot-local fallback) instead of rendering a config carrying a live
// token that can never boot.
func TestRenderNetBootConfig_MalformedServerURL(t *testing.T) {
	cfg := Config{
		ServerURL: "https://boot.example.test:8090",
	}.withDefaults()

	if got, err := renderNetBootConfig(cfg, "deadbeef"); err == nil {
		t.Fatalf("renderNetBootConfig with a non-http ServerURL = %q, want error", got)
	}
}
