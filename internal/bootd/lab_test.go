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
	"bufio"
	"context"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// TestDnsmasqLab runs the real Dnsmasq supervisor against a real
// dnsmasq binary, driven by an external harness over a control FIFO.
// It is not a CI test: it is the in-repo entry point the containerized
// packet lab (a netns topology with a PXE-shaped test client) uses to
// exercise the exact code paths production runs - config rendering,
// supervision, hostsfile rewrite + SIGHUP - with real packets on real
// sockets, holding only the capabilities the pod grants (uid 0 with
// the three-capability bounding set, per config/bootd's deployment).
// Skipped unless BOOTD_LAB=1.
//
// Environment:
//
//   - BOOTD_LAB=1: enables the test.
//   - BOOTD_LAB_IFACE, BOOTD_LAB_SERVER_IP, BOOTD_LAB_CIDR: the
//     provisioning interface/address/subnet inside the lab namespace.
//   - BOOTD_LAB_RELAY: optional relay target (BOOTD_DHCP_RELAY_SERVER
//     equivalent).
//   - BOOTD_LAB_LEASE_MODE=1: enables Config.LeaseMode (mutually
//     exclusive with BOOTD_LAB_RELAY, same as production).
//   - BOOTD_LAB_LEASE_RANGE_START, BOOTD_LAB_LEASE_RANGE_END: optional
//     explicit lease range bounds; both unset auto-derives from
//     BOOTD_LAB_CIDR.
//   - BOOTD_LAB_RUN_DIR: writable run directory.
//   - BOOTD_DNSMASQ_PATH: dnsmasq binary path.
//   - BOOTD_LAB_CTRL: path of a FIFO carrying one command per line:
//     "allow <mac>[,<mac>...]" pushes an allowlist (as the Machine
//     informer would), "deny" pushes an empty one, "quit" ends the
//     test.
//   - BOOTD_LAB_CLEAR_AMBIENT=1: clear the process's ambient
//     capability set first, emulating a container runtime that grants
//     capabilities to pid 1 without ambient inheritance - the raise in
//     raiseAmbientCaps must then be what lets the dnsmasq child bind
//     its sockets.
func TestDnsmasqLab(t *testing.T) {
	if os.Getenv("BOOTD_LAB") != "1" {
		t.Skip("BOOTD_LAB not set; this is the containerized packet lab's entry point, not a CI test")
	}

	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	log := logf.Log.WithName("lab")

	if os.Getenv("BOOTD_LAB_CLEAR_AMBIENT") == "1" {
		if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
			t.Fatalf("clearing ambient capabilities: %v", err)
		}
		log.Info("cleared ambient capability set; dnsmasq must rely on raiseAmbientCaps")
	}

	serverIP := net.ParseIP(os.Getenv("BOOTD_LAB_SERVER_IP"))
	if serverIP == nil {
		t.Fatal("BOOTD_LAB_SERVER_IP is required")
	}
	_, cidr, err := net.ParseCIDR(os.Getenv("BOOTD_LAB_CIDR"))
	if err != nil {
		t.Fatalf("BOOTD_LAB_CIDR: %v", err)
	}
	cfg := Config{
		Interface:       os.Getenv("BOOTD_LAB_IFACE"),
		ServerIP:        serverIP,
		ProvisioningNet: cidr,
		TFTPDir:         t.TempDir(),
	}
	if s := os.Getenv("BOOTD_LAB_RELAY"); s != "" {
		cfg.RelayServerIP = net.ParseIP(s)
		if cfg.RelayServerIP == nil {
			t.Fatalf("BOOTD_LAB_RELAY %q is not an IP address", s)
		}
	}
	cfg.LeaseMode = os.Getenv("BOOTD_LAB_LEASE_MODE") == "1"
	if s := os.Getenv("BOOTD_LAB_LEASE_RANGE_START"); s != "" {
		cfg.LeaseRangeStart = net.ParseIP(s)
		if cfg.LeaseRangeStart == nil {
			t.Fatalf("BOOTD_LAB_LEASE_RANGE_START %q is not an IP address", s)
		}
	}
	if s := os.Getenv("BOOTD_LAB_LEASE_RANGE_END"); s != "" {
		cfg.LeaseRangeEnd = net.ParseIP(s)
		if cfg.LeaseRangeEnd == nil {
			t.Fatalf("BOOTD_LAB_LEASE_RANGE_END %q is not an IP address", s)
		}
	}

	d := &Dnsmasq{
		Config:     cfg,
		RunDir:     os.Getenv("BOOTD_LAB_RUN_DIR"),
		BinaryPath: os.Getenv("BOOTD_DNSMASQ_PATH"),
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	ctrlPath := os.Getenv("BOOTD_LAB_CTRL")
	if ctrlPath == "" {
		t.Fatal("BOOTD_LAB_CTRL is required")
	}
	ctrlFile, err := os.Open(ctrlPath)
	if err != nil {
		t.Fatalf("opening control FIFO: %v", err)
	}
	defer func() { _ = ctrlFile.Close() }()

	scanner := bufio.NewScanner(ctrlFile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "quit":
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("supervisor exited with error: %v", err)
			}
			log.Info("lab run complete")
			return
		case line == "deny":
			log.Info("control: deny all")
			d.SetAllowedMACs(nil)
		case strings.HasPrefix(line, "allow "):
			macs := strings.Split(strings.TrimPrefix(line, "allow "), ",")
			log.Info("control: allow", "macs", macs)
			d.SetAllowedMACs(macs)
		case line == "":
		default:
			t.Fatalf("unknown control command %q", line)
		}
		// Fail fast if the supervisor died while processing commands.
		select {
		case err := <-done:
			t.Fatalf("supervisor exited unexpectedly: %v", err)
		default:
		}
	}
	t.Fatalf("control FIFO closed without quit (scanner error: %v)", scanner.Err())
}
