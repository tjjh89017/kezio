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
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// DefaultDnsmasqPath is where docker/bootd's image installs
// dnsmasq-base's binary. An absolute default rather than a bare
// "dnsmasq" so the lookup never depends on the container's PATH.
const DefaultDnsmasqPath = "/usr/sbin/dnsmasq"

// DefaultRunDir is where the rendered dnsmasq config and its writable
// files (dhcp-hostsfile, leasefile) live by default - an emptyDir
// mount in the pod, since the container's root filesystem is
// read-only.
const DefaultRunDir = "/run/bootd"

// Dnsmasq renders a dnsmasq configuration (see RenderDnsmasqConf) and
// supervises a dnsmasq child process running it, restarting with backoff
// on crash and propagating a fatal error if it cannot stay up.
//
// It also implements MACSink: SetAllowedMACs rewrites the dhcp-hostsfile
// and SIGHUPs the child - dnsmasq re-reads hostsfiles on SIGHUP but not
// its main config, so the hostsfile is the only dynamic input.
//
// The hostsfile written at startup is empty: until the Machine informer
// syncs and pushes a snapshot (MACCache.Start), no MAC boots - fail-secure.
type Dnsmasq struct {
	// Config is the dnsmasq configuration to render. Required fields
	// per RenderDnsmasqConf.
	Config Config
	// RunDir is the writable directory holding the rendered config and
	// the dhcp-hostsfile. Also holds dnsmasq's leasefile unless LeaseDir
	// is set. Empty means DefaultRunDir.
	RunDir string
	// LeaseDir optionally places dnsmasq's leasefile on a directory
	// separate from RunDir - the persistent PVC mount a lease-mode Subnet
	// gets (see internal/controller's buildBootdDeployment), so the
	// leasefile survives a pod recreate that RunDir's emptyDir does not.
	// Empty means the leasefile lives under RunDir, unchanged from before
	// this field existed.
	LeaseDir string
	// BinaryPath is the dnsmasq executable to run. Empty means
	// DefaultDnsmasqPath.
	BinaryPath string
	// OnApplied, if set, is called after the hostsfile loop successfully
	// writes and SIGHUPs dnsmasq for a revision (see SetReservations) -
	// SubnetDHCPCache wires this to write status.dhcp.appliedRevision
	// back onto the Subnet. Called with the empty string when
	// SetReservations has never been called with a non-empty revision;
	// never called concurrently with itself.
	OnApplied func(ctx context.Context, revision string)
	// DHCPReleasePath is the dhcp_release executable Start uses to build
	// the default Release function. Empty means DefaultDHCPReleasePath.
	// Ignored when Release is set directly.
	DHCPReleasePath string
	// Release sends a DHCPRELEASE for a lease a MAC no longer allowed to
	// hold is found to still have (see releaseLeases). Nil means
	// execDHCPRelease(resolved DHCPReleasePath) - a test seam otherwise.
	Release ReleaseFunc
	// SubnetMember, if set, reports whether a Machine still enrolled with
	// mac as its boot MAC targets this bootd instance's own Subnet (see
	// MACCache.HasLocalMember). SetReservations uses it to tell a
	// Machine's ordinary Complete release (reservation gone, Machine
	// still enrolled on this Subnet - the OS keeps its lease) apart from
	// a spec.subnetRef change (reservation gone, Machine now enrolled
	// elsewhere - the address must be actively released). Nil means
	// bootd cannot tell the two apart, so a reservation disappearing
	// alone never triggers a release.
	SubnetMember func(mac string) bool
	// ReloadTimeout bounds how long hostsfileLoop waits for dnsmasq to
	// confirm (via its own log line, see hostsfileReadMarker) that it
	// has actually re-read the dhcp-hostsfile after a SIGHUP - a SIGHUP
	// only requests a reload dnsmasq performs whenever its event loop
	// next runs, which is not immediate under load, so appliedRevision
	// must wait for the confirmation rather than the signal send. Zero
	// means DefaultReloadTimeout.
	ReloadTimeout time.Duration

	mu           sync.Mutex
	macs         []string
	reservations map[string]string
	revision     string
	revSet       bool
	child        *os.Process
	// pendingReleases accumulates MACs SetAllowedMACs or SetReservations
	// has seen drop out of enrollment since the last hostsfileLoop tick,
	// so a burst of removals coalesced into one dirty signal still
	// releases every one of them, not just the most recent.
	pendingReleases []string

	dirtyOnce sync.Once
	dirty     chan struct{}

	// readMu/readSeq/readSig implement the hostsfile-reload-confirmation
	// signal: noteHostsfileRead (fed by runChild's stdout/stderr
	// forwarders matching hostsfileReadMarker) bumps readSeq and wakes
	// any waiter every time dnsmasq logs that it re-read the hostsfile,
	// whether that read followed a SIGHUP or was dnsmasq's own startup
	// read.
	readMu  sync.Mutex
	readSeq uint64
	readSig chan struct{}

	// firstMACsOnce/firstMACsSig close firstMACsCh() exactly once, the
	// first time SetAllowedMACs is called - the same fail-secure signal
	// MACCache.Start's first push already represents, reused here so
	// Start's LeaseMode startup filter (filterLeaseFile) waits for it
	// before ever launching dnsmasq.
	firstMACsChOnce sync.Once
	firstMACsSig    chan struct{}
	firstMACsOnce   sync.Once

	// Test seams, defaulted in Start: how long hostsfile rewrites are
	// coalesced before writing+SIGHUPing, the first restart delay
	// after a crash, and the uptime under which an exit counts as
	// "immediately after start" toward the fatal-exit limit.
	hupDebounce    time.Duration
	initialBackoff time.Duration
	fastExitWindow time.Duration
}

var _ manager.Runnable = (*Dnsmasq)(nil)
var _ MACSink = (*Dnsmasq)(nil)
var _ ReservationSink = (*Dnsmasq)(nil)

// maxBackoff caps the crash-restart delay.
const maxBackoff = 30 * time.Second

// maxFastExits is how many consecutive immediate exits (each within
// fastExitWindow of its start) Start tolerates before treating the
// failure as fatal and returning an error - an exit that fast, over
// and over, means dnsmasq cannot even hold its sockets or parse its
// config, and endless silent restarting would hide that from the
// operator; a crashing pod is the honest signal.
const maxFastExits = 3

// DefaultReloadTimeout is how long hostsfileLoop waits for dnsmasq to
// confirm a hostsfile reload when Dnsmasq.ReloadTimeout is left zero.
const DefaultReloadTimeout = 30 * time.Second

// hostsfileReadMarker returns the substring dnsmasq's own log line
// carries when it (re-)reads path, the dhcp-hostsfile - on a
// SIGHUP-triggered reload and on its own startup read alike.
// Lab-confirmed wording: "dnsmasq-dhcp: read /run/bootd/dhcp-hosts.conf".
func hostsfileReadMarker(path string) string {
	return "read " + path
}

// noteHostsfileRead records that dnsmasq has just re-read the
// hostsfile, bumping readSeq and waking every waiter blocked in
// waitForHostsfileReadAfter.
func (d *Dnsmasq) noteHostsfileRead() {
	d.readMu.Lock()
	d.readSeq++
	ch := d.readSig
	d.readSig = nil
	d.readMu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// currentReadSeq returns the number of hostsfile reads observed so far,
// for a caller to snapshot as the baseline waitForHostsfileReadAfter
// should wait past.
func (d *Dnsmasq) currentReadSeq() uint64 {
	d.readMu.Lock()
	defer d.readMu.Unlock()
	return d.readSeq
}

// waitForHostsfileReadAfter blocks until a hostsfile read has been
// observed with sequence number greater than after (true), ctx is
// cancelled, or timeout elapses (false) - whichever comes first. A read
// that happened at or before after (an earlier reload, or one left over
// from a previous dnsmasq child) never satisfies the wait.
func (d *Dnsmasq) waitForHostsfileReadAfter(ctx context.Context, after uint64, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		d.readMu.Lock()
		if d.readSeq > after {
			d.readMu.Unlock()
			return true
		}
		if d.readSig == nil {
			d.readSig = make(chan struct{})
		}
		ch := d.readSig
		d.readMu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		}
	}
}

// requeueDirty nudges hostsfileLoop to run another iteration after the
// next debounce window, the same non-blocking signal SetAllowedMACs and
// SetReservations use - so a SIGHUP that failed to send, or was never
// confirmed, gets retried without waiting for something external to
// change.
func (d *Dnsmasq) requeueDirty() {
	select {
	case d.dirtyCh() <- struct{}{}:
	default:
	}
}

// dirtyCh lazily builds the coalescing channel so SetAllowedMACs is
// safe to call before Start - the manager starts its Runnables
// concurrently, and the Machine informer can push its first snapshot
// before this Runnable's Start has run.
func (d *Dnsmasq) dirtyCh() chan struct{} {
	d.dirtyOnce.Do(func() { d.dirty = make(chan struct{}, 1) })
	return d.dirty
}

// firstMACsChan lazily builds the channel Start's LeaseMode startup
// filter waits on, closed the first time SetAllowedMACs is called.
func (d *Dnsmasq) firstMACsChan() chan struct{} {
	d.firstMACsChOnce.Do(func() { d.firstMACsSig = make(chan struct{}) })
	return d.firstMACsSig
}

// SetAllowedMACs implements MACSink: it records the new allowlist and
// nudges the hostsfile loop (see Start). Non-blocking - the channel
// carries only a "something changed" bit, and the loop always renders
// from the latest recorded list, so any number of pushes coalesce
// into at most one pending rewrite.
//
// In LeaseMode, a MAC present in the old allowlist but absent from macs
// is queued for an active DHCPRELEASE (see releaseLeases): it stopped
// being an enrolled Machine's boot MAC (deleted, or its BootMACAddress
// changed), and its lease - if it has one - must not linger for the
// segment's own lease time. A Machine's normal Deployed-to-Complete
// transition never removes its MAC from this set, so a completed
// deployment's OS keeps renewing its lease undisturbed.
func (d *Dnsmasq) SetAllowedMACs(macs []string) {
	d.mu.Lock()
	old := d.macs
	d.macs = slices.Clone(macs)
	if d.Config.LeaseMode {
		d.pendingReleases = append(d.pendingReleases, macDiff(old, d.macs)...)
	}
	d.mu.Unlock()
	d.firstMACsOnce.Do(func() { close(d.firstMACsChan()) })
	select {
	case d.dirtyCh() <- struct{}{}:
	default:
	}
}

// SetReservations implements ReservationSink: it records the current
// lease-mode DHCP reservation table (normalized MAC -> IPv4 address) and
// the revision it was computed from, then nudges the hostsfile loop the
// same way SetAllowedMACs does. Once the loop writes+SIGHUPs dnsmasq for
// this exact revision, OnApplied (if set) is called with it.
//
// In LeaseMode, a MAC that held a reservation in the previous push but
// has none now, while still present in the allowlist (a MAC that also
// left the allowlist is SetAllowedMACs' own release to queue, not this
// one's), is queued for an active DHCPRELEASE only when SubnetMember
// reports it no longer targets this bootd's own Subnet - a
// spec.subnetRef change, whose old Subnet must not keep holding its
// address. A MAC SubnetMember still finds local is left alone: an
// ordinary Complete release also drops the reservation, and a completed
// deployment's OS must keep renewing its lease undisturbed. With
// SubnetMember unset, the two cases cannot be told apart, so nothing is
// ever released from a reservation disappearing alone (see
// Dnsmasq.SubnetMember).
func (d *Dnsmasq) SetReservations(revision string, reservations map[string]string) {
	d.mu.Lock()
	old := d.reservations
	macs := d.macs
	d.reservations = maps.Clone(reservations)
	d.revision = revision
	d.revSet = true
	if d.Config.LeaseMode && d.SubnetMember != nil {
		for mac := range old {
			if _, stillReserved := reservations[mac]; stillReserved {
				continue
			}
			if !slices.Contains(macs, mac) {
				continue
			}
			if !d.SubnetMember(mac) {
				d.pendingReleases = append(d.pendingReleases, mac)
			}
		}
	}
	d.mu.Unlock()
	select {
	case d.dirtyCh() <- struct{}{}:
	default:
	}
}

// Start implements manager.Runnable: render the config, write the
// initial hostsfile, then supervise dnsmasq until ctx is cancelled.
func (d *Dnsmasq) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-dnsmasq")

	runDir := d.RunDir
	if runDir == "" {
		runDir = DefaultRunDir
	}
	binary := d.BinaryPath
	if binary == "" {
		binary = DefaultDnsmasqPath
	}
	if d.hupDebounce == 0 {
		d.hupDebounce = 200 * time.Millisecond
	}
	if d.initialBackoff == 0 {
		d.initialBackoff = time.Second
	}
	if d.fastExitWindow == 0 {
		d.fastExitWindow = 10 * time.Second
	}
	if d.ReloadTimeout == 0 {
		d.ReloadTimeout = DefaultReloadTimeout
	}

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("creating run directory %s: %w", runDir, err)
	}
	leaseFilePath := filepath.Join(runDir, leasefileName)
	if d.LeaseDir != "" {
		if err := os.MkdirAll(d.LeaseDir, 0o755); err != nil {
			return fmt.Errorf("creating lease directory %s: %w", d.LeaseDir, err)
		}
		leaseFilePath = filepath.Join(d.LeaseDir, leasefileName)
	}
	d.Config.LeaseFilePath = leaseFilePath

	conf, err := RenderDnsmasqConf(d.Config, runDir)
	if err != nil {
		return fmt.Errorf("rendering dnsmasq config: %w", err)
	}
	confPath := filepath.Join(runDir, "dnsmasq.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("writing dnsmasq config: %w", err)
	}
	d.mu.Lock()
	initial := RenderHostsfile(d.macs, d.reservations)
	d.mu.Unlock()
	if err := writeFileAtomic(HostsfilePath(runDir), initial); err != nil {
		return fmt.Errorf("writing initial dhcp-hostsfile: %w", err)
	}

	if d.Config.LeaseMode {
		// Wait for the Machine allowlist's first push (MACCache.Start's
		// fail-secure sync gate) before dnsmasq ever reads the persisted
		// lease file: a lease surviving a bootd restart on behalf of a
		// Machine deleted or re-MACed while bootd was down must not be
		// handed back out before the allowlist is known.
		select {
		case <-d.firstMACsChan():
			d.mu.Lock()
			allowed := slices.Clone(d.macs)
			d.mu.Unlock()
			dropped, err := filterLeaseFile(leaseFilePath, allowed)
			if err != nil {
				log.Error(err, "filtering persisted lease file at startup failed; stale leases may linger", "path", leaseFilePath)
			} else {
				log.Info("filtered persisted lease file against Machine allowlist", "path", leaseFilePath, "droppedLeases", dropped)
			}
		case <-ctx.Done():
			return nil
		}
	}

	// The dnsmasq child needs bootd's own capabilities (see caps.go).
	// bootd runs as uid 0, so execve alone re-grants them from the
	// bounding set; raiseAmbientCaps is best-effort belt-and-braces
	// for non-root environments.
	raiseAmbientCaps(log)

	hostsfilePath := HostsfilePath(runDir)
	go d.hostsfileLoop(ctx, log, runDir, leaseFilePath, initial)

	backoff := d.initialBackoff
	fastExits := 0
	for {
		started := time.Now()
		exitErr, runErr := d.runChild(ctx, log, binary, confPath, hostsfilePath)
		if runErr != nil {
			return runErr
		}
		if ctx.Err() != nil {
			return nil
		}

		uptime := time.Since(started)
		if uptime < d.fastExitWindow {
			fastExits++
			if fastExits >= maxFastExits {
				return fmt.Errorf("dnsmasq exited %d consecutive times within %v of starting (last exit: %v); giving up", fastExits, d.fastExitWindow, exitErr)
			}
		} else {
			fastExits = 0
			backoff = d.initialBackoff
		}

		log.Error(exitErr, "dnsmasq exited; restarting", "uptime", uptime.String(), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runChild runs one dnsmasq child to completion. The first return
// value is the child's exit result (nil for a clean exit); the second
// is a startup error that makes supervision itself impossible (binary
// missing, pipes unavailable) and is returned fatally by Start. On
// ctx cancellation the child is SIGTERMed and awaited before
// returning.
func (d *Dnsmasq) runChild(ctx context.Context, log logr.Logger, binary, confPath, hostsfilePath string) (exitErr error, fatal error) {
	// --no-daemon: stay a direct child (supervision/SIGHUP need the
	// pid), log to stderr, skip the pidfile.
	//
	// --user=root: started as root, dnsmasq otherwise drops to "nobody"
	// via setuid/setgid, which needs CAP_SETUID/CAP_SETGID - capabilities
	// the pod deliberately doesn't grant (only the three in caps.go).
	// Without this flag dnsmasq dies at startup on the failed drop; same
	// flag kube-dns uses, for the same reason. Inert when already
	// non-root (the lab's setpriv path).
	cmd := exec.Command(binary, "--conf-file="+confPath, "--no-daemon", "--user=root")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening dnsmasq stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("opening dnsmasq stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting dnsmasq (%s): %w", binary, err)
	}
	log.Info("dnsmasq started", "pid", cmd.Process.Pid, "config", confPath)

	d.mu.Lock()
	d.child = cmd.Process
	d.mu.Unlock()

	marker := hostsfileReadMarker(hostsfilePath)
	onLine := func(line string) {
		if strings.Contains(line, marker) {
			d.noteHostsfileRead()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); forwardLines(log, stdout, onLine) }()
	go func() { defer wg.Done(); forwardLines(log, stderr, onLine) }()

	waitCh := make(chan error, 1)
	go func() {
		wg.Wait()
		waitCh <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(sigterm) // dnsmasq's clean-shutdown signal
		exitErr = <-waitCh
	case exitErr = <-waitCh:
	}

	d.mu.Lock()
	d.child = nil
	d.mu.Unlock()
	return exitErr, nil
}

// hostsfileLoop debounces a burst of dirty signals into one atomic
// hostsfile rewrite + SIGHUP, and - in LeaseMode - one round of active
// DHCPRELEASEs for MACs the debounced burst dropped from the allowlist
// (see releaseLeases). Byte-identical hostsfile content skips the
// rewrite, but pending releases are always processed regardless, since a
// MAC can drop out and back in within one debounce window with no net
// hostsfile change.
//
// OnApplied is called only once dnsmasq has actually confirmed (via its
// own log line, see hostsfileReadMarker) that it re-read the hostsfile -
// a SIGHUP only requests that reload, which dnsmasq performs whenever
// its event loop next runs, not immediately, so acking on the signal
// send alone can report a revision applied before dnsmasq has actually
// served it. Whenever the last write is not yet confirmed (a fresh
// write, a SIGHUP that failed to send, or a previous wait that timed
// out), this resends the SIGHUP and waits again - a redundant SIGHUP
// for content dnsmasq already has is harmless. If ReloadTimeout elapses
// with no confirming read, the tick ends without calling OnApplied and
// requeues itself for another attempt; the deployer's own requeue loop
// is the fail-safe in the meantime.
func (d *Dnsmasq) hostsfileLoop(ctx context.Context, log logr.Logger, runDir, leaseFilePath, lastWritten string) {
	path := HostsfilePath(runDir)
	var lastAppliedRev string
	lastAppliedRevSet := false
	// confirmed is whether dnsmasq has been observed to have actually
	// read lastWritten's exact content since it was written.
	confirmed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.dirtyCh():
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.hupDebounce):
		}
		// Drain a signal that raced in during the debounce window: the
		// render below reads the latest list anyway.
		select {
		case <-d.dirtyCh():
		default:
		}

		d.mu.Lock()
		content := RenderHostsfile(d.macs, d.reservations)
		child := d.child
		revision, revSet := d.revision, d.revSet
		toRelease := d.pendingReleases
		d.pendingReleases = nil
		d.mu.Unlock()

		if len(toRelease) > 0 {
			d.releaseLeases(log, leaseFilePath, toRelease)
		}

		if content != lastWritten {
			if err := writeFileAtomic(path, content); err != nil {
				log.Error(err, "rewriting dhcp-hostsfile failed; MAC allowlist is stale until the next change", "path", path)
				continue
			}
			lastWritten = content
			confirmed = false
		}

		if !confirmed {
			if child == nil {
				// No child to SIGHUP (mid-restart, or still gated in
				// Start's LeaseMode startup filter) - nothing to confirm
				// yet; retry once something runs.
				d.requeueDirty()
				continue
			}
			baseline := d.currentReadSeq()
			if err := child.Signal(sighup); err != nil {
				log.Error(err, "SIGHUP to dnsmasq failed; allowlist applies at next restart", "pid", child.Pid)
				d.requeueDirty()
				continue
			}
			if !d.waitForHostsfileReadAfter(ctx, baseline, d.ReloadTimeout) {
				if ctx.Err() != nil {
					return
				}
				log.Error(fmt.Errorf("no confirming read of %s within %v", path, d.ReloadTimeout), "dnsmasq did not confirm the dhcp-hostsfile reload; revision not marked applied, will retry", "revision", revision)
				d.requeueDirty()
				continue
			}
			confirmed = true
			log.Info("dnsmasq confirmed dhcp-hostsfile reload", "path", path, "entries", countLines(content))
		}

		// The hostsfile now reflects revision, confirmed read by dnsmasq
		// itself: tell OnApplied, unless this exact revision was already
		// reported.
		if revSet && (!lastAppliedRevSet || revision != lastAppliedRev) {
			lastAppliedRev, lastAppliedRevSet = revision, true
			if d.OnApplied != nil {
				d.OnApplied(ctx, revision)
			}
		}
	}
}

// releaseLeases sends a DHCPRELEASE for each of macs that currently holds
// a lease in dnsmasq's lease file, so the address returns to the pool
// immediately instead of waiting out the segment's lease time. A MAC
// with no current lease is skipped silently - there is nothing to
// release.
func (d *Dnsmasq) releaseLeases(log logr.Logger, leaseFilePath string, macs []string) {
	if d.Config.Interface == "" {
		log.Error(fmt.Errorf("no interface configured"), "cannot send DHCPRELEASE with no interface; leases will linger until they expire", "macs", macs)
		return
	}
	leases, err := readLeaseFileByMAC(leaseFilePath)
	if err != nil {
		log.Error(err, "reading lease file for DHCPRELEASE failed; releases skipped", "path", leaseFilePath)
		return
	}
	release := d.Release
	if release == nil {
		releasePath := d.DHCPReleasePath
		if releasePath == "" {
			releasePath = DefaultDHCPReleasePath
		}
		release = execDHCPRelease(releasePath)
	}
	for _, mac := range macs {
		ip, ok := leases[mac]
		if !ok {
			continue
		}
		if err := release(d.Config.Interface, ip, mac); err != nil {
			log.Error(err, "dhcp_release failed; lease will linger until it expires", "mac", mac, "ip", ip)
			continue
		}
		log.Info("released DHCP lease for a MAC no longer enrolled", "mac", mac, "ip", ip)
	}
}

// forwardLines copies each line of r into log, calling onLine (if
// non-nil) with the same line - runChild uses that to notice dnsmasq's
// hostsfile-reread confirmation without a second scan of the stream. A
// read error other than EOF ends forwarding early and is itself logged -
// that pipe is the only channel dnsmasq's diagnostics reach the operator
// through.
func forwardLines(log logr.Logger, r io.Reader, onLine func(string)) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		log.Info(line)
		if onLine != nil {
			onLine(line)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Error(err, "reading dnsmasq output failed; log forwarding stopped early")
	}
}

// countLines counts non-empty lines, for the "entries" log field only.
func countLines(s string) int {
	n := 0
	for line := range strings.SplitSeq(s, "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

// writeFileAtomic writes content to path via a temp file and rename,
// so dnsmasq re-reading the hostsfile mid-write can never observe a
// half-written allowlist.
func writeFileAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
