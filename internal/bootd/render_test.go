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

package bootd

import (
	"net"
	"strings"
	"testing"
	"time"
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
		"dhcp-ignore-clid\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("rendered config missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "enable-tftp") {
		t.Errorf("rendered config enables dnsmasq TFTP; TFTP is served in-process:\n%s", conf)
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
	if !strings.Contains(conf, "dhcp-ignore-clid\n") {
		t.Errorf("AnswerAll config drops dhcp-ignore-clid:\n%s", conf)
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

func TestRenderDnsmasqConf_LeaseModeAutoDerivesRange(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}

	for _, want := range []string{
		"dhcp-range=192.0.2.1,192.0.2.254,30m\n",
		"dhcp-boot=shimx64.efi,,192.0.2.2\n",
		"dhcp-match=set:efi-x86_64,option:client-arch,7\n",
		"dhcp-boot=tag:efi-x86_64,shimx64.efi,,192.0.2.2\n",
		"dhcp-ignore=tag:!kezio\n",
		"dhcp-ignore-clid\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("lease-mode config missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "dhcp-range=192.0.2.0,proxy") {
		t.Errorf("lease-mode config still renders a proxy dhcp-range:\n%s", conf)
	}
	if strings.Contains(conf, "pxe-service=") {
		t.Errorf("lease-mode config still renders pxe-service, which does not work outside proxy mode:\n%s", conf)
	}
}

func TestRenderDnsmasqConf_LeaseModeExplicitRange(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	cfg.LeaseRangeStart = net.ParseIP("192.0.2.50")
	cfg.LeaseRangeEnd = net.ParseIP("192.0.2.60")
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, "dhcp-range=192.0.2.50,192.0.2.60,30m\n") {
		t.Errorf("lease-mode config does not honor the explicit range:\n%s", conf)
	}
}

// TestRenderDnsmasqConf_LeaseTime pins the lease time rendered into
// dhcp-range: unset defaults to 30 minutes, and dnsmasq's accepted
// forms (hours, minutes, seconds) each render correctly.
func TestRenderDnsmasqConf_LeaseTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want string
	}{
		{"unset defaults to 30m", 0, "30m"},
		{"whole hours", 2 * time.Hour, "2h"},
		{"whole minutes", 45 * time.Minute, "45m"},
		{"seconds fallback", 90 * time.Second, "90"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.LeaseMode = true
			cfg.LeaseTime = tc.d
			conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
			if err != nil {
				t.Fatalf("RenderDnsmasqConf: %v", err)
			}
			want := "dhcp-range=192.0.2.1,192.0.2.254," + tc.want + "\n"
			if !strings.Contains(conf, want) {
				t.Errorf("rendered config missing %q:\n%s", want, conf)
			}
		})
	}
}

// TestRenderDnsmasqConf_LeaseFilePathOverride proves Config.LeaseFilePath
// replaces the default runDir/dnsmasq.leases path - the persistent lease
// PVC mount lease mode uses.
func TestRenderDnsmasqConf_LeaseFilePathOverride(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseFilePath = "/var/lib/bootd-leases/dnsmasq.leases"
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, "dhcp-leasefile=/var/lib/bootd-leases/dnsmasq.leases\n") {
		t.Errorf("rendered config does not honor LeaseFilePath:\n%s", conf)
	}
	if strings.Contains(conf, "dhcp-leasefile=/run/bootd/dnsmasq.leases\n") {
		t.Errorf("rendered config still points at the default leasefile path:\n%s", conf)
	}
}

// TestRenderDnsmasqConf_GatewayStates walks Config.Gateway's three
// states in lease mode and pins that each changes the rendered config by
// exactly its own routing line and nothing else - so a later refactor
// cannot quietly alter what a machine's lease says about leaving its
// segment.
func TestRenderDnsmasqConf_GatewayStates(t *testing.T) {
	base := testConfig()
	base.LeaseMode = true
	// The nil rendering is the baseline the other states are diffed
	// against: no routing line at all, which is what every lease rendered
	// before this option existed.
	baseline, err := RenderDnsmasqConf(base, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if strings.Contains(baseline, "dhcp-option=3") {
		t.Fatalf("a nil Gateway must render no routing option:\n%s", baseline)
	}

	for _, tc := range []struct {
		name     string
		gateway  string
		wantLine string
	}{
		{
			name:     "an address is the segment's router",
			gateway:  "192.0.2.1",
			wantLine: "dhcp-option=3,192.0.2.1\n",
		},
		{
			// dnsmasq's suppression form: declaring the option with no
			// value stops dnsmasq substituting its own address for it.
			name:     "the empty string declares a segment with no exit",
			gateway:  "",
			wantLine: "dhcp-option=3\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Gateway = &tc.gateway
			conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
			if err != nil {
				t.Fatalf("RenderDnsmasqConf: %v", err)
			}
			if !strings.Contains(conf, tc.wantLine) {
				t.Errorf("rendered config missing %q:\n%s", tc.wantLine, conf)
			}
			if got := strings.Replace(conf, tc.wantLine, "", 1); got != baseline {
				t.Errorf("a Gateway changed the config beyond its own routing line:\nbaseline:\n%s\ngot:\n%s", baseline, conf)
			}
		})
	}
}

// TestRenderDnsmasqConf_GatewayRejectsNonIPv4: the CRD pattern rejects
// these already, so reaching the renderer means a Config was assembled
// some other way - and a bad address must not become a silently
// malformed dnsmasq line.
func TestRenderDnsmasqConf_GatewayRejectsNonIPv4(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	for _, bad := range []string{"not-an-ip", "2001:db8::1", "192.0.2"} {
		cfg.Gateway = &bad
		if _, err := RenderDnsmasqConf(cfg, "/run/bootd"); err == nil {
			t.Errorf("Gateway %q rendered without error, want a rejection", bad)
		}
	}
}

// TestRenderDnsmasqConf_ProxyModeRendersNoRouterOption: in proxy mode
// bootd is not the DHCP server - the segment's own one is, and it owns
// every routing option in the lease. So no Gateway state renders
// anything here, and each must leave the config byte-identical to the
// one that names no Gateway at all.
func TestRenderDnsmasqConf_ProxyModeRendersNoRouterOption(t *testing.T) {
	baseline, err := RenderDnsmasqConf(testConfig(), "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}

	for _, gateway := range []string{"192.0.2.1", ""} {
		cfg := testConfig()
		cfg.Gateway = &gateway
		conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
		if err != nil {
			t.Fatalf("RenderDnsmasqConf: %v", err)
		}
		if strings.Contains(conf, "dhcp-option=3") {
			t.Errorf("proxy-mode config renders a routing option, overriding the segment's own DHCP server:\n%s", conf)
		}
		if conf != baseline {
			t.Errorf("a proxy-mode Gateway %q changed the rendered config:\nbaseline:\n%s\ngot:\n%s", gateway, baseline, conf)
		}
	}
}

// TestRenderDnsmasqConf_LeaseModeMACGateUnchanged: the same
// dhcp-hostsfile/dhcp-ignore pair gates lease mode exactly as proxy mode,
// and AnswerAll drops it the same way in both.
func TestRenderDnsmasqConf_LeaseModeMACGateUnchanged(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if !strings.Contains(conf, "dhcp-hostsfile=/run/bootd/dhcp-hosts.conf\n") {
		t.Errorf("lease-mode config drops the dhcp-hostsfile MAC gate:\n%s", conf)
	}
	if !strings.Contains(conf, "dhcp-ignore=tag:!kezio\n") {
		t.Errorf("lease-mode config drops dhcp-ignore:\n%s", conf)
	}
	if !strings.Contains(conf, "dhcp-ignore-clid\n") {
		t.Errorf("lease-mode config drops dhcp-ignore-clid:\n%s", conf)
	}

	cfg.AnswerAll = true
	conf, err = RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}
	if strings.Contains(conf, "dhcp-ignore=") {
		t.Errorf("lease-mode AnswerAll config still carries dhcp-ignore:\n%s", conf)
	}
	if !strings.Contains(conf, "dhcp-ignore-clid\n") {
		t.Errorf("lease-mode AnswerAll config drops dhcp-ignore-clid:\n%s", conf)
	}
}

// TestRenderDnsmasqConf_LeaseModeHTTPBoot: an HTTP Boot client (client
// architecture 16) must be answered with the URL, carrying option 60
// HTTPClient so firmware recognizes the offer as an HTTP Boot one, while
// the PXE client's own filename/next-server answer stays exactly as it
// was.
func TestRenderDnsmasqConf_LeaseModeHTTPBoot(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	cfg.HTTPBootURL = "http://192.0.2.2/boot/http/shimx64.efi"
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}

	for _, want := range []string{
		"dhcp-match=set:httpclient,option:client-arch,16\n",
		"dhcp-option-force=tag:httpclient,60,HTTPClient\n",
		"dhcp-boot=tag:httpclient,http://192.0.2.2/boot/http/shimx64.efi\n",
		"dhcp-match=set:efi-x86_64,option:client-arch,7\n",
		"dhcp-boot=tag:efi-x86_64,shimx64.efi,,192.0.2.2\n",
		"dhcp-boot=shimx64.efi,,192.0.2.2\n",
		"dhcp-ignore=tag:!kezio\n",
		"dhcp-ignore-clid\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("lease-mode HTTP Boot config missing %q:\n%s", want, conf)
		}
	}
	// Architecture 15 is 32-bit x86 UEFI HTTP, which cannot load the
	// x86-64 shim this hands out.
	if strings.Contains(conf, "option:client-arch,15") {
		t.Errorf("config answers client architecture 15 with an x86-64 boot file:\n%s", conf)
	}
}

// TestRenderDnsmasqConf_ProxyModeHTTPBoot: proxy mode needs
// dhcp-pxe-vendor to make dnsmasq's proxyDHCP engine engage for an
// HTTPClient request at all (it only answers PXEClient by default), and
// must keep PXEClient in that same list so ordinary PXE clients are still
// answered.
func TestRenderDnsmasqConf_ProxyModeHTTPBoot(t *testing.T) {
	cfg := testConfig()
	cfg.HTTPBootURL = "http://192.0.2.2/boot/http/shimx64.efi"
	conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
	if err != nil {
		t.Fatalf("RenderDnsmasqConf: %v", err)
	}

	for _, want := range []string{
		"dhcp-pxe-vendor=PXEClient,HTTPClient\n",
		"dhcp-match=set:httpclient,option:client-arch,16\n",
		"dhcp-boot=tag:httpclient,http://192.0.2.2/boot/http/shimx64.efi\n",
		`pxe-service=tag:kezio,x86-64_EFI,"kezio network boot",shimx64.efi,192.0.2.2` + "\n",
		"dhcp-range=192.0.2.0,proxy,255.255.255.0\n",
		"dhcp-ignore=tag:!kezio\n",
		"dhcp-ignore-clid\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("proxy-mode HTTP Boot config missing %q:\n%s", want, conf)
		}
	}
	// The proxy path echoes the matched vendor class back as option 60
	// itself (dnsmasq's pxe_misc), so forcing it would be a second,
	// conflicting copy.
	if strings.Contains(conf, "dhcp-option-force=tag:httpclient,60") {
		t.Errorf("proxy-mode config forces option 60, which the proxy path already sets:\n%s", conf)
	}
	// An untagged dhcp-boot in proxy mode would hand every PXE client the
	// HTTP Boot answer through find_boot's untagged fallback.
	if strings.Contains(conf, "dhcp-boot=http") {
		t.Errorf("proxy-mode config renders an untagged dhcp-boot:\n%s", conf)
	}
}

// TestRenderDnsmasqConf_NoHTTPBootURL proves an unset HTTPBootURL leaves
// both modes byte-identical to what they rendered before HTTP Boot
// existed - a deployment with no boot server to serve the artifacts must
// not advertise a URL that would 404.
func TestRenderDnsmasqConf_NoHTTPBootURL(t *testing.T) {
	for _, leaseMode := range []bool{false, true} {
		cfg := testConfig()
		cfg.LeaseMode = leaseMode
		conf, err := RenderDnsmasqConf(cfg, "/run/bootd")
		if err != nil {
			t.Fatalf("RenderDnsmasqConf(LeaseMode=%v): %v", leaseMode, err)
		}
		for _, unwanted := range []string{"httpclient", "HTTPClient", "dhcp-pxe-vendor"} {
			if strings.Contains(conf, unwanted) {
				t.Errorf("config with no HTTPBootURL (LeaseMode=%v) still carries %q:\n%s", leaseMode, unwanted, conf)
			}
		}
	}
}

// TestRenderDnsmasqConf_RejectsUnusableHTTPBootURL: dnsmasq copies the
// value straight into the offer's file field, so anything firmware cannot
// fetch fails here rather than as the same silent DISCOVER loop HTTP Boot
// support exists to end.
func TestRenderDnsmasqConf_RejectsUnusableHTTPBootURL(t *testing.T) {
	for _, rawURL := range []string{
		"shimx64.efi",
		"/boot/http/shimx64.efi",
		"tftp://192.0.2.2/shimx64.efi",
		"http:///boot/http/shimx64.efi",
		"://not a url",
	} {
		for _, leaseMode := range []bool{false, true} {
			cfg := testConfig()
			cfg.LeaseMode = leaseMode
			cfg.HTTPBootURL = rawURL
			if got, err := RenderDnsmasqConf(cfg, "/run/bootd"); err == nil {
				t.Errorf("RenderDnsmasqConf accepted HTTPBootURL %q (LeaseMode=%v):\n%s", rawURL, leaseMode, got)
			}
		}
	}
}

func TestRenderDnsmasqConf_LeaseModeRejectsOneSidedRange(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	cfg.LeaseRangeStart = net.ParseIP("192.0.2.50")
	if _, err := RenderDnsmasqConf(cfg, "/run/bootd"); err == nil {
		t.Error("RenderDnsmasqConf accepted a LeaseRangeStart with no LeaseRangeEnd")
	}
}

func TestRenderDnsmasqConf_LeaseModeRejectsTooSmallSubnet(t *testing.T) {
	cfg := testConfig()
	cfg.LeaseMode = true
	_, ipNet, _ := net.ParseCIDR("192.0.2.0/31")
	cfg.ProvisioningNet = ipNet
	if _, err := RenderDnsmasqConf(cfg, "/run/bootd"); err == nil {
		t.Error("RenderDnsmasqConf accepted auto-derivation on a /31 with no usable host addresses")
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
		name         string
		macs         []string
		reservations map[string]string
		want         string
	}{
		{"empty means nothing boots", nil, nil, ""},
		{"one MAC", []string{"aa:bb:cc:dd:ee:01"}, nil, "aa:bb:cc:dd:ee:01,set:kezio\n"},
		{
			"sorted and deduplicated",
			[]string{"aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
			nil,
			"aa:bb:cc:dd:ee:01,set:kezio\naa:bb:cc:dd:ee:02,set:kezio\n",
		},
		{
			"a reservation adds the pinned address",
			[]string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
			map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"},
			"aa:bb:cc:dd:ee:01,set:kezio,192.0.2.10\naa:bb:cc:dd:ee:02,set:kezio\n",
		},
		{
			"a reservation for a MAC not in the allowlist is dropped",
			[]string{"aa:bb:cc:dd:ee:01"},
			map[string]string{"aa:bb:cc:dd:ee:99": "192.0.2.10"},
			"aa:bb:cc:dd:ee:01,set:kezio\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderHostsfile(tc.macs, tc.reservations); got != tc.want {
				t.Errorf("RenderHostsfile(%v, %v) = %q, want %q", tc.macs, tc.reservations, got, tc.want)
			}
		})
	}
}

// TestRenderGrubConfig pins the bootstrap config at GrubConfigPath: one
// configfile line in GRUB's network file syntax pointing at the boot
// config server's per-MAC route, ${net_default_mac} left for the client.
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

// TestRenderGrubConfig_RejectsNonHTTP proves an unfetchable boot config
// URL (GRUB's netboot image has no TLS stack) fails at render time, not
// as a silent boot-time dead end.
func TestRenderGrubConfig_RejectsNonHTTP(t *testing.T) {
	for _, url := range []string{"https://192.0.2.1:8090", "not a url", ""} {
		if got, err := RenderGrubConfig(url); err == nil {
			t.Errorf("RenderGrubConfig(%q) = %q, want error", url, got)
		}
	}
}
