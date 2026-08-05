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
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// TestListenUDPSocketIsBroadcastCapable documents and guards an
// assumption bootd's proxyDHCP send path relies on: Go's net stdlib
// enables SO_BROADCAST by default on every datagram socket it opens
// (see setDefaultSockopts in $GOROOT/src/net/sockopt_linux.go, called
// from the socket() path shared by net.ListenUDP and
// net.ListenConfig.ListenPacket). RoleProxyDHCP's non-relayed replies
// depend on that: destinationFor sends them to 255.255.255.255, which
// the kernel refuses with EACCES on a socket without SO_BROADCAST. This
// test does not guard a historical bug - it pins the assumption so
// that a future change to listenUDP (for example, switching to a raw
// or custom socket construction that does not go through Go's default
// socket setup) gets caught here instead of failing silently in the
// field.
func TestListenUDPSocketIsBroadcastCapable(t *testing.T) {
	conn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	rawConn, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn() error = %v", err)
	}

	var optVal int
	var optErr error
	err = rawConn.Control(func(fd uintptr) {
		optVal, optErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST)
	})
	if err != nil {
		t.Fatalf("rawConn.Control() error = %v", err)
	}
	if optErr != nil {
		t.Fatalf("GetsockoptInt(SO_BROADCAST) error = %v", optErr)
	}
	if optVal == 0 {
		t.Fatalf("SO_BROADCAST not set on listenUDP's socket (got %d, want nonzero); proxyDHCP replies to 255.255.255.255 would fail with EACCES", optVal)
	}
}

// TestServeUDP_PXERoundTrip exercises the server layer itself
// (listenUDP + serveUDP), which BuildResponse's own unit tests do not
// touch: it binds a real loopback socket, sends a wire-encoded
// DHCPREQUEST from a second real socket standing in for a PXE client,
// and asserts the reply that comes back over the network matches what
// BuildResponse would produce. RolePXE is used because its reply is
// unicast to the request's actual source address (see destinationFor),
// so the round trip needs no privileged port and no broadcast.
func TestServeUDP_PXERoundTrip(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go serveUDP(ctx, logr.Discard(), serverConn, RolePXE, testConfig(), knownGate, errCh)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client ListenUDP() error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64, dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	if _, err := clientConn.WriteToUDP(req.ToBytes(), serverConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("sending request: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, maxDHCPPacket)
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}

	resp, err := dhcpv4.FromBytes(buf[:n])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if resp.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want Ack", resp.MessageType())
	}
	if !resp.ServerIPAddr.Equal(testConfig().ServerIP) {
		t.Errorf("ServerIPAddr = %v, want %v", resp.ServerIPAddr, testConfig().ServerIP)
	}
	if resp.BootFileName != DefaultBootFilename {
		t.Errorf("BootFileName = %q, want %q", resp.BootFileName, DefaultBootFilename)
	}

	select {
	case err := <-errCh:
		t.Fatalf("serveUDP reported an error: %v", err)
	default:
	}
}
