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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDnsmasqLabClient is the packet lab's client-side entry point: a
// PXE-shaped DHCPDISCOVER sender that runs in the lab's client network
// namespace, on the far end of the veth pair from TestDnsmasqLab's
// dnsmasq. It sends one DHCPDISCOVER carrying the PXE vendor class and
// architecture options a real UEFI NIC ROM sends, then asserts on
// whether - and what - dnsmasq answers with. This is what turns the
// packet lab's dnsmasq-side assertions ("the config renders correctly",
// "the supervisor stays up") into an end-to-end assertion on the wire:
// scenario 1 (no relay) and scenario 3 (lease mode) both hinge on what
// a client actually receives, not just on what bootd renders.
//
// Skipped unless BOOTD_LAB_CLIENT=1, for the same reason TestDnsmasqLab
// skips unless BOOTD_LAB=1: this needs a real interface in a real
// network namespace and is not a CI test.
//
// Environment:
//
//   - BOOTD_LAB_CLIENT=1: enables the test.
//   - BOOTD_LAB_CLIENT_IFACE: the client-side interface to bind to
//     (SO_BINDTODEVICE), required.
//   - BOOTD_LAB_CLIENT_MAC: the chaddr this DHCPDISCOVER carries,
//     required.
//   - BOOTD_LAB_CLIENT_TIMEOUT: how long to wait for a reply before
//     deciding none is coming. Empty means 3s.
//   - BOOTD_LAB_CLIENT_EXPECT: "offer" or "none", required. "offer"
//     asserts a DHCPOFFER carrying this client's own transaction ID
//     arrives before the timeout; "none" asserts nothing does - the
//     shape a MAC-gated dhcp-ignore drop produces.
//   - BOOTD_LAB_CLIENT_EXPECT_SIADDR: required when EXPECT=offer, the
//     next-server address (bootd's ServerIP or NextServerIP) the offer
//     must carry in its siaddr field.
//   - BOOTD_LAB_CLIENT_EXPECT_FILE: optional even when EXPECT=offer -
//     see the check's own comment for why proxyDHCP's first offer
//     never carries it. Set for a lease-mode assertion, which does
//     carry the boot filename directly.
//   - BOOTD_LAB_CLIENT_EXPECT_LEASE=1: required when EXPECT=offer and
//     the lab server is running in lease mode - asserts yiaddr is a
//     real, non-zero lease address rather than proxyDHCP's 0.0.0.0.
//     Omitted (the default) asserts yiaddr is 0.0.0.0, proxyDHCP's
//     shape.
func TestDnsmasqLabClient(t *testing.T) {
	if os.Getenv("BOOTD_LAB_CLIENT") != "1" {
		t.Skip("BOOTD_LAB_CLIENT not set; this is the containerized packet lab's client entry point, not a CI test")
	}

	iface := os.Getenv("BOOTD_LAB_CLIENT_IFACE")
	if iface == "" {
		t.Fatal("BOOTD_LAB_CLIENT_IFACE is required")
	}
	macStr := os.Getenv("BOOTD_LAB_CLIENT_MAC")
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		t.Fatalf("BOOTD_LAB_CLIENT_MAC %q: %v", macStr, err)
	}
	timeout := 3 * time.Second
	if s := os.Getenv("BOOTD_LAB_CLIENT_TIMEOUT"); s != "" {
		timeout, err = time.ParseDuration(s)
		if err != nil {
			t.Fatalf("BOOTD_LAB_CLIENT_TIMEOUT %q: %v", s, err)
		}
	}
	expect := os.Getenv("BOOTD_LAB_CLIENT_EXPECT")
	if expect != "offer" && expect != "none" {
		t.Fatalf("BOOTD_LAB_CLIENT_EXPECT must be \"offer\" or \"none\", got %q", expect)
	}

	conn, err := dialBroadcastDHCP(iface)
	if err != nil {
		t.Fatalf("opening broadcast DHCP socket on %s: %v", iface, err)
	}
	defer func() { _ = conn.Close() }()

	xid := rand.Uint32()
	discover := buildDHCPDiscover(xid, mac)
	if _, err := conn.WriteToUDP(discover, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatalf("sending DHCPDISCOVER: %v", err)
	}

	offer, err := readMatchingOffer(conn, xid, timeout)
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}

	if expect == "none" {
		if offer != nil {
			t.Fatalf("expected no reply for MAC %s, got a DHCP message type %d with yiaddr %s siaddr %s file %q",
				macStr, offer.msgType, offer.yiaddr, offer.siaddr, offer.file)
		}
		return
	}

	// expect == "offer"
	if offer == nil {
		t.Fatalf("expected a DHCPOFFER for MAC %s within %s, got none", macStr, timeout)
	}
	if offer.msgType != dhcpMsgTypeOffer {
		t.Fatalf("expected DHCPOFFER (message type %d), got message type %d", dhcpMsgTypeOffer, offer.msgType)
	}
	wantSiaddr := os.Getenv("BOOTD_LAB_CLIENT_EXPECT_SIADDR")
	if wantSiaddr == "" {
		t.Fatal("BOOTD_LAB_CLIENT_EXPECT_SIADDR is required when EXPECT=offer")
	}
	if !offer.siaddr.Equal(net.ParseIP(wantSiaddr)) {
		t.Fatalf("siaddr mismatch: want %s, got %s", wantSiaddr, offer.siaddr)
	}
	// BOOTD_LAB_CLIENT_EXPECT_FILE is optional: proxyDHCP's first
	// DHCPOFFER (scenario 1) never carries the boot filename - a real
	// PXE ROM only receives it from a second exchange on port 4011
	// (pxe-service), which this lab client does not replicate. Lease
	// mode's DHCPOFFER (scenario 3) is a plain, non-proxy offer and
	// does carry it directly, so lease-mode scenarios set this.
	if wantFile := os.Getenv("BOOTD_LAB_CLIENT_EXPECT_FILE"); wantFile != "" {
		if offer.file != wantFile {
			t.Fatalf("boot filename mismatch: want %q, got %q", wantFile, offer.file)
		}
	}
	wantLease := os.Getenv("BOOTD_LAB_CLIENT_EXPECT_LEASE") == "1"
	gotLease := !offer.yiaddr.Equal(net.IPv4zero)
	if wantLease != gotLease {
		t.Fatalf("lease-address expectation mismatch: want non-zero yiaddr=%v, got yiaddr %s", wantLease, offer.yiaddr)
	}
	t.Logf("received DHCPOFFER: yiaddr=%s siaddr=%s file=%q", offer.yiaddr, offer.siaddr, offer.file)
}

// dialBroadcastDHCP opens a UDP socket bound to port 68 (the DHCP
// client port - dnsmasq replies there) on iface alone, with
// SO_BROADCAST set so sending to 255.255.255.255 is permitted. A plain
// net.ListenUDP does not set SO_BROADCAST, which the kernel otherwise
// refuses a broadcast sendto with EACCES.
func dialBroadcastDHCP(iface string) (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
				if sockErr != nil {
					return
				}
				sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":68")
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

// dhcpMsgTypeDiscover and dhcpMsgTypeOffer are DHCP message type option
// (53) values (RFC 2131).
const (
	dhcpMsgTypeDiscover = 1
	dhcpMsgTypeOffer    = 2
)

// dhcpOffer is the subset of a received DHCP reply this lab client
// checks.
type dhcpOffer struct {
	msgType byte
	yiaddr  net.IP
	siaddr  net.IP
	file    string
}

// buildDHCPDiscover renders a minimal BOOTP/DHCP DISCOVER packet
// carrying the PXE options a UEFI x86-64 NIC ROM sends: vendor class
// "PXEClient" (option 60) and client system architecture 7 - EFI
// x86-64, RFC 4578 - (option 93). These two are what dnsmasq's
// pxe-service/dhcp-match machinery keys off of; internal/bootd/render.go
// documents both. Padded to BOOTP's traditional 300-byte minimum, the
// same size real PXE firmware sends, since some DHCP servers silently
// ignore anything shorter.
func buildDHCPDiscover(xid uint32, mac net.HardwareAddr) []byte {
	buf := make([]byte, 236)
	buf[0] = 1 // op: BOOTREQUEST
	buf[1] = 1 // htype: Ethernet
	buf[2] = 6 // hlen
	buf[3] = 0 // hops
	binary.BigEndian.PutUint32(buf[4:8], xid)
	binary.BigEndian.PutUint16(buf[8:10], 0)       // secs
	binary.BigEndian.PutUint16(buf[10:12], 0x8000) // flags: broadcast
	// ciaddr, yiaddr, siaddr, giaddr (12:28) stay zero.
	copy(buf[28:28+len(mac)], mac) // chaddr

	var b bytes.Buffer
	b.Write(buf)
	b.Write([]byte{99, 130, 83, 99}) // magic cookie

	writeOpt := func(code byte, data []byte) {
		b.WriteByte(code)
		b.WriteByte(byte(len(data)))
		b.Write(data)
	}
	writeOpt(53, []byte{dhcpMsgTypeDiscover})
	writeOpt(55, []byte{1, 3, 6, 15, 66, 67}) // parameter request list
	writeOpt(60, []byte("PXEClient"))
	writeOpt(93, []byte{0x00, 0x07}) // client system architecture: EFI x86-64
	b.WriteByte(255)                 // end

	pkt := b.Bytes()
	if len(pkt) < 300 {
		pkt = append(pkt, make([]byte, 300-len(pkt))...)
	}
	return pkt
}

// readMatchingOffer reads packets off conn until one whose xid matches
// want arrives, or timeout elapses with none seen. A nil, nil return
// means the timeout elapsed with nothing matching - the expected
// outcome for a MAC-gated dhcp-ignore drop.
func readMatchingOffer(conn *net.UDPConn, want uint32, timeout time.Duration) (*dhcpOffer, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1500)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return nil, fmt.Errorf("setting read deadline: %w", err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if os.IsTimeout(err) {
				return nil, nil
			}
			return nil, err
		}
		offer, xid, ok := parseDHCPReply(buf[:n])
		if !ok || xid != want {
			continue // not a DHCP reply, or a stray reply to a different transaction
		}
		return offer, nil
	}
}

// parseDHCPReply parses a BOOTP/DHCP reply packet's fixed header and
// options, reporting the fields TestDnsmasqLabClient checks plus the
// packet's own xid (so the caller can filter out replies to other
// transactions sharing the broadcast domain). ok is false for anything
// too short to be a DHCP packet or missing the magic cookie.
func parseDHCPReply(pkt []byte) (offer *dhcpOffer, xid uint32, ok bool) {
	if len(pkt) < 240 {
		return nil, 0, false
	}
	if !bytes.Equal(pkt[236:240], []byte{99, 130, 83, 99}) {
		return nil, 0, false
	}
	xid = binary.BigEndian.Uint32(pkt[4:8])
	yiaddr := net.IP(pkt[16:20])
	siaddr := net.IP(pkt[20:24])
	file := nullTerminated(pkt[108:236])

	var msgType byte
	opts := pkt[240:]
	for len(opts) > 0 {
		code := opts[0]
		if code == 255 {
			break
		}
		if code == 0 {
			opts = opts[1:]
			continue
		}
		if len(opts) < 2 {
			break
		}
		l := int(opts[1])
		if len(opts) < 2+l {
			break
		}
		data := opts[2 : 2+l]
		if code == 53 && l == 1 {
			msgType = data[0]
		}
		opts = opts[2+l:]
	}

	return &dhcpOffer{
		msgType: msgType,
		yiaddr:  yiaddr,
		siaddr:  siaddr,
		file:    file,
	}, xid, true
}

// nullTerminated returns b up to its first zero byte, as a string -
// the encoding the BOOTP sname/file header fields use.
func nullTerminated(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
