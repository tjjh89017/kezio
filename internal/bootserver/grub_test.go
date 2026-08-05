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

import "testing"

// TestRenderNetBootConfig_FetchParam pins the exact live-boot cmdline
// contract: boot=live selects the live-boot initrd path at all, and
// fetch=<squashfs URL> is what live-boot's initrd reads to download the
// root file system over HTTP instead of expecting a local disk. Both
// tokens must appear together on the same "linux" line GRUB emits, ahead
// of the separate "initrd" line for the kernel's own initrd image.
func TestRenderNetBootConfig_FetchParam(t *testing.T) {
	cfg := Config{
		ServerURL: "http://boot.example.test:8090",
	}.withDefaults()

	got := renderNetBootConfig(cfg, "deadbeef")

	want := "linux http://boot.example.test:8090/boot/artifacts/vmlinuz " +
		"boot=live fetch=http://boot.example.test:8090/boot/artifacts/filesystem.squashfs " +
		"kezio.server=http://boot.example.test:8090 kezio.token=deadbeef"
	if !containsAll(got, want) {
		t.Fatalf("net-boot config missing expected linux line: got %q, want substring %q", got, want)
	}
	if !containsAll(got, "initrd http://boot.example.test:8090/boot/artifacts/initrd.img") {
		t.Fatalf("net-boot config missing expected initrd line: %q", got)
	}
}

// TestRenderNetBootConfig_KezioServerUsesAgentServerURL pins the fix for
// a registration handshake that used to never complete: kezio.server=
// (where the agent registers) must come from Config.AgentServerURL, not
// ServerURL (the boot config server's own address, used only for the
// kernel/initrd/squashfs URLs on the same line) - internal/agentserver
// and internal/bootserver are two different Services on two different
// container ports fronting the same controller-manager Pod, so
// collapsing kezio.server= onto ServerURL sends every agent registration
// to a server that never mounts internal/agentserver's routes.
func TestRenderNetBootConfig_KezioServerUsesAgentServerURL(t *testing.T) {
	cfg := Config{
		ServerURL:      "http://boot.example.test:8090",
		AgentServerURL: "http://agent.example.test:8091",
	}.withDefaults()

	got := renderNetBootConfig(cfg, "deadbeef")

	if !containsAll(got, "kezio.server=http://agent.example.test:8091 kezio.token=deadbeef") {
		t.Fatalf("net-boot config kezio.server= did not use AgentServerURL: %q", got)
	}
	if containsAll(got, "kezio.server=http://boot.example.test:8090") {
		t.Fatalf("net-boot config kezio.server= incorrectly used ServerURL: %q", got)
	}
	if !containsAll(got, "linux http://boot.example.test:8090/boot/artifacts/vmlinuz") {
		t.Fatalf("net-boot config kernel URL should still use ServerURL: %q", got)
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

	got := renderNetBootConfig(cfg, "deadbeef")

	if !containsAll(got, "fetch=http://boot.example.test:8090/boot/artifacts/rootfs.squashfs") {
		t.Fatalf("net-boot config did not honor custom SquashfsPath: %q", got)
	}
}
