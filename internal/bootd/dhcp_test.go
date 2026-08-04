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
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// setGate is a MACGate backed by an explicit allowlist, for tests.
type setGate map[string]bool

func (g setGate) Allow(mac net.HardwareAddr) bool { return g[mac.String()] }

var knownMAC = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}
var unknownMAC = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02}

var knownGate = setGate{knownMAC.String(): true}

// wireRoundtrip builds a DHCPv4 packet from modifiers, serializes it,
// and re-parses it - so tests exercise BuildResponse against exactly
// the same byte-level packet a real firmware's request would produce,
// not an in-memory struct that skipped encoding.
func wireRoundtrip(t *testing.T, modifiers ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	pkt, err := dhcpv4.New(modifiers...)
	if err != nil {
		t.Fatalf("building fixture packet: %v", err)
	}
	parsed, err := dhcpv4.FromBytes(pkt.ToBytes())
	if err != nil {
		t.Fatalf("round-tripping fixture packet: %v", err)
	}
	return parsed
}

func pxeDiscover(t *testing.T, hwaddr net.HardwareAddr, arch iana.Arch, extra ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	base := make([]dhcpv4.Modifier, 0, 4+len(extra))
	base = append(base,
		dhcpv4.WithHwAddr(hwaddr),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(pxeVendorClass)),
		dhcpv4.WithOption(dhcpv4.OptClientArch(arch)),
	)
	return wireRoundtrip(t, append(base, extra...)...)
}

func httpBootDiscover(t *testing.T, hwaddr net.HardwareAddr, arch iana.Arch, classID string) *dhcpv4.DHCPv4 {
	t.Helper()
	return wireRoundtrip(t,
		dhcpv4.WithHwAddr(hwaddr),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(classID)),
		dhcpv4.WithOption(dhcpv4.OptClientArch(arch)),
	)
}

func testConfig() Config {
	return Config{
		ServerIP: net.IPv4(10, 0, 0, 5),
		TFTPDir:  "/tftp",
	}
}

var srcAddr = &net.UDPAddr{IP: net.IPv4(10, 0, 0, 50), Port: dhcpv4.ClientPort}

func TestBuildResponse_AnswersKnownMACSupportedArch(t *testing.T) {
	for _, arch := range []iana.Arch{iana.EFI_X86_64, iana.EFI_BC} {
		req := pxeDiscover(t, knownMAC, arch)
		resp, dst, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)

		if outcome != OutcomeAnswered {
			t.Fatalf("arch %v: outcome = %v, want %v", arch, outcome, OutcomeAnswered)
		}
		if resp == nil {
			t.Fatalf("arch %v: resp is nil", arch)
		}
		if !resp.YourIPAddr.Equal(net.IPv4zero) {
			t.Errorf("arch %v: YourIPAddr = %v, want 0.0.0.0 (no lease)", arch, resp.YourIPAddr)
		}
		if resp.MessageType() != dhcpv4.MessageTypeOffer {
			t.Errorf("arch %v: MessageType = %v, want Offer", arch, resp.MessageType())
		}
		if resp.BootFileName != DefaultBootFilename {
			t.Errorf("arch %v: BootFileName = %q, want %q", arch, resp.BootFileName, DefaultBootFilename)
		}
		if len(resp.GetOneOption(dhcpv4.OptionBootfileName)) == 0 {
			t.Errorf("arch %v: option 67 (bootfile name) missing from response", arch)
		}
		if !resp.ServerIPAddr.Equal(testConfig().ServerIP) {
			t.Errorf("arch %v: ServerIPAddr (siaddr/next-server) = %v, want %v", arch, resp.ServerIPAddr, testConfig().ServerIP)
		}
		if !dst.IP.Equal(net.IPv4bcast) || dst.Port != dhcpv4.ClientPort {
			t.Errorf("arch %v: dst = %v, want broadcast:%d (no giaddr, RoleProxyDHCP)", arch, dst, dhcpv4.ClientPort)
		}
	}
}

func TestBuildResponse_RelayAware(t *testing.T) {
	giaddr := net.IPv4(192, 168, 1, 1)
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64, dhcpv4.WithGatewayIP(giaddr))

	resp, dst, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeAnswered {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeAnswered)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	if !dst.IP.Equal(giaddr) || dst.Port != dhcpv4.ServerPort {
		t.Errorf("dst = %v, want unicast %v:%d (relay-aware)", dst, giaddr, dhcpv4.ServerPort)
	}
}

func TestBuildResponse_UnsupportedArchIgnored(t *testing.T) {
	req := pxeDiscover(t, knownMAC, iana.INTEL_X86PC)
	resp, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeUnsupportedArch {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeUnsupportedArch)
	}
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
}

func TestBuildResponse_UnknownMACIgnoredByDefault(t *testing.T) {
	req := pxeDiscover(t, unknownMAC, iana.EFI_X86_64)
	resp, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeUnknownMAC {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeUnknownMAC)
	}
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
}

func TestBuildResponse_AnswerAllBypassesGate(t *testing.T) {
	req := pxeDiscover(t, unknownMAC, iana.EFI_X86_64)
	cfg := testConfig()
	cfg.AnswerAll = true
	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, cfg, knownGate)
	if outcome != OutcomeAnswered {
		t.Errorf("outcome = %v, want %v (AnswerAll should bypass the gate)", outcome, OutcomeAnswered)
	}
}

func TestBuildResponse_NonPXEVendorClassIgnored(t *testing.T) {
	req := wireRoundtrip(t,
		dhcpv4.WithHwAddr(knownMAC),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.EFI_X86_64)),
	)
	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeNotPXE {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeNotPXE)
	}
}

func TestBuildResponse_ProxyDHCPPortIgnoresRequest(t *testing.T) {
	// A DHCPREQUEST on port 67 is production DHCP's lease handshake;
	// bootd must not answer it there (see BuildResponse's doc comment).
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64, dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeWrongMessageType {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeWrongMessageType)
	}
}

func TestBuildResponse_PXEPortAnswersRequestUnicast(t *testing.T) {
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64, dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	resp, dst, outcome := BuildResponse(req, RolePXE, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeAnswered {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeAnswered)
	}
	if resp.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want Ack", resp.MessageType())
	}
	if dst.String() != srcAddr.String() {
		t.Errorf("dst = %v, want the request's own source address %v (no giaddr)", dst, srcAddr)
	}
}

func TestBuildResponse_PXEPortIgnoresDiscover(t *testing.T) {
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64) // MessageTypeDiscover
	_, _, outcome := BuildResponse(req, RolePXE, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeWrongMessageType {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeWrongMessageType)
	}
}

func TestBuildResponse_NilGateWithAnswerAllFalseDeniesEverything(t *testing.T) {
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64)
	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), nil)
	if outcome != OutcomeUnknownMAC {
		t.Errorf("outcome = %v, want %v (nil gate defaults to deny-all via alwaysAllow{} substitution only under AnswerAll)", outcome, OutcomeUnknownMAC)
	}
}

const testHTTPBootURL = "http://10.0.0.5/boot/http/shimx64.efi"

func TestBuildResponse_HTTPClientWithConfiguredURLAnswersWithURL(t *testing.T) {
	req := httpBootDiscover(t, knownMAC, iana.EFI_X86_64, httpClientVendorClass)
	cfg := testConfig()
	cfg.HTTPBootURL = testHTTPBootURL

	resp, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, cfg, knownGate)
	if outcome != OutcomeAnswered {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeAnswered)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}

	// Boot filename (both option 67 and the legacy BOOTP "file" field)
	// must be the configured URL, not the TFTP shim filename.
	if resp.BootFileName != testHTTPBootURL {
		t.Errorf("BootFileName = %q, want %q", resp.BootFileName, testHTTPBootURL)
	}
	opt67 := resp.GetOneOption(dhcpv4.OptionBootfileName)
	if string(opt67) != testHTTPBootURL {
		t.Errorf("option 67 bytes = %q, want %q", string(opt67), testHTTPBootURL)
	}

	// Option 60 must echo "HTTPClient" verbatim - firmware requires
	// this exact echo to accept the offer.
	opt60 := resp.GetOneOption(dhcpv4.OptionClassIdentifier)
	if string(opt60) != httpClientVendorClass {
		t.Errorf("option 60 bytes = %q, want %q", string(opt60), httpClientVendorClass)
	}
}

func TestBuildResponse_HTTPClientSuffixedFormAccepted(t *testing.T) {
	// The UEFI spec allows firmware to suffix the class with its
	// architecture, e.g. "HTTPClient:Arch:00016"; bootd matches by
	// prefix, so this form must be recognized the same as the bare one.
	req := httpBootDiscover(t, knownMAC, iana.EFI_X86_64, "HTTPClient:Arch:00016")
	cfg := testConfig()
	cfg.HTTPBootURL = testHTTPBootURL

	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, cfg, knownGate)
	if outcome != OutcomeAnswered {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeAnswered)
	}
}

func TestBuildResponse_HTTPClientWithoutURLConfiguredDeclines(t *testing.T) {
	req := httpBootDiscover(t, knownMAC, iana.EFI_X86_64, httpClientVendorClass)
	// testConfig() leaves HTTPBootURL unset: HTTP Boot is opt-in.
	resp, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, testConfig(), knownGate)
	if outcome != OutcomeHTTPBootUnconfigured {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeHTTPBootUnconfigured)
	}
	if resp != nil {
		t.Errorf("resp = %v, want nil (must not hand out a TFTP filename an HTTP client cannot use)", resp)
	}
}

func TestBuildResponse_HTTPClientUnsupportedArchIgnored(t *testing.T) {
	// The arch-93 gate applies identically to HTTP Boot clients.
	req := httpBootDiscover(t, knownMAC, iana.INTEL_X86PC, httpClientVendorClass)
	cfg := testConfig()
	cfg.HTTPBootURL = testHTTPBootURL

	_, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, cfg, knownGate)
	if outcome != OutcomeUnsupportedArch {
		t.Errorf("outcome = %v, want %v", outcome, OutcomeUnsupportedArch)
	}
}

func TestBuildResponse_PXEClientUnaffectedByHTTPBootConfig(t *testing.T) {
	// Regression: configuring HTTPBootURL must not change what a
	// PXEClient request gets back - the two vendor-class paths coexist
	// without interfering with each other.
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64)
	cfg := testConfig()
	cfg.HTTPBootURL = testHTTPBootURL

	resp, _, outcome := BuildResponse(req, RoleProxyDHCP, srcAddr, cfg, knownGate)
	if outcome != OutcomeAnswered {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeAnswered)
	}
	if resp.BootFileName != DefaultBootFilename {
		t.Errorf("BootFileName = %q, want %q (TFTP shim, unaffected by HTTPBootURL)", resp.BootFileName, DefaultBootFilename)
	}
	opt60 := resp.GetOneOption(dhcpv4.OptionClassIdentifier)
	if string(opt60) != pxeVendorClass {
		t.Errorf("option 60 bytes = %q, want %q", string(opt60), pxeVendorClass)
	}
}
