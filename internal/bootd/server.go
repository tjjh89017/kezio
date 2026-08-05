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

	"github.com/go-logr/logr"
	"github.com/insomniacslk/dhcp/dhcpv4"
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

	pxeConn, err := listenUDP(cfg.PXEAddr)
	if err != nil {
		return fmt.Errorf("listening on PXE boot-server address %s: %w", cfg.PXEAddr, err)
	}
	defer func() { _ = pxeConn.Close() }()

	errCh := make(chan error, 2)
	go serveUDP(ctx, log, dhcpConn, RoleProxyDHCP, cfg, gate, errCh)
	go serveUDP(ctx, log, pxeConn, RolePXE, cfg, gate, errCh)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// listenUDP opens a UDP socket on addr with broadcast sends enabled -
// required for RoleProxyDHCP's non-relayed replies, which are
// broadcast to 255.255.255.255:68 (see destinationFor).
func listenUDP(addr string) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// maxDHCPPacket is a generous upper bound on a DHCPv4 packet's wire
// size (RFC 2131's minimum required support is 576 bytes; PXE clients
// routinely exceed that with vendor options, so this leaves headroom
// well past any real request without accepting an unbounded read).
const maxDHCPPacket = 4096

// serveUDP is one listener's receive loop: read, parse, hand off to
// BuildResponse, send the answer if any. It never exits on a
// per-packet error (a malformed or unparsable packet from one client
// must not take the listener down for every other client on the
// segment) - only a read error on the socket itself, or ctx
// cancellation, ends the loop.
func serveUDP(ctx context.Context, log logr.Logger, conn *net.UDPConn, role Role, cfg Config, gate MACGate, errCh chan<- error) {
	buf := make([]byte, maxDHCPPacket)
	for {
		n, srcAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("reading from %s: %w", conn.LocalAddr(), err)
			return
		}

		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			log.Info("dropping malformed DHCP packet", "remote", srcAddr, "error", err.Error())
			continue
		}

		resp, dst, outcome := BuildResponse(req, role, srcAddr, cfg, gate)
		if outcome != OutcomeAnswered {
			log.V(1).Info("not answering", "remote", srcAddr, "mac", req.ClientHWAddr, "outcome", string(outcome))
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
		log.Info("answering PXE request", "remote", srcAddr, "dst", dst, "mac", req.ClientHWAddr, "role", role)

		if _, err := conn.WriteToUDP(resp.ToBytes(), dst); err != nil {
			log.Info("sending response failed", "remote", srcAddr, "dst", dst, "error", err.Error())
		}
	}
}
