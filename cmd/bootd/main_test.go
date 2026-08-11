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

package main

import "testing"

// clearBootdEnv unsets every environment variable bootdConfigFromEnv
// reads (plus the three explicitly-rejected legacy names), so each test
// starts from a clean slate regardless of what the process environment
// or an earlier test in this file happens to carry.
func clearBootdEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BOOTD_SERVER_IP", "BOOTD_PROVISIONING_CIDR", "BOOTD_DHCP_INTERFACE",
		"BOOTD_NEXT_SERVER_IP", "BOOTD_TFTP_DIR",
		"BOOTD_TFTP_ADDR", "BOOTD_BOOT_CONFIG_URL", "BOOTD_BOOT_FILENAME",
		"BOOTD_RUN_DIR", "BOOTD_DNSMASQ_PATH", "BOOTD_ANSWER_ALL",
		"BOOTD_AGENT_UPSTREAM_URL", "BOOTD_BOOT_UPSTREAM_URL", "BOOTD_PROXY_ADDR",
		"BOOTD_HTTP_BOOT_URL", "BOOTD_DHCP_ADDR", "BOOTD_PXE_ADDR", "BOOTD_DHCP_RELAY_SERVER",
	} {
		t.Setenv(key, "")
	}
}

func requiredBootdEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BOOTD_SERVER_IP", "192.0.2.2")
	t.Setenv("BOOTD_PROVISIONING_CIDR", "192.0.2.0/24")
	t.Setenv("BOOTD_TFTP_DIR", "/tftp")
}

// TestBootdConfigFromEnv_ProxyDisabledByDefault pins the opt-in
// contract: an existing bootd deployment that never sets either upstream
// URL gets a disabled ProxyConfig, unchanged from before those variables
// existed - cmd/bootd only adds a ProxyServer to the manager when
// cfg.Proxy.Enabled() is true.
func TestBootdConfigFromEnv_ProxyDisabledByDefault(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Proxy.Enabled() {
		t.Errorf("Proxy.Enabled() = true with no upstream URL set, want false")
	}
	if cfg.Proxy.Addr != "" {
		t.Errorf("Proxy.Addr = %q with the proxy disabled, want empty", cfg.Proxy.Addr)
	}
}

// TestBootdConfigFromEnv_ProxyAddrDefaultsToServerIP proves an enabled
// proxy with no explicit BOOTD_PROXY_ADDR binds to BOOTD_SERVER_IP on
// port 80 - the provisioning interface's own address, not every
// interface the pod happens to have.
func TestBootdConfigFromEnv_ProxyAddrDefaultsToServerIP(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_AGENT_UPSTREAM_URL", "http://kezio-agent-server.kezio-system.svc:8091")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if !cfg.Proxy.Enabled() {
		t.Fatal("Proxy.Enabled() = false with BOOTD_AGENT_UPSTREAM_URL set")
	}
	const want = "192.0.2.2:80"
	if cfg.Proxy.Addr != want {
		t.Errorf("Proxy.Addr = %q, want %q", cfg.Proxy.Addr, want)
	}
	if cfg.Proxy.AgentUpstreamURL != "http://kezio-agent-server.kezio-system.svc:8091" {
		t.Errorf("Proxy.AgentUpstreamURL = %q", cfg.Proxy.AgentUpstreamURL)
	}
}

// TestBootdConfigFromEnv_ProxyAddrExplicitOverride proves
// BOOTD_PROXY_ADDR, when set, wins over the BOOTD_SERVER_IP-derived
// default.
func TestBootdConfigFromEnv_ProxyAddrExplicitOverride(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_BOOT_UPSTREAM_URL", "http://kezio-boot-server.kezio-system.svc:8090")
	t.Setenv("BOOTD_PROXY_ADDR", "0.0.0.0:8080")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Proxy.Addr != "0.0.0.0:8080" {
		t.Errorf("Proxy.Addr = %q, want %q", cfg.Proxy.Addr, "0.0.0.0:8080")
	}
	if cfg.Proxy.BootUpstreamURL != "http://kezio-boot-server.kezio-system.svc:8090" {
		t.Errorf("Proxy.BootUpstreamURL = %q", cfg.Proxy.BootUpstreamURL)
	}
}

// TestBootdConfigFromEnv_ProxyBothUpstreams proves both upstream URLs
// can be set together and are carried through independently.
func TestBootdConfigFromEnv_ProxyBothUpstreams(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_AGENT_UPSTREAM_URL", "http://agent.example:8091")
	t.Setenv("BOOTD_BOOT_UPSTREAM_URL", "http://boot.example:8090")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Proxy.AgentUpstreamURL != "http://agent.example:8091" {
		t.Errorf("Proxy.AgentUpstreamURL = %q", cfg.Proxy.AgentUpstreamURL)
	}
	if cfg.Proxy.BootUpstreamURL != "http://boot.example:8090" {
		t.Errorf("Proxy.BootUpstreamURL = %q", cfg.Proxy.BootUpstreamURL)
	}
}

// TestBootdConfigFromEnv_HTTPBootURLDerivedFromProxy proves a deployment
// that already proxies the boot server gets the UEFI HTTP Boot answer
// without setting anything new: the URL points back at this bootd's own
// proxy address, which is the only address a machine on the provisioning
// segment is guaranteed to reach.
func TestBootdConfigFromEnv_HTTPBootURLDerivedFromProxy(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_BOOT_UPSTREAM_URL", "http://kezio-boot-server.kezio-system.svc:8090")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	const want = "http://192.0.2.2/boot/http/shimx64.efi"
	if cfg.Server.HTTPBootURL != want {
		t.Errorf("Server.HTTPBootURL = %q, want %q", cfg.Server.HTTPBootURL, want)
	}
}

// TestBootdConfigFromEnv_HTTPBootURLNeedsABootUpstream: with no boot
// server behind it, /boot/http/<name> is not proxied at all, so
// advertising the URL would send firmware to a 404 - worse than the PXE
// answer it already has.
func TestBootdConfigFromEnv_HTTPBootURLNeedsABootUpstream(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_AGENT_UPSTREAM_URL", "http://kezio-agent-server.kezio-system.svc:8091")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Server.HTTPBootURL != "" {
		t.Errorf("Server.HTTPBootURL = %q with no boot upstream, want empty", cfg.Server.HTTPBootURL)
	}
}

// TestBootdConfigFromEnv_HTTPBootURLExplicitOverride covers the site
// whose EFI binaries are fronted somewhere other than this pod.
func TestBootdConfigFromEnv_HTTPBootURLExplicitOverride(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_BOOT_UPSTREAM_URL", "http://kezio-boot-server.kezio-system.svc:8090")
	t.Setenv("BOOTD_HTTP_BOOT_URL", "http://192.0.2.9:8090/boot/http/shimx64.efi")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Server.HTTPBootURL != "http://192.0.2.9:8090/boot/http/shimx64.efi" {
		t.Errorf("Server.HTTPBootURL = %q, want the explicit override", cfg.Server.HTTPBootURL)
	}
}

// TestBootdConfigFromEnv_HTTPBootURLFollowsBootFilename: the derived URL
// must name the same file the PXE answer does, or the two paths boot
// different binaries.
func TestBootdConfigFromEnv_HTTPBootURLFollowsBootFilename(t *testing.T) {
	clearBootdEnv(t)
	requiredBootdEnv(t)
	t.Setenv("BOOTD_BOOT_UPSTREAM_URL", "http://kezio-boot-server.kezio-system.svc:8090")
	t.Setenv("BOOTD_BOOT_FILENAME", "other.efi")

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		t.Fatalf("bootdConfigFromEnv: %v", err)
	}
	if cfg.Server.BootFilename != "other.efi" {
		t.Errorf("Server.BootFilename = %q, want %q", cfg.Server.BootFilename, "other.efi")
	}
	const want = "http://192.0.2.2/boot/http/other.efi"
	if cfg.Server.HTTPBootURL != want {
		t.Errorf("Server.HTTPBootURL = %q, want %q", cfg.Server.HTTPBootURL, want)
	}
}
