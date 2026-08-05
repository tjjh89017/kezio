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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// recordingSink is a minimal logr.LogSink that appends every message
// passed to Info, guarded by a mutex since serveUDP's two listener
// goroutines (RoleProxyDHCP and RolePXE) can log concurrently. Used to
// assert on log content directly rather than on internal call counts,
// since the "answering PXE request" / "served TFTP file" lines are the
// operator-facing signal this package's tests exist to pin (see
// server.go's serveUDP and tftp.go's readHandler).
type recordingSink struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recordingSink) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	copy(out, r.msgs)
	return out
}

func newRecordingLogger(sink *recordingSink) logr.Logger {
	return funcr.NewJSON(func(obj string) {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		sink.msgs = append(sink.msgs, obj)
	}, funcr.Options{})
}

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
	serverPC, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}
	go serveUDP(ctx, logr.Discard(), serverPC, RolePXE, testConfig(), knownGate, newDeniedMACLog(), errCh)

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
	// The request arrived over loopback, so the reply must advertise the
	// loopback interface's own address as siaddr - not testConfig()'s
	// ServerIP, which is only the fallback for an arrival interface that
	// has no IPv4 address (see BuildResponse's ifaceIP parameter).
	loopback := net.IPv4(127, 0, 0, 1)
	if !resp.ServerIPAddr.Equal(loopback) {
		t.Errorf("ServerIPAddr = %v, want the arrival interface's %v", resp.ServerIPAddr, loopback)
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

// TestServeUDP_LogsAnsweredRequest pins the observability fix this test
// file's recordingSink exists for: a request BuildResponse answers must
// produce an "answering PXE request" log line at the default verbosity
// (V(0)), not only at V(1) alongside every declined request - see
// server.go's serveUDP. Without this, bootd's log is silent on both a
// working netboot and a client that never reached it at all, which is
// indistinguishable from the outside.
func TestServeUDP_LogsAnsweredRequest(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	serverPC, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}
	go serveUDP(ctx, newRecordingLogger(sink), serverPC, RolePXE, testConfig(), knownGate, newDeniedMACLog(), errCh)

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
	if _, _, err := clientConn.ReadFromUDP(buf); err != nil {
		t.Fatalf("reading reply: %v", err)
	}

	if !containsSubstring(sink.messages(), "answering PXE request") {
		t.Errorf("log messages = %v, want one containing %q", sink.messages(), "answering PXE request")
	}
}

// TestServeUDP_DoesNotLogNonMACGateDeclineAtDefaultVerbosity is the
// counterpart to TestServeUDP_LogsAnsweredRequest for a decline that is
// not a MAC-gate deny: BuildResponse's OutcomeWrongMessageType branch
// (a DHCPDISCOVER on the RolePXE port, which only answers
// DHCPREQUEST) must stay at V(1) - it must not also start appearing in
// the default-verbosity log, which would drown the one signal that
// matters in noise from every other device sharing the L2 segment.
// TestServeUDP_LogsMACGateDenialOncePerMAC below covers the one decline
// (OutcomeUnknownMAC) that is deliberately promoted to Info.
func TestServeUDP_DoesNotLogNonMACGateDeclineAtDefaultVerbosity(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	serverPC, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}
	go serveUDP(ctx, newRecordingLogger(sink), serverPC, RolePXE, testConfig(), knownGate, newDeniedMACLog(), errCh)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client ListenUDP() error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// A DHCPDISCOVER (not DHCPREQUEST) on the RolePXE port: an ordinary,
	// expected decline (OutcomeWrongMessageType) that must not raise the
	// default log's noise floor, unlike the MAC-gate deny below.
	req := pxeDiscover(t, knownMAC, iana.EFI_X86_64)
	if _, err := clientConn.WriteToUDP(req.ToBytes(), serverConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("sending request: %v", err)
	}

	// No reply is expected for a declined request; give serveUDP time to
	// process and log it before asserting on sink contents.
	time.Sleep(200 * time.Millisecond)

	if len(sink.messages()) != 0 {
		t.Errorf("log messages = %v, want none at default verbosity for a non-MAC-gate decline", sink.messages())
	}

	select {
	case err := <-errCh:
		t.Fatalf("serveUDP reported an error: %v", err)
	default:
	}
}

// TestServeUDP_LogsMACGateDenialOncePerMAC pins the fix for the
// invisible-deny failure mode: a request declined for OutcomeUnknownMAC
// (BuildResponse's MAC gate) must log at the default verbosity with the
// denied MAC, since it is the one decline outcome that most plausibly
// means a kezio Machine is failing to net-boot rather than an unrelated
// device on the segment being ignored as expected. A repeat deny for
// the same MAC (a PXE retry loop resending the same DHCPDISCOVER) must
// not log again at the default verbosity - see deniedMACLog.
func TestServeUDP_LogsMACGateDenialOncePerMAC(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	serverPC, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}
	go serveUDP(ctx, newRecordingLogger(sink), serverPC, RolePXE, testConfig(), knownGate, newDeniedMACLog(), errCh)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client ListenUDP() error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	req := pxeDiscover(t, unknownMAC, iana.EFI_X86_64, dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	send := func() {
		t.Helper()
		if _, err := clientConn.WriteToUDP(req.ToBytes(), serverConn.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatalf("sending request: %v", err)
		}
	}

	// Send the same denied MAC's request twice, simulating a firmware
	// PXE retry loop; give serveUDP time to process and log each one
	// before asserting on sink contents.
	send()
	time.Sleep(200 * time.Millisecond)
	send()
	time.Sleep(200 * time.Millisecond)

	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("log messages = %v, want exactly 1 default-verbosity line for two denies of the same MAC", msgs)
	}
	if !strings.Contains(msgs[0], "denying PXE request") || !strings.Contains(msgs[0], unknownMAC.String()) {
		t.Errorf("log message = %q, want it to name the denied MAC %q", msgs[0], unknownMAC.String())
	}

	select {
	case err := <-errCh:
		t.Fatalf("serveUDP reported an error: %v", err)
	default:
	}
}

// TestReplyControlMessage_PinsToArrivalInterface pins the core of the
// dual-homed reply fix: when the received packet named an arrival
// interface, the reply's control message must carry that same
// interface index, so the reply is sent out the interface the request
// actually came in on rather than whatever interface the kernel's
// default route would otherwise pick.
func TestReplyControlMessage_PinsToArrivalInterface(t *testing.T) {
	const arrivalIfIndex = 7
	got := replyControlMessage(&ipv4.ControlMessage{IfIndex: arrivalIfIndex}, nil)
	if got == nil {
		t.Fatal("replyControlMessage() = nil, want a non-nil ControlMessage")
	}
	if got.IfIndex != arrivalIfIndex {
		t.Errorf("IfIndex = %d, want %d", got.IfIndex, arrivalIfIndex)
	}
}

// TestReplyControlMessage_PinsSourceAddress pins the source half of the
// dual-homed reply fix: pinning the egress interface alone does not pin
// the reply's source address (the kernel can still select an address
// from another interface), so when the arrival interface's own address
// is known it must ride along in the control message as the reply's
// source.
func TestReplyControlMessage_PinsSourceAddress(t *testing.T) {
	src := net.IPv4(192, 0, 2, 10)
	got := replyControlMessage(&ipv4.ControlMessage{IfIndex: 7}, src)
	if got == nil {
		t.Fatal("replyControlMessage() = nil, want a non-nil ControlMessage")
	}
	if !got.Src.Equal(src) {
		t.Errorf("Src = %v, want %v", got.Src, src)
	}
}

// TestReplyControlMessage_NilArrivalFallsBackToKernelChoice covers the
// platform-gap case: ReadFrom reporting no control message at all (nil
// cm) must not make WriteTo fail or panic - it must fall back to a
// zero-value ControlMessage, the same "let the kernel pick" behavior
// this package had before interface pinning existed.
func TestReplyControlMessage_NilArrivalFallsBackToKernelChoice(t *testing.T) {
	got := replyControlMessage(nil, net.IPv4(192, 0, 2, 10))
	if got == nil {
		t.Fatal("replyControlMessage(nil) = nil, want a non-nil zero-value ControlMessage")
	}
	if got.IfIndex != 0 {
		t.Errorf("IfIndex = %d, want 0 (kernel default)", got.IfIndex)
	}
}

// TestNewInterfaceAwarePacketConn_EnablesInterfaceControlMessages
// guards the setup half of the fix: the wrapped connection must
// actually report an arrival interface on receive, or
// replyControlMessage above would have nothing to pin the reply to.
// This exercises a real loopback socket rather than asserting on
// internal state, since the behavior under test is a kernel-reported
// property of the socket, not a Go-level field.
func TestNewInterfaceAwarePacketConn_EnablesInterfaceControlMessages(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	pc, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client ListenUDP() error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := clientConn.WriteToUDP([]byte("probe"), serverConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("sending probe packet: %v", err)
	}

	if err := serverConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	_, cm, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if cm == nil {
		t.Fatal("ReadFrom() control message = nil, want the arrival interface reported")
	}
	if cm.IfIndex == 0 {
		t.Errorf("IfIndex = 0, want the loopback interface's nonzero index")
	}
}

// TestListenUDPSocketDisablesTxChecksum guards the SO_NO_CHECK half of
// the reply path (see disableUDPTxChecksum): every reply socket must
// send its datagrams with a zero UDP checksum, so a checksum-offloaded
// partial value can never reach a booting firmware un-filled and make
// it discard the OFFER. Asserted via getsockopt on the live socket -
// the actual on-wire zero checksum is a kernel behavior a unit test
// cannot capture, but the option being set is the entire code-level
// contract.
func TestListenUDPSocketDisablesTxChecksum(t *testing.T) {
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
		optVal, optErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NO_CHECK)
	})
	if err != nil {
		t.Fatalf("rawConn.Control() error = %v", err)
	}
	if optErr != nil {
		t.Fatalf("GetsockoptInt(SO_NO_CHECK) error = %v", optErr)
	}
	if optVal == 0 {
		t.Fatalf("SO_NO_CHECK not set on listenUDP's socket (got %d, want nonzero); offloaded partial checksums could reach PXE firmware un-filled", optVal)
	}
}

// TestArrivalInterfaceIP_ResolvesLoopback exercises arrivalInterfaceIP
// against a real kernel-reported control message: a packet received
// over loopback must resolve to the loopback interface's own IPv4
// address, the address every reply-content and source-selection
// decision in serveUDP keys off.
func TestArrivalInterfaceIP_ResolvesLoopback(t *testing.T) {
	serverConn, err := listenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenUDP() error = %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	pc, err := newInterfaceAwarePacketConn(serverConn)
	if err != nil {
		t.Fatalf("newInterfaceAwarePacketConn() error = %v", err)
	}

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client ListenUDP() error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := clientConn.WriteToUDP([]byte("probe"), serverConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("sending probe packet: %v", err)
	}

	if err := serverConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	_, cm, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}

	got := arrivalInterfaceIP(cm)
	if !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("arrivalInterfaceIP() = %v, want 127.0.0.1 (the loopback interface's own address)", got)
	}
}

// TestArrivalInterfaceIP_NilOrUnresolvableYieldsNil covers every "no
// address to resolve" input in one place: a nil control message, a
// zero interface index, and an index no interface holds must all yield
// nil - the callers' signal to fall back to Config.ServerIP and to
// kernel source selection.
func TestArrivalInterfaceIP_NilOrUnresolvableYieldsNil(t *testing.T) {
	cases := map[string]*ipv4.ControlMessage{
		"nil control message":  nil,
		"zero interface index": {IfIndex: 0},
		"unknown interface":    {IfIndex: 1 << 30},
	}
	for name, cm := range cases {
		if got := arrivalInterfaceIP(cm); got != nil {
			t.Errorf("%s: arrivalInterfaceIP() = %v, want nil", name, got)
		}
	}
}

// containsSubstring reports whether any element of msgs contains want.
func containsSubstring(msgs []string, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}
