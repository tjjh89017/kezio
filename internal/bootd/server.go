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
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/go-logr/logr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Server runs the two proxyDHCP/PXE UDP listeners described in the
// package doc comment. It is the thin socket loop around BuildResponse
// (dhcp.go): all it does per packet is parse, call BuildResponse, and -
// if answered - send the result. Every actual decision lives in
// BuildResponse, so it stays testable without a socket.
type Server struct {
	// Config holds the listen addresses, boot filename, and MAC-gating
	// mode.
	Config Config
	// Gate decides whether a client's MAC is answered; see MACGate. A
	// nil Gate with Config.AnswerAll unset answers nothing (fail
	// secure) rather than panicking - see resolveGate.
	Gate MACGate
}

var _ manager.Runnable = (*Server)(nil)

// resolveGate returns the MACGate BuildResponse should consult:
// Config.AnswerAll's alwaysAllow when set (BuildResponse applies that
// override itself), s.Gate otherwise, or - critically - a gate that
// denies everything when s.Gate is nil and AnswerAll is not set. A nil
// Gate is not a caller convenience worth accepting silently: it would
// otherwise nil-pointer panic deep inside BuildResponse's gate.Allow
// call for the very first real request, and "crash on the first
// packet" is a worse fail-secure story than "log once at startup and
// deny everything".
func (s *Server) resolveGate() MACGate {
	if s.Config.AnswerAll {
		return alwaysAllow{}
	}
	if s.Gate == nil {
		return denyAll{}
	}
	return s.Gate
}

// denyAll is the fail-secure MACGate used when Server is misconfigured
// with neither a real Gate nor AnswerAll.
type denyAll struct{}

func (denyAll) Allow(net.HardwareAddr) bool { return false }

// Start implements manager.Runnable: it runs both listeners until ctx
// is cancelled.
func (s *Server) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd")
	cfg := s.Config.withDefaults()
	gate := s.resolveGate()

	dhcpConn, err := listenUDP(cfg.DHCPAddr)
	if err != nil {
		return fmt.Errorf("listening on proxyDHCP address %s: %w", cfg.DHCPAddr, err)
	}
	defer func() { _ = dhcpConn.Close() }()
	dhcpPC, err := newInterfaceAwarePacketConn(dhcpConn)
	if err != nil {
		return fmt.Errorf("enabling interface control messages on %s: %w", dhcpConn.LocalAddr(), err)
	}

	pxeConn, err := listenUDP(cfg.PXEAddr)
	if err != nil {
		return fmt.Errorf("listening on PXE boot-server address %s: %w", cfg.PXEAddr, err)
	}
	defer func() { _ = pxeConn.Close() }()
	pxePC, err := newInterfaceAwarePacketConn(pxeConn)
	if err != nil {
		return fmt.Errorf("enabling interface control messages on %s: %w", pxeConn.LocalAddr(), err)
	}

	// One deniedMACLog shared by both listeners: a firmware's PXE retry
	// loop sends the same DHCPDISCOVER (port 67) and, once it holds a
	// lease, the same DHCPREQUEST (port 4011) every few seconds, and a
	// MAC gate deny is the same story on either port - "log the first
	// one at default verbosity, then quiet down" should apply once per
	// MAC across the whole server, not once per port.
	denied := newDeniedMACLog()

	errCh := make(chan error, 2)
	go serveUDP(ctx, log, dhcpPC, RoleProxyDHCP, cfg, gate, denied, errCh)
	go serveUDP(ctx, log, pxePC, RolePXE, cfg, gate, denied, errCh)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// listenUDP opens a UDP socket on addr with broadcast sends enabled -
// required for RoleProxyDHCP's non-relayed replies, which are
// broadcast to 255.255.255.255:68 (see destinationFor) - and with UDP
// transmit checksums disabled (see disableUDPTxChecksum for why an
// absent checksum is safer here than a computed one).
func listenUDP(addr string) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}
	if err := disableUDPTxChecksum(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("disabling UDP transmit checksums: %w", err)
	}
	return conn, nil
}

// disableUDPTxChecksum sets SO_NO_CHECK on conn, so every reply this
// socket sends carries a zero UDP checksum instead of a computed one.
// RFC 768 makes the checksum optional for UDP over IPv4 - zero means
// "not computed" and every conforming receiver (PXE firmware included;
// EDK2's UDP driver only validates a nonzero checksum field) accepts
// the datagram without verification.
//
// A zero checksum is deliberately preferred over a computed one because
// of how these replies actually travel in a virtualized boot network:
// the kernel defers checksumming to the egress device (checksum
// offload), leaving only a partial pseudo-header value in the field,
// and a path that crosses a veth pair onto a bridge and into a
// hypervisor tap device can traverse every hop without any of them
// performing the deferred fill. The frame then reaches the booting
// firmware with a checksum field that fails verification, and the
// firmware silently discards the one OFFER it was waiting for. An
// absent checksum cannot be mangled in transit this way. dnsmasq
// applies the same option to its DHCP sockets for the same reason.
func disableUDPTxChecksum(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var optErr error
	if err := rawConn.Control(func(fd uintptr) {
		optErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NO_CHECK, 1)
	}); err != nil {
		return err
	}
	return optErr
}

// maxDHCPPacket is a generous upper bound on a DHCPv4 packet's wire
// size (RFC 2131's minimum required support is 576 bytes; PXE clients
// routinely exceed that with vendor options, so this leaves headroom
// well past any real request without accepting an unbounded read).
const maxDHCPPacket = 4096

// deniedMACLog rate-limits MAC-gate deny logging to the default
// verbosity (Info), rather than leaving every deny at V(1) where the
// default zap level (see cmd/bootd/main.go) filters it out entirely.
// The MAC gate is the one deny path most likely to hide a real
// misconfiguration - a Machine that never got net-booted because bootd
// silently refused its MAC - and unlike a malformed packet or an
// unsupported architecture (still logged at V(1) only; those are noise
// from other devices on the segment, not a kezio Machine failing to
// boot), a repeated deny for the same MAC across a PXE retry loop
// (firmware resends its DHCPDISCOVER/DHCPREQUEST every few seconds
// until it gives up) must not flood the default log. seen reports
// true, and stays true, only after the first deny for a given MAC is
// logged.
type deniedMACLog struct {
	mu     sync.Mutex
	logged map[string]struct{}
}

// newDeniedMACLog returns an empty deniedMACLog, ready to use.
func newDeniedMACLog() *deniedMACLog {
	return &deniedMACLog{logged: make(map[string]struct{})}
}

// seen reports whether mac has already been logged once by this
// deniedMACLog, and marks it logged if this is the first time - so the
// caller can tell "log at Info" (first sighting) from "log at V(1)
// only" (repeat) with a single call.
func (d *deniedMACLog) seen(mac string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.logged[mac]; ok {
		return true
	}
	d.logged[mac] = struct{}{}
	return false
}

// serveUDP is one listener's receive loop: read, parse, hand off to
// BuildResponse, send the answer if any. It never exits on a
// per-packet error (a malformed or unparsable packet from one client
// must not take the listener down for every other client on the
// segment) - only a read error on the socket itself, or ctx
// cancellation, ends the loop.
//
// pc must come from newInterfaceAwarePacketConn - an ipv4.PacketConn
// with per-packet interface control messages enabled - so every reply
// can be pinned to send out the same network interface the request
// arrived on. This matters whenever the process has more than one
// network interface (for example, a pod with both a cluster
// default-route interface and a separate provisioning-network
// interface): a broadcast or unrelayed reply sent from a socket bound
// to 0.0.0.0 would otherwise be routed by the kernel's default route,
// which has no reason to point at the interface the request actually
// came in on. Pinning egress to the arrival interface makes the reply
// path correct regardless of which interface holds the default route.
// The wrap happens in the caller, before this loop starts, so no
// packet can ever be received ahead of the control messages being
// enabled - one read without them and that reply falls back to the
// default route's interface and address.
func serveUDP(ctx context.Context, log logr.Logger, pc *ipv4.PacketConn, role Role, cfg Config, gate MACGate, denied *deniedMACLog, errCh chan<- error) {
	buf := make([]byte, maxDHCPPacket)
	for {
		n, cm, srcAddr, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("reading from %s: %w", pc.LocalAddr(), err)
			return
		}
		srcUDPAddr, ok := srcAddr.(*net.UDPAddr)
		if !ok {
			// net.UDPConn.ReadFrom always returns a *net.UDPAddr; this
			// only guards against a future change to the underlying
			// connection type rather than a case observed in practice.
			log.Info("dropping packet from non-UDP source", "remote", srcAddr)
			continue
		}

		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			log.Info("dropping malformed DHCP packet", "remote", srcUDPAddr, "error", err.Error())
			continue
		}

		// The arrival interface's own IPv4 address (nil when it has
		// none) feeds both the response's contents - Server Identifier
		// and next-server, see BuildResponse - and the reply's source
		// address selection below: the client can only reach bootd on
		// the segment this request came in on, so every address in the
		// reply must name that segment, never whichever interface holds
		// the process's default route.
		ifaceIP := arrivalInterfaceIP(cm)

		resp, dst, outcome := BuildResponse(req, role, srcUDPAddr, ifaceIP, cfg, gate)
		if outcome != OutcomeAnswered {
			// A MAC-gate deny (OutcomeUnknownMAC) is logged once per MAC
			// at the default verbosity - see deniedMACLog's doc comment.
			// Every other decline (not a PXE client, unsupported
			// architecture, HTTP Boot requested but unconfigured, wrong
			// message type for this port) stays at V(1): those are
			// expected noise from devices on the segment that were never
			// going to be answered, not a kezio Machine failing to boot.
			if outcome == OutcomeUnknownMAC && !denied.seen(req.ClientHWAddr.String()) {
				log.Info("denying PXE request: MAC not enrolled", "remote", srcUDPAddr, "mac", req.ClientHWAddr, "role", role)
			} else {
				log.V(1).Info("not answering", "remote", srcUDPAddr, "mac", req.ClientHWAddr, "outcome", string(outcome))
			}
			continue
		}

		// Logged at the default (non-V(1)) level, unlike the "not
		// answering" branch above: an answered request is the one event
		// on this path an operator actually needs to see without raising
		// verbosity, since it is both low-volume (one per booting
		// machine, not one per stray broadcast on the segment) and the
		// single strongest signal that a PXE boot is progressing -
		// its absence from the log is what first flagged a netboot that
		// never reached bootd at all.
		log.Info("answering PXE request", "remote", srcUDPAddr, "dst", dst, "mac", req.ClientHWAddr, "role", role)

		// replyControlMessage pins the reply's egress interface to the
		// interface the request arrived on and its source address to
		// that interface's own IPv4 address (see this function's doc
		// comment); a nil cm (the platform did not report one) falls
		// back to letting the kernel choose, the same behavior this
		// package had before interface pinning existed.
		if _, err := pc.WriteTo(resp.ToBytes(), replyControlMessage(cm, ifaceIP), dst); err != nil {
			log.Info("sending response failed", "remote", srcUDPAddr, "dst", dst, "error", err.Error())
		}
	}
}

// newInterfaceAwarePacketConn wraps conn in an ipv4.PacketConn with
// per-packet arrival-interface reporting turned on, so serveUDP can
// read back which interface each request came in on (via the
// *ipv4.ControlMessage ReadFrom returns) and pin the reply to leave by
// that same interface (see replyControlMessage). Enabling this control
// message is a plain socket option - it needs no elevated capability
// beyond what binding the listening port itself already requires.
func newInterfaceAwarePacketConn(conn *net.UDPConn) (*ipv4.PacketConn, error) {
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return nil, err
	}
	return pc, nil
}

// replyControlMessage builds the control message serveUDP's WriteTo
// call uses to select the reply's egress interface and source address.
// cm is whatever ReadFrom reported for the received request; when it
// named an interface, the reply is pinned to leave by that same one, so
// a broadcast or unrelayed reply from a multi-interface process reaches
// the network the request actually came from rather than following
// whichever interface holds the process's default route.
//
// src is the arrival interface's own IPv4 address (see
// arrivalInterfaceIP); when known, it is set as the reply's source
// address too. Pinning the egress interface alone does not pin the
// source: the kernel's source selection can still fall back to another
// interface's address when the egress interface offers none it
// prefers, and a booting firmware then sees an OFFER from an address
// that is not on its own segment. A nil src leaves source selection to
// the kernel, and a nil cm (no interface reported) yields a zero-value
// ControlMessage - the kernel picks both, the same behavior this
// package had before interface pinning existed.
func replyControlMessage(cm *ipv4.ControlMessage, src net.IP) *ipv4.ControlMessage {
	if cm == nil {
		return &ipv4.ControlMessage{}
	}
	return &ipv4.ControlMessage{IfIndex: cm.IfIndex, Src: src}
}

// arrivalInterfaceIP resolves the IPv4 address of the interface cm
// reports a request arrived on, or nil when there is none to resolve:
// no control message, no interface index in it, the interface gone by
// lookup time, or an interface with no IPv4 address at all (an L2-only
// attachment - possible, and the reply then falls back to
// Config.ServerIP for its contents and to kernel source selection for
// its source address). The first IPv4 address wins when the interface
// holds several; a boot-network attachment carrying exactly one
// address is the configuration this package documents (see
// config/bootd's NetworkAttachmentDefinition example).
func arrivalInterfaceIP(cm *ipv4.ControlMessage) net.IP {
	if cm == nil || cm.IfIndex == 0 {
		return nil
	}
	iface, err := net.InterfaceByIndex(cm.IfIndex)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4
		}
	}
	return nil
}
