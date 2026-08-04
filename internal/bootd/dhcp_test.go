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
