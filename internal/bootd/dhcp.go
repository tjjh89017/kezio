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

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// pxeVendorClass is the option 60 (Vendor Class Identifier) value every
// PXE-capable UEFI firmware sends. bootd only ever answers a request
// that carries it - anything else is not a PXE boot attempt at all
// (an ordinary DHCP client sharing the same broadcast domain, for
// example), and answering it would be pure noise at best.
const pxeVendorClass = "PXEClient"

// supportedArchs are the RFC 4578 client system architectures
// (DHCP option 93) this deployment ships a loader for: x86-64 UEFI
// native and EFI Byte Code, both of which chainload the same
// shimx64.efi (see DefaultBootFilename). Every other architecture is
// ignored - not answered, not erred - so a legacy BIOS client or an
// ARM client on the same segment silently falls through to whatever
// else might answer it, instead of receiving a boot file that does not
// exist for its platform.
var supportedArchs = []iana.Arch{iana.EFI_X86_64, iana.EFI_BC}

// Role names which of the two listeners (see the package doc comment)
// received a request, since the two ports answer different DHCP
// message types and destination rules (see BuildResponse).
type Role int

const (
	// RoleProxyDHCP is the UDP/67 listener: answers DHCPDISCOVER.
	RoleProxyDHCP Role = iota
	// RolePXE is the UDP/4011 listener: answers DHCPREQUEST.
	RolePXE
)

// MACGate decides whether bootd is willing to answer the client
// identified by mac. See maccache.go for the Kubernetes-backed
// implementation; Config.AnswerAll bypasses this decision entirely
// when set.
type MACGate interface {
	Allow(mac net.HardwareAddr) bool
}

// alwaysAllow is the MACGate used when Config.AnswerAll is set.
type alwaysAllow struct{}

func (alwaysAllow) Allow(net.HardwareAddr) bool { return true }

// Outcome names why BuildResponse did or did not produce a response,
// for logging by the socket loop (server.go) and assertions in tests.
type Outcome string

const (
	// OutcomeAnswered means resp is non-nil and should be sent to dst.
	OutcomeAnswered Outcome = "answered"
	// OutcomeNotPXE means the request carried no PXEClient vendor
	// class option (option 60): not a PXE boot attempt.
	OutcomeNotPXE Outcome = "not-pxe-client"
	// OutcomeUnsupportedArch means option 93 named an architecture
	// this deployment has no loader for.
	OutcomeUnsupportedArch Outcome = "unsupported-arch"
	// OutcomeUnknownMAC means the MACGate declined the client's MAC.
	OutcomeUnknownMAC Outcome = "unknown-mac"
	// OutcomeWrongMessageType means the request's DHCP message type is
	// not one this port answers (see BuildResponse's port-specific
	// rules).
	OutcomeWrongMessageType Outcome = "wrong-message-type"
)

// BuildResponse is the packet-handling core shared by the UDP/67 and
// UDP/4011 listeners (see the package doc comment): given a parsed
// request, which listener received it, the server's Config, and the
// MACGate to consult, it decides whether to answer at all and, if so,
// builds the exact response packet and the UDP address to send it to.
// It opens no socket and has no side effects, so every decision branch
// is directly unit-testable.
//
// role distinguishes the two roles a proxyDHCP setup plays:
//
//   - RoleProxyDHCP (port 67): answers DHCPDISCOVER only, with a
//     DHCPOFFER-shaped reply. bootd never answers DHCPREQUEST on this
//     port - that message is production DHCP's lease handshake, which
//     bootd has no part in (see the package doc comment: no IP
//     leases).
//   - RolePXE (port 4011): answers DHCPREQUEST only, the PXE client's
//     second-phase boot-server query once it already holds a real
//     lease from production DHCP.
//
// Relay awareness: when req.GatewayIPAddr is set (a DHCP relay agent
// forwarded this request), the response is unicast back to that
// relay's address on the DHCP server port, exactly as RFC 2131 requires
// - the relay, not bootd, is responsible for delivering it on to the
// client from there. When GatewayIPAddr is unset, port 67 responses are
// broadcast (the client has no IP yet to unicast to) and port 4011
// responses are unicast back to srcAddr (the client already holds a
// lease by the time it reaches this port).
func BuildResponse(req *dhcpv4.DHCPv4, role Role, srcAddr *net.UDPAddr, cfg Config, gate MACGate) (resp *dhcpv4.DHCPv4, dst *net.UDPAddr, outcome Outcome) {
	cfg = cfg.withDefaults()
	switch {
	case cfg.AnswerAll:
		gate = alwaysAllow{}
	case gate == nil:
		// Fail secure at the core, not only in Server.resolveGate: a
		// nil MACGate with AnswerAll unset must deny every client, the
		// same as an unsynced MACCache would (see maccache.go) - never
		// silently answer everyone just because no gate was wired in.
		gate = denyAll{}
	}

	wantType := dhcpv4.MessageTypeDiscover
	if role == RolePXE {
		wantType = dhcpv4.MessageTypeRequest
	}
	if req.MessageType() != wantType {
		return nil, nil, OutcomeWrongMessageType
	}

	if req.ClassIdentifier() != pxeVendorClass {
		return nil, nil, OutcomeNotPXE
	}

	if !archSupported(req.ClientArch()) {
		return nil, nil, OutcomeUnsupportedArch
	}

	if !gate.Allow(req.ClientHWAddr) {
		return nil, nil, OutcomeUnknownMAC
	}

	respType := dhcpv4.MessageTypeOffer
	if wantType == dhcpv4.MessageTypeRequest {
		respType = dhcpv4.MessageTypeAck
	}

	modifiers := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(respType),
		// No lease: this is the one invariant every response this
		// package sends must preserve (see the package doc comment) -
		// bootd is not a DHCP server and never assigns an address.
		dhcpv4.WithYourIP(net.IPv4zero),
		dhcpv4.WithServerIP(cfg.NextServerIP),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(cfg.ServerIP)),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(pxeVendorClass)),
		dhcpv4.WithOption(dhcpv4.OptBootFileName(cfg.BootFilename)),
	}
	resp, err := dhcpv4.NewReplyFromRequest(req, modifiers...)
	if err != nil {
		// NewReplyFromRequest only fails building the underlying
		// packet (for example an oversized option); every input here
		// is ours or already validated above, so treat it the same as
		// any other "do not answer" outcome rather than panicking or
		// inventing a new Outcome for a case that should not occur.
		return nil, nil, OutcomeWrongMessageType
	}
	// PXE firmware conventionally also reads the legacy BOOTP "file"
	// field, not only option 67 (OptBootFileName above); set both so
	// firmware that only looks at one of them still boots.
	resp.BootFileName = cfg.BootFilename

	return resp, destinationFor(req, role, srcAddr), OutcomeAnswered
}

// archSupported reports whether archs contains any of supportedArchs.
// A client can advertise more than one architecture; matching any one
// of them is enough, since the loader offered (shimx64.efi) works for
// both entries in supportedArchs.
func archSupported(archs []iana.Arch) bool {
	for _, got := range archs {
		for _, want := range supportedArchs {
			if got == want {
				return true
			}
		}
	}
	return false
}

// destinationFor picks the UDP address BuildResponse's caller should
// send resp to. See BuildResponse's doc comment for the relay-aware
// rule this implements.
func destinationFor(req *dhcpv4.DHCPv4, role Role, srcAddr *net.UDPAddr) *net.UDPAddr {
	if req.GatewayIPAddr != nil && !req.GatewayIPAddr.IsUnspecified() {
		return &net.UDPAddr{IP: req.GatewayIPAddr, Port: dhcpv4.ServerPort}
	}
	if role == RoleProxyDHCP {
		// The client has no IP yet: broadcast is the only address that
		// reaches it directly on this L2 segment.
		return &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}
	}
	// Port 4011: the client already holds a real lease and reached us
	// by unicast, so reply to exactly where it came from.
	return srcAddr
}
