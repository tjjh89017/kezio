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

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// pxeVendorClass is the option 60 (Vendor Class Identifier) prefix a
// PXE-capable UEFI firmware sends. Real firmware never sends the bare
// string: it appends its architecture and network-interface identifiers
// per RFC 4578, for example "PXEClient:Arch:00007:UNDI:003001". bootd
// therefore matches by prefix (see isPXEBootClass), exactly as it does
// for the HTTPClient class. A request carrying neither class is not a
// boot attempt bootd recognizes at all (an ordinary DHCP client sharing
// the same broadcast domain, for example), and answering it would be
// pure noise at best.
const pxeVendorClass = "PXEClient"

// httpClientVendorClass is the option 60 value UEFI firmware sends when
// it wants HTTP Boot instead of PXE+TFTP: a full HTTP(S) URL as the boot
// filename, fetched directly, no TFTP round trip. The UEFI spec allows
// firmware to suffix it with its architecture, for example
// "HTTPClient:Arch:00016" - bootd matches by prefix so both the bare
// form and any suffixed form are recognized the same way.
const httpClientVendorClass = "HTTPClient"

// isHTTPBootClass reports whether classID (option 60, as read off the
// wire) marks the request as a UEFI HTTP Boot attempt rather than a PXE
// one. See httpClientVendorClass's doc comment for the prefix form this
// matches.
func isHTTPBootClass(classID string) bool {
	return classID == httpClientVendorClass || strings.HasPrefix(classID, httpClientVendorClass+":")
}

// isPXEBootClass reports whether classID (option 60, as read off the
// wire) marks the request as a PXE boot attempt. Real UEFI firmware
// sends the RFC 4578 suffixed form ("PXEClient:Arch:...:UNDI:...")
// rather than the bare string, so this matches by prefix - mirroring
// isHTTPBootClass - instead of by exact equality, which would reject
// every legitimate PXE client. See pxeVendorClass's doc comment.
func isPXEBootClass(classID string) bool {
	return classID == pxeVendorClass || strings.HasPrefix(classID, pxeVendorClass+":")
}

// supportedArchs are the RFC 4578 client system architectures
// (DHCP option 93) this deployment ships a loader for: x86-64 UEFI
// native, EFI Byte Code, and x86-64 UEFI HTTP Boot, all of which
// chainload the same shimx64.efi (see DefaultBootFilename). The
// HTTP-boot arch is listed so an x86-64 firmware advertising it reaches
// the opt-in HTTP Boot gate (see OutcomeHTTPBootUnconfigured) and is
// declined for the right reason when HTTP Boot is off, rather than being
// rejected here as an unsupported architecture. Every other architecture
// is ignored - not answered, not erred - so a legacy BIOS client or an
// ARM client on the same segment silently falls through to whatever
// else might answer it, instead of receiving a boot file that does not
// exist for its platform.
var supportedArchs = []iana.Arch{iana.EFI_X86_64, iana.EFI_BC, iana.EFI_X86_64_HTTP}

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

// String implements fmt.Stringer so Role reads as a name rather than a
// bare int in logs (see server.go's serveUDP).
func (r Role) String() string {
	if r == RolePXE {
		return "pxe"
	}
	return "proxy-dhcp"
}

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
	// OutcomeNotPXE means the request carried neither the PXEClient
	// nor the HTTPClient vendor class option (option 60): not a boot
	// attempt bootd recognizes at all.
	OutcomeNotPXE Outcome = "not-pxe-client"
	// OutcomeUnsupportedArch means option 93 named an architecture
	// this deployment has no loader for. Applies to both the PXE and
	// HTTP Boot paths - both hand out the same x86-64 UEFI shim.
	OutcomeUnsupportedArch Outcome = "unsupported-arch"
	// OutcomeHTTPBootUnconfigured means the request advertised the
	// HTTPClient vendor class but Config.HTTPBootURL is unset: HTTP
	// Boot is opt-in, so bootd declines rather than hand an HTTP
	// client a TFTP filename it cannot use.
	OutcomeHTTPBootUnconfigured Outcome = "http-boot-unconfigured"
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
// Two vendor classes (option 60) are recognized, side by side:
//
//   - PXEClient: the default PXE+TFTP flow. The reply echoes
//     "PXEClient" and hands back cfg.BootFilename (the TFTP shim).
//   - HTTPClient (or "HTTPClient:..." per the UEFI spec's suffixed
//     form): UEFI HTTP Boot. The reply echoes "HTTPClient" back -
//     firmware requires this exact echo to accept the offer - and
//     hands back cfg.HTTPBootURL, a full HTTP(S) URL, as the boot
//     filename instead of a TFTP-relative one. This path only answers
//     when Config.HTTPBootURL is set (see OutcomeHTTPBootUnconfigured);
//     it is opt-in and does not change the PXEClient path at all.
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

	classID := req.ClassIdentifier()
	httpBoot := isHTTPBootClass(classID)
	if !isPXEBootClass(classID) && !httpBoot {
		return nil, nil, OutcomeNotPXE
	}

	if !archSupported(req.ClientArch()) {
		return nil, nil, OutcomeUnsupportedArch
	}

	// HTTP Boot is opt-in (Config.HTTPBootURL unset means disabled): an
	// HTTPClient request with nothing configured to hand back is
	// declined here, before ever consulting the MAC gate, rather than
	// falling through to the PXE reply below with a TFTP filename the
	// requesting firmware did not ask for and cannot use.
	if httpBoot && cfg.HTTPBootURL == "" {
		return nil, nil, OutcomeHTTPBootUnconfigured
	}

	if !gate.Allow(req.ClientHWAddr) {
		return nil, nil, OutcomeUnknownMAC
	}

	respType := dhcpv4.MessageTypeOffer
	if wantType == dhcpv4.MessageTypeRequest {
		respType = dhcpv4.MessageTypeAck
	}

	// respClass and bootFilename branch on which vendor class the
	// client advertised: a PXE client gets pxeVendorClass echoed back
	// and the TFTP shim filename; an HTTP Boot client gets
	// httpClientVendorClass echoed back (firmware requires this exact
	// echo to accept the offer) and the configured HTTP(S) URL in its
	// place.
	respClass := pxeVendorClass
	bootFilename := cfg.BootFilename
	if httpBoot {
		respClass = httpClientVendorClass
		bootFilename = cfg.HTTPBootURL
	}

	modifiers := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(respType),
		// No lease: this is the one invariant every response this
		// package sends must preserve (see the package doc comment) -
		// bootd is not a DHCP server and never assigns an address.
		dhcpv4.WithYourIP(net.IPv4zero),
		dhcpv4.WithServerIP(cfg.NextServerIP),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(cfg.ServerIP)),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(respClass)),
		dhcpv4.WithOption(dhcpv4.OptBootFileName(bootFilename)),
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
	// firmware that only looks at one of them still boots. HTTP Boot
	// firmware reads the same field, now holding the full URL instead
	// of a TFTP-relative filename.
	resp.BootFileName = bootFilename

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
