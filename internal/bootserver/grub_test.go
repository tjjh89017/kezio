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
	"strings"
	"testing"
)

// TestBootLocalConfig_ChainloadsESPFallbackDirectly pins the fix for a
// measured KubeVirt/OVMF boot loop: a plain "exit" here used to rely on
// BDS falling through to the next boot entry on its own, but a real UEFI
// boot never resumed BDS at the next BootOrder entry after GRUB ran - it
// repeated the same PXE entry from scratch instead, so the machine's disk
// was never reached. bootLocalConfig must instead chainload the disk's
// UEFI removable-fallback loader directly, at the exact path
// internal/agent/deploy's efiRemovableLoaderPath contract guarantees
// every bootable Image carries, and must never call "exit" at all.
func TestBootLocalConfig_ChainloadsESPFallbackDirectly(t *testing.T) {
	if strings.Contains(bootLocalConfig, "exit") {
		t.Fatalf("bootLocalConfig still calls exit, relying on unreliable BDS fallthrough: %q", bootLocalConfig)
	}
	for _, want := range []string{
		"search --no-floppy --set=root --file /EFI/BOOT/BOOTX64.EFI",
		"chainloader /EFI/BOOT/BOOTX64.EFI",
		"boot",
	} {
		if !strings.Contains(bootLocalConfig, want) {
			t.Fatalf("bootLocalConfig missing %q: %q", want, bootLocalConfig)
		}
	}
}

// TestGrubNetPath pins the one file-path syntax GRUB's network stack
// resolves: "(http,host:port)/path". A bare URL is not it -
// grub_file_open treats only a leading "(" as naming a device, so
// "http://host/path" is opened as a relative path on $root (the TFTP
// server, mid net-boot) and fails; that mistake is exactly what
// GrubNetPath exists to make unrepresentable.
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

// TestRenderNetBootConfig_FetchParam pins the exact live-boot cmdline
// contract: boot=live selects the live-boot initrd path at all, and
// fetch=<squashfs URL> is what live-boot's initrd reads to download the
// root file system over HTTP instead of expecting a local disk. Both
// tokens must appear together on the same "linux" line GRUB emits, ahead
// of the separate "initrd" line for the kernel's own initrd image. The
// linux/initrd file arguments use GRUB's network path syntax (GRUB is
// the consumer that opens those files), while fetch= and kezio.server=
// stay real URLs (live-boot's initrd and the agent consume those, not
// GRUB).
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

// TestRenderNetBootConfig_KezioServerUsesAgentServerURL pins the fix for
// a registration handshake that used to never complete: kezio.server=
// (where the agent registers) must come from Config.AgentServerURL, not
// ServerURL (the boot config server's own address, used only for the
// kernel/initrd/squashfs paths on the same line) - internal/agentserver
// and internal/bootserver are two different Services on two different
// container ports fronting the same controller-manager Pod, so
// collapsing kezio.server= onto ServerURL sends every agent registration
// to a server that never mounts internal/agentserver's routes.
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
