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

package bootd

import (
	"net"
	"strings"
	"testing"
)

func testConfig() Config {
	_, ipNet, _ := net.ParseCIDR("192.0.2.0/24")
	return Config{
		Interface:       "net1",
		ServerIP:        net.ParseIP("192.0.2.2"),
		ProvisioningNet: ipNet,
		TFTPDir:         "/tftp",
	}
}

func TestRenderDnsmasqConf_ProxyOnly(t *testing.T) {
	conf, err := RenderDnsmasqConf(testConfig(), "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}

	for _, want := range []string{
		"port=0\n",
		"bind-interfaces\n",
		"interface=net1\n",
		"dhcp-leasefile=/run/bootd/dnsmasq.leases\n",
		"dhcp-range=192.0.2.0,proxy,255.255.255.0\n",
		"dhcp-hostsfile=/run/bootd/dhcp-hosts.conf\n",
		`pxe-service=tag:kezio,x86-64_EFI,"kezio network boot",shimx64.efi,192.0.2.2` + "\n",
		"dhcp-ignore=tag:!kezio\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("rendered config missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "dhcp-relay=") {
		t.Errorf("rendered config contains dhcp-relay with no relay configured:\n%s", conf)
	}
	if strings.Contains(conf, "enable-tftp") {
		t.Errorf("rendered config enables dnsmasq TFTP; TFTP is served in-process:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_Relay(t *testing.T) {
	cfg := testConfig()
	cfg.RelayServerIP = net.ParseIP("10.0.0.1")
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, "dhcp-relay=192.0.2.2,10.0.0.1\n") {
		t.Errorf("rendered config missing dhcp-relay line:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_NextServerOverridesPXEServiceAddress(t *testing.T) {
	cfg := testConfig()
	cfg.NextServerIP = net.ParseIP("192.0.2.9")
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, `pxe-service=tag:kezio,x86-64_EFI,"kezio network boot",shimx64.efi,192.0.2.9`+"\n") {
		t.Errorf("pxe-service does not use NextServerIP:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_BootFilenameOverride(t *testing.T) {
	cfg := testConfig()
	cfg.BootFilename = "other.efi"
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, `"kezio network boot",other.efi,192.0.2.2`+"\n") {
		t.Errorf("pxe-service does not carry the overridden boot filename:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_AnswerAllDropsMACGate(t *testing.T) {
	cfg := testConfig()
	cfg.AnswerAll = true
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if strings.Contains(conf, "dhcp-ignore=") {
		t.Errorf("AnswerAll config still carries dhcp-ignore:\n%s", conf)
	}
	if !strings.Contains(conf, `pxe-service=x86-64_EFI,"kezio network boot",shimx64.efi,192.0.2.2`+"\n") {
		t.Errorf("AnswerAll pxe-service still tag-gated:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_NoInterface(t *testing.T) {
	cfg := testConfig()
	cfg.Interface = ""
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if strings.Contains(conf, "interface=") || strings.Contains(conf, "bind-interfaces") {
		t.Errorf("empty Interface must render no interface pinning:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_RangeUsesNetworkAddress(t *testing.T) {
	cfg := testConfig()
	// A host address inside the subnet, not the network address: the
	// renderer must still emit the network address in dhcp-range.
	_, ipNet, _ := net.ParseCIDR("10.1.2.0/23")
	ipNet.IP = net.ParseIP("10.1.3.7").To4()
	cfg.ProvisioningNet = ipNet
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, "dhcp-range=10.1.2.0,proxy,255.255.254.0\n") {
		t.Errorf("dhcp-range not normalized to the network address:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_Validation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing ServerIP", func(c *Config) { c.ServerIP = nil }},
		{"missing ProvisioningNet", func(c *Config) { c.ProvisioningNet = nil }},
		{"IPv6 ServerIP", func(c *Config) { c.ServerIP = net.ParseIP("2001:db8::1") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)
			if _, err := RenderDnsmasqConf(cfg, "/run/bootd"); err == nil {
				t.Error("RenderDnsmasqConf accepted an invalid config")
			}
		})
	}
}

func TestRenderHostsfile(t *testing.T) {
	tests := []struct {
		name string
		macs []string
		want string
	}{
		{"empty means nothing boots", nil, ""},
		{"one MAC", []string{"aa:bb:cc:dd:ee:01"}, "aa:bb:cc:dd:ee:01,set:kezio\n"},
		{
			"sorted and deduplicated",
			[]string{"aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
			"aa:bb:cc:dd:ee:01,set:kezio\naa:bb:cc:dd:ee:02,set:kezio\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderHostsfile(tc.macs); got != tc.want {
				t.Errorf("RenderHostsfile(%v) = %q, want %q", tc.macs, got, tc.want)
			}
		})
	}
}

// TestRenderGrubConfig pins the exact bootstrap config TFTPServer hands
// the netboot GRUB image at GrubConfigPath: one configfile line in
// GRUB's network file syntax (bare URLs are unopenable by GRUB - see
// bootserver.GrubNetPath), pointing at the boot config server's per-MAC
// route with ${net_default_mac} left for the client to expand. No boot
// logic beyond the handoff - that all lives in the fetched config.
func TestRenderGrubConfig(t *testing.T) {
	got, err := RenderGrubConfig("http://192.0.2.1:8090")
	if err != nil {
		t.Fatalf("RenderGrubConfig: %v", err)
	}
	want := "# Generated by kezio-bootd - do not edit.\n" +
		"configfile (http,192.0.2.1:8090)/boot/grub.cfg-${net_default_mac}\n"
	if got != want {
		t.Errorf("RenderGrubConfig = %q, want %q", got, want)
	}
}

// TestRenderGrubConfig_RejectsNonHTTP proves a boot config URL GRUB
// could never fetch from (its netboot image has no TLS stack) fails at
// render time - bootd's startup - not as a silent boot-time dead end.
func TestRenderGrubConfig_RejectsNonHTTP(t *testing.T) {
	for _, url := range []string{"https://192.0.2.1:8090", "not a url", ""} {
		if got, err := RenderGrubConfig(url); err == nil {
			t.Errorf("RenderGrubConfig(%q) = %q, want error", url, got)
		}
	}
}
