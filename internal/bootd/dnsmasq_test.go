/*
Copyright 2026.

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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// writeFakeDnsmasq writes an executable shell script standing in for
// the dnsmasq binary, so supervisor behavior (spawn, log forwarding,
// SIGHUP, crash restart, shutdown) is testable without dnsmasq or
// network namespaces. The script receives the same argv the real
// binary would.
func writeFakeDnsmasq(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-dnsmasq")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestDnsmasq(t *testing.T, script string) (*Dnsmasq, string) {
	t.Helper()
	runDir := t.TempDir()
	d := &Dnsmasq{
		Config:         testConfig(),
		RunDir:         runDir,
		BinaryPath:     writeFakeDnsmasq(t, runDir, script),
		ReloadTimeout:  50 * time.Millisecond,
		hupDebounce:    10 * time.Millisecond,
		initialBackoff: 10 * time.Millisecond,
		fastExitWindow: 200 * time.Millisecond,
	}
	return d, runDir
}

// waitFor polls cond until it reports true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const testDHCPRevision1 = "rev-1"

const longRunningScript = `trap 'exit 0' TERM INT
echo fake-dnsmasq-started
while :; do sleep 0.05; done
`

func TestDnsmasq_WritesConfigAndInitialHostsfile(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	sink := &recordingSink{}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), newRecordingLogger(sink)))
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	// Wait for the child spawn, which Start does after every start-up
	// write - not for the config file, which exists before it holds
	// its content.
	waitFor(t, "dnsmasq child start", func() bool {
		return containsSubstring(sink.messages(), "dnsmasq started")
	})
	conf, err := os.ReadFile(filepath.Join(runDir, "dnsmasq.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "dhcp-range=192.0.2.0,proxy,255.255.255.0") {
		t.Errorf("written config missing proxy dhcp-range:\n%s", conf)
	}

	hosts, err := os.ReadFile(HostsfilePath(runDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("initial hostsfile = %q, want empty (fail-secure: nothing boots before the informer syncs)", hosts)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v after ctx cancellation, want nil", err)
	}
}

func TestDnsmasq_ForwardsChildOutputToLogger(t *testing.T) {
	d, _ := newTestDnsmasq(t, longRunningScript)
	sink := &recordingSink{}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), newRecordingLogger(sink)))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "forwarded child output", func() bool {
		return containsSubstring(sink.messages(), "fake-dnsmasq-started")
	})
	cancel()
	<-done
}

func TestDnsmasq_SetAllowedMACsRewritesHostsfileAndSIGHUPs(t *testing.T) {
	// The fake dnsmasq appends a line to a marker file on every HUP,
	// standing in for dnsmasq's hostsfile re-read.
	d, runDir := newTestDnsmasq(t, `marker="$MARKER_FILE"
trap 'echo hup >> "$marker"' HUP
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	marker := filepath.Join(runDir, "hup-marker")
	t.Setenv("MARKER_FILE", marker)

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "hostsfile", func() bool {
		_, err := os.Stat(HostsfilePath(runDir))
		return err == nil
	})

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})

	waitFor(t, "hostsfile rewrite", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && string(got) == "aa:bb:cc:dd:ee:01,set:kezio\n"
	})
	waitFor(t, "SIGHUP delivery", func() bool {
		got, err := os.ReadFile(marker)
		return err == nil && strings.Contains(string(got), "hup")
	})

	// Flipping back to empty (informer revoked the Machine) must
	// rewrite to the fail-secure empty file, not leave the stale MAC.
	d.SetAllowedMACs(nil)
	waitFor(t, "hostsfile revocation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && len(got) == 0
	})

	cancel()
	<-done
}

func TestDnsmasq_SetReservationsRendersAddressesAndReportsApplied(t *testing.T) {
	d, runDir := newTestDnsmasq(t, `marker="$MARKER_FILE"
trap 'echo hup >> "$marker"; echo "dnsmasq-dhcp: read $HOSTS_PATH"' HUP
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	marker := filepath.Join(runDir, "hup-marker")
	t.Setenv("MARKER_FILE", marker)
	t.Setenv("HOSTS_PATH", HostsfilePath(runDir))

	var mu sync.Mutex
	var applied []string
	d.OnApplied = func(_ context.Context, revision string) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, revision)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "hostsfile", func() bool {
		_, err := os.Stat(HostsfilePath(runDir))
		return err == nil
	})

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"})

	waitFor(t, "hostsfile with a reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && string(got) == "aa:bb:cc:dd:ee:01,set:kezio,192.0.2.10\naa:bb:cc:dd:ee:02,set:kezio\n"
	})
	waitFor(t, "OnApplied called with rev-1", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 1 && applied[0] == testDHCPRevision1
	})

	// A later revision with the exact same rendered content (a
	// reservation added for a MAC not in the allowlist) still must be
	// separately reported applied - the hostsfile reflects it, so a
	// waiting Machine must not be kept blocked on a revision that in
	// fact took effect trivially.
	d.SetReservations("rev-2", map[string]string{
		"aa:bb:cc:dd:ee:01": "192.0.2.10",
		"aa:bb:cc:dd:ee:99": "192.0.2.11",
	})
	waitFor(t, "OnApplied called with rev-2", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 2 && applied[1] == "rev-2"
	})

	cancel()
	<-done
}

// TestDnsmasq_OnAppliedWaitsForDnsmasqReloadConfirmation proves OnApplied
// is not called merely because a SIGHUP was sent: the fake dnsmasq here
// delays its confirming "read" line well past the SIGHUP, standing in
// for the real dnsmasq's reload lagging under load (the bug this whole
// mechanism exists to close).
func TestDnsmasq_OnAppliedWaitsForDnsmasqReloadConfirmation(t *testing.T) {
	d, runDir := newTestDnsmasq(t, `trap 'sleep 0.2; echo "dnsmasq-dhcp: read $HOSTS_PATH"' HUP
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	t.Setenv("HOSTS_PATH", HostsfilePath(runDir))
	d.ReloadTimeout = 2 * time.Second

	var mu sync.Mutex
	var applied []string
	d.OnApplied = func(_ context.Context, revision string) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, revision)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "hostsfile", func() bool {
		_, err := os.Stat(HostsfilePath(runDir))
		return err == nil
	})

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"})

	waitFor(t, "hostsfile with the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "192.0.2.10")
	})

	// The write (and therefore the SIGHUP) has already happened, but the
	// fake dnsmasq's confirming read line is still 200ms away.
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	early := len(applied)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("OnApplied called %d times before dnsmasq confirmed the reload, want 0", early)
	}

	waitFor(t, "OnApplied called once dnsmasq confirms the reload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 1 && applied[0] == testDHCPRevision1
	})

	cancel()
	<-done
}

// TestDnsmasq_StaleReadDoesNotSatisfyANewerRevision proves a read line
// already spent confirming one revision cannot also confirm a later one:
// the fake dnsmasq here only ever emits its confirming line on the first
// SIGHUP it receives, so a second revision's SIGHUP must time out rather
// than being satisfied by that leftover line.
func TestDnsmasq_StaleReadDoesNotSatisfyANewerRevision(t *testing.T) {
	d, runDir := newTestDnsmasq(t, `count_file="$COUNT_FILE"
trap '
n=$(( $(cat "$count_file" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$count_file"
if [ "$n" -eq 1 ]; then echo "dnsmasq-dhcp: read $HOSTS_PATH"; fi
' HUP
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	countFile := filepath.Join(runDir, "hup-count")
	t.Setenv("COUNT_FILE", countFile)
	t.Setenv("HOSTS_PATH", HostsfilePath(runDir))

	var mu sync.Mutex
	var applied []string
	d.OnApplied = func(_ context.Context, revision string) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, revision)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "hostsfile", func() bool {
		_, err := os.Stat(HostsfilePath(runDir))
		return err == nil
	})

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"})
	waitFor(t, "OnApplied called with rev-1", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 1 && applied[0] == testDHCPRevision1
	})

	d.SetReservations("rev-2", map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.11"})
	waitFor(t, "hostsfile reflects rev-2", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "192.0.2.11")
	})

	// Give the (short, test-seam) ReloadTimeout several chances to expire
	// and retry - the fake dnsmasq stays silent on every SIGHUP past the
	// first, so none of those retries can confirm rev-2 either.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), applied...)
	mu.Unlock()
	if len(got) != 1 || got[0] != testDHCPRevision1 {
		t.Fatalf("applied = %v, want only rev-1 - rev-2 must not be satisfied by rev-1's stale read", got)
	}

	cancel()
	<-done
}

// TestDnsmasq_ReloadTimeoutRetriesSighupWithoutApplying proves that when
// dnsmasq never confirms a reload at all, hostsfileLoop never calls
// OnApplied, logs the failure, and keeps resending the SIGHUP rather
// than giving up.
func TestDnsmasq_ReloadTimeoutRetriesSighupWithoutApplying(t *testing.T) {
	d, runDir := newTestDnsmasq(t, `count_file="$COUNT_FILE"
trap 'n=$(( $(cat "$count_file" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$count_file"' HUP
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	countFile := filepath.Join(runDir, "hup-count")
	t.Setenv("COUNT_FILE", countFile)

	sink := &recordingSink{}
	var mu sync.Mutex
	var applied []string
	d.OnApplied = func(_ context.Context, revision string) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, revision)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), newRecordingLogger(sink)))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "hostsfile", func() bool {
		_, err := os.Stat(HostsfilePath(runDir))
		return err == nil
	})

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"})

	waitFor(t, "the timeout is logged", func() bool {
		return containsSubstring(sink.messages(), "did not confirm the dhcp-hostsfile reload")
	})
	waitFor(t, "the SIGHUP is retried", func() bool {
		got, err := os.ReadFile(countFile)
		if err != nil {
			return false
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(got)))
		return n >= 2
	})

	mu.Lock()
	got := len(applied)
	mu.Unlock()
	if got != 0 {
		t.Errorf("OnApplied called %d times though dnsmasq never confirmed the reload, want 0", got)
	}

	cancel()
	<-done
}

// releaseCall records one ReleaseFunc invocation, for the recording
// ReleaseFunc the release-trigger tests below use in place of a real
// dhcp_release binary.
type releaseCall struct{ iface, ip, mac string }

// recordingRelease is a ReleaseFunc test double that appends every call
// it receives, safe for concurrent use by hostsfileLoop's goroutine.
type recordingRelease struct {
	mu    sync.Mutex
	calls []releaseCall
}

func (r *recordingRelease) release(iface, ip, mac string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, releaseCall{iface, ip, mac})
	return nil
}

func (r *recordingRelease) snapshot() []releaseCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]releaseCall(nil), r.calls...)
}

// testLeaseLine is one dnsmasq lease file line shared by every
// release-trigger test below: MAC aa:bb:cc:dd:ee:01 holding 192.0.2.50.
const testLeaseLine = "1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *\n"

// TestDnsmasq_ReleasesLeaseWhenMACLeavesAllowlist proves a MAC dropping
// out of the allowlist while it holds a lease in the persisted lease file
// gets an active DHCPRELEASE, so its address returns to the pool
// immediately instead of waiting out the segment's lease time.
func TestDnsmasq_ReleasesLeaseWhenMACLeavesAllowlist(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release

	leasePath := filepath.Join(runDir, leasefileName)
	if err := os.WriteFile(leasePath, []byte(testLeaseLine), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	// Initial sync: the MAC is enrolled, so the startup filter keeps its
	// lease and nothing is released yet.
	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	waitFor(t, "hostsfile reflects the initial sync", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "aa:bb:cc:dd:ee:01")
	})
	if got := rel.snapshot(); len(got) != 0 {
		t.Fatalf("Release called %v before any MAC left the allowlist, want no calls", got)
	}

	// The Machine is deleted (or re-MACed): its MAC drops out.
	d.SetAllowedMACs(nil)
	waitFor(t, "DHCPRELEASE for the de-enrolled MAC", func() bool {
		return len(rel.snapshot()) > 0
	})

	got := rel.snapshot()
	want := []releaseCall{{iface: "net1", ip: "192.0.2.50", mac: "aa:bb:cc:dd:ee:01"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Release calls = %#v, want %#v", got, want)
	}

	cancel()
	<-done
}

// TestDnsmasq_NoReleaseForStillEnrolledMAC proves a MAC that stays
// enrolled across a SetAllowedMACs push - only the allowlist's other
// members changing - triggers no release, matching the "not on an
// ordinary Complete" requirement: nothing about finishing a deployment
// removes a Machine's MAC from this set.
func TestDnsmasq_NoReleaseForStillEnrolledMAC(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release

	leasePath := filepath.Join(runDir, leasefileName)
	if err := os.WriteFile(leasePath, []byte(testLeaseLine), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	waitFor(t, "initial hostsfile", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "aa:bb:cc:dd:ee:01")
	})

	// A second Machine enrolls; the first MAC stays present throughout.
	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"})
	waitFor(t, "hostsfile reflects the new enrollment", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "aa:bb:cc:dd:ee:02")
	})

	if got := rel.snapshot(); len(got) != 0 {
		t.Errorf("Release called %v for a MAC that never left the allowlist, want no calls", got)
	}

	cancel()
	<-done
}

// TestDnsmasq_NoReleaseForMACWithoutALease proves a de-enrolled MAC
// holding no current lease is skipped rather than erroring: there is
// nothing for dhcp_release to release.
func TestDnsmasq_NoReleaseForMACWithoutALease(t *testing.T) {
	d, _ := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	waitFor(t, "initial hostsfile", func() bool {
		got, err := os.ReadFile(HostsfilePath(d.RunDir))
		return err == nil && len(got) > 0
	})

	d.SetAllowedMACs(nil)
	// No lease file entry exists for the MAC, so give the loop a chance
	// to run and confirm it stays quiet rather than waiting for a call
	// that will never come.
	time.Sleep(50 * time.Millisecond)
	if got := rel.snapshot(); len(got) != 0 {
		t.Errorf("Release called %v for a MAC with no current lease, want no calls", got)
	}

	cancel()
	<-done
}

// TestDnsmasq_ReleasesLeaseWhenReservationMovesToAnotherSubnet proves a
// MAC that stays in the allowlist but loses its reservation entry gets an
// active DHCPRELEASE when SubnetMember reports it no longer targets this
// bootd's own Subnet - the spec.subnetRef-change case, which SetAllowedMACs'
// own diff cannot see because the MAC never left the allowlist.
func TestDnsmasq_ReleasesLeaseWhenReservationMovesToAnotherSubnet(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release
	d.SubnetMember = func(mac string) bool { return false } // moved elsewhere

	leasePath := filepath.Join(runDir, leasefileName)
	if err := os.WriteFile(leasePath, []byte(testLeaseLine), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.50"})
	waitFor(t, "hostsfile with the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "192.0.2.50")
	})

	// The reservation disappears (moved to another Subnet's own status),
	// but the MAC stays enrolled cluster-wide.
	d.SetReservations("rev-2", map[string]string{})
	waitFor(t, "DHCPRELEASE for the moved MAC", func() bool {
		return len(rel.snapshot()) > 0
	})

	got := rel.snapshot()
	want := []releaseCall{{iface: "net1", ip: "192.0.2.50", mac: "aa:bb:cc:dd:ee:01"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Release calls = %#v, want %#v", got, want)
	}

	cancel()
	<-done
}

// TestDnsmasq_NoReleaseWhenReservationDropsForCompletedDeploy proves a
// MAC that stays in the allowlist and loses its reservation entry
// triggers no release when SubnetMember reports it still targets this
// bootd's own Subnet - the ordinary Complete-release case, where the OS
// must keep renewing its lease undisturbed.
func TestDnsmasq_NoReleaseWhenReservationDropsForCompletedDeploy(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release
	d.SubnetMember = func(mac string) bool { return true } // still enrolled here

	leasePath := filepath.Join(runDir, leasefileName)
	if err := os.WriteFile(leasePath, []byte(testLeaseLine), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.50"})
	waitFor(t, "hostsfile with the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "192.0.2.50")
	})

	d.SetReservations("rev-2", map[string]string{})
	waitFor(t, "hostsfile without the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && !strings.Contains(string(got), "192.0.2.50")
	})

	if got := rel.snapshot(); len(got) != 0 {
		t.Errorf("Release called %v for a Machine still enrolled on this Subnet, want no calls", got)
	}

	cancel()
	<-done
}

// TestDnsmasq_NoReleaseFromReservationWithoutSubnetMember proves that
// with SubnetMember unset, a reservation disappearing never triggers a
// release on its own: bootd cannot tell a Complete release apart from a
// subnetRef change, and the documented behavior is to never guess.
func TestDnsmasq_NoReleaseFromReservationWithoutSubnetMember(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true
	rel := &recordingRelease{}
	d.Release = rel.release

	leasePath := filepath.Join(runDir, leasefileName)
	if err := os.WriteFile(leasePath, []byte(testLeaseLine), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})
	d.SetReservations(testDHCPRevision1, map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.50"})
	waitFor(t, "hostsfile with the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && strings.Contains(string(got), "192.0.2.50")
	})

	d.SetReservations("rev-2", map[string]string{})
	waitFor(t, "hostsfile without the reservation", func() bool {
		got, err := os.ReadFile(HostsfilePath(runDir))
		return err == nil && !strings.Contains(string(got), "192.0.2.50")
	})

	if got := rel.snapshot(); len(got) != 0 {
		t.Errorf("Release called %v with no SubnetMember to tell Complete apart from a subnetRef change, want no calls", got)
	}

	cancel()
	<-done
}

// TestDnsmasq_StartupFilterDropsLeasesOutsideAllowlist proves Start
// rewrites the persisted lease file against the current Machine
// allowlist before dnsmasq ever launches: a lease surviving a bootd
// restart on behalf of a since-deleted Machine must not be handed back
// out.
func TestDnsmasq_StartupFilterDropsLeasesOutsideAllowlist(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)
	d.Config.LeaseMode = true

	leasePath := filepath.Join(runDir, leasefileName)
	content := testLeaseLine + "1893456000 aa:bb:cc:dd:ee:02 192.0.2.51 host-2 *\n"
	if err := os.WriteFile(leasePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	// Start blocks on this push before ever launching dnsmasq (see
	// Start's LeaseMode branch); only ee:01 is still enrolled.
	d.SetAllowedMACs([]string{"aa:bb:cc:dd:ee:01"})

	waitFor(t, "lease file filtered", func() bool {
		got, err := os.ReadFile(leasePath)
		return err == nil && !strings.Contains(string(got), "aa:bb:cc:dd:ee:02")
	})
	got, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "aa:bb:cc:dd:ee:01") {
		t.Errorf("startup filter dropped an enrolled MAC's lease:\n%s", got)
	}

	cancel()
	<-done
}

func TestDnsmasq_RestartsCrashedChild(t *testing.T) {
	// First two runs crash immediately; the third stays up. The
	// supervisor must keep restarting (staying under maxFastExits)
	// and settle on the healthy run.
	d, runDir := newTestDnsmasq(t, `count_file="$COUNT_FILE"
n=$(cat "$count_file" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$count_file"
if [ "$n" -lt 3 ]; then exit 1; fi
trap 'exit 0' TERM INT
while :; do sleep 0.05; done
`)
	countFile := filepath.Join(runDir, "count")
	t.Setenv("COUNT_FILE", countFile)

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "third (healthy) child run", func() bool {
		got, err := os.ReadFile(countFile)
		return err == nil && strings.TrimSpace(string(got)) == "3"
	})

	// Still supervising, not exited: Start must not have returned.
	select {
	case err := <-done:
		t.Fatalf("Start returned early with %v while the healthy child was running", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v after ctx cancellation, want nil", err)
	}
}

func TestDnsmasq_GivesUpAfterRepeatedImmediateExits(t *testing.T) {
	d, _ := newTestDnsmasq(t, "exit 1\n")

	ctx := logf.IntoContext(context.Background(), logf.Log)
	err := d.Start(ctx)
	if err == nil {
		t.Fatal("Start returned nil for a child that always exits immediately; want a fatal error")
	}
	if !strings.Contains(err.Error(), "giving up") {
		t.Errorf("Start error = %v, want it to name the give-up condition", err)
	}
}

func TestDnsmasq_StartFailsOnMissingBinary(t *testing.T) {
	d, _ := newTestDnsmasq(t, "")
	d.BinaryPath = filepath.Join(t.TempDir(), "does-not-exist")

	if err := d.Start(logf.IntoContext(context.Background(), logf.Log)); err == nil {
		t.Fatal("Start returned nil with a nonexistent dnsmasq binary")
	}
}

func TestDnsmasq_StartFailsOnInvalidConfig(t *testing.T) {
	d, _ := newTestDnsmasq(t, longRunningScript)
	d.Config.ServerIP = nil

	if err := d.Start(logf.IntoContext(context.Background(), logf.Log)); err == nil {
		t.Fatal("Start returned nil with an unrenderable config")
	}
}

// erroringReader yields lines then a non-EOF error, standing in for a
// dnsmasq stdout/stderr pipe that fails mid-read (the child dying
// abruptly, for example) rather than closing cleanly.
type erroringReader struct {
	lines []string
	err   error
	i     int
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.i < len(r.lines) {
		n := copy(p, r.lines[r.i])
		r.i++
		return n, nil
	}
	return 0, r.err
}

func TestForwardLines_LogsScannerErrorAfterReadFailure(t *testing.T) {
	sink := &recordingSink{}
	log := newRecordingLogger(sink)
	boom := errors.New("boom")

	forwardLines(log, &erroringReader{lines: []string{"hello\n"}, err: boom}, nil)

	msgs := sink.messages()
	if !containsSubstring(msgs, "hello") {
		t.Errorf("forwardLines dropped the line read before the failure: %v", msgs)
	}
	if !containsSubstring(msgs, "reading dnsmasq output failed") {
		t.Errorf("forwardLines did not log the scanner error: %v", msgs)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	for i, content := range []string{"one\n", "two\n", ""} {
		if err := writeFileAtomic(path, content); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if string(got) != content {
			t.Errorf("content %d = %q, want %q", i, got, content)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover temp files: %v", names)
	}
}
