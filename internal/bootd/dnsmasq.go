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
	"fmt"
	"io"
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
	// RunDir is the writable directory holding the rendered config,
	// the dhcp-hostsfile, and dnsmasq's leasefile. Empty means
	// DefaultRunDir.
	RunDir string
	// BinaryPath is the dnsmasq executable to run. Empty means
	// DefaultDnsmasqPath.
	BinaryPath string

	mu    sync.Mutex
	macs  []string
	child *os.Process

	dirtyOnce sync.Once
	dirty     chan struct{}

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

// maxBackoff caps the crash-restart delay.
const maxBackoff = 30 * time.Second

// maxFastExits is how many consecutive immediate exits (each within
// fastExitWindow of its start) Start tolerates before treating the
// failure as fatal and returning an error - an exit that fast, over
// and over, means dnsmasq cannot even hold its sockets or parse its
// config, and endless silent restarting would hide that from the
// operator; a crashing pod is the honest signal.
const maxFastExits = 3

// dirtyCh lazily builds the coalescing channel so SetAllowedMACs is
// safe to call before Start - the manager starts its Runnables
// concurrently, and the Machine informer can push its first snapshot
// before this Runnable's Start has run.
func (d *Dnsmasq) dirtyCh() chan struct{} {
	d.dirtyOnce.Do(func() { d.dirty = make(chan struct{}, 1) })
	return d.dirty
}

// SetAllowedMACs implements MACSink: it records the new allowlist and
// nudges the hostsfile loop (see Start). Non-blocking - the channel
// carries only a "something changed" bit, and the loop always renders
// from the latest recorded list, so any number of pushes coalesce
// into at most one pending rewrite.
func (d *Dnsmasq) SetAllowedMACs(macs []string) {
	d.mu.Lock()
	d.macs = slices.Clone(macs)
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

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("creating run directory %s: %w", runDir, err)
	}
	conf, err := RenderDnsmasqConf(d.Config, runDir)
	if err != nil {
		return fmt.Errorf("rendering dnsmasq config: %w", err)
	}
	confPath := filepath.Join(runDir, "dnsmasq.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("writing dnsmasq config: %w", err)
	}
	d.mu.Lock()
	initial := RenderHostsfile(d.macs)
	d.mu.Unlock()
	if err := writeFileAtomic(HostsfilePath(runDir), initial); err != nil {
		return fmt.Errorf("writing initial dhcp-hostsfile: %w", err)
	}

	// The dnsmasq child needs bootd's own capabilities (see caps.go).
	// bootd runs as uid 0, so execve alone re-grants them from the
	// bounding set; raiseAmbientCaps is best-effort belt-and-braces
	// for non-root environments.
	raiseAmbientCaps(log)

	go d.hostsfileLoop(ctx, log, runDir, initial)

	backoff := d.initialBackoff
	fastExits := 0
	for {
		started := time.Now()
		exitErr, runErr := d.runChild(ctx, log, binary, confPath)
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
func (d *Dnsmasq) runChild(ctx context.Context, log logr.Logger, binary, confPath string) (exitErr error, fatal error) {
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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); forwardLines(log, stdout) }()
	go func() { defer wg.Done(); forwardLines(log, stderr) }()

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
// hostsfile rewrite + SIGHUP. Byte-identical content skips the SIGHUP.
// If no child is running at signal time (mid-restart), the rewrite still
// happens and the next child reads the fresh file at startup.
func (d *Dnsmasq) hostsfileLoop(ctx context.Context, log logr.Logger, runDir, lastWritten string) {
	path := HostsfilePath(runDir)
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
		content := RenderHostsfile(d.macs)
		child := d.child
		d.mu.Unlock()

		if content == lastWritten {
			continue
		}
		if err := writeFileAtomic(path, content); err != nil {
			log.Error(err, "rewriting dhcp-hostsfile failed; MAC allowlist is stale until the next change", "path", path)
			continue
		}
		lastWritten = content
		if child != nil {
			if err := child.Signal(sighup); err != nil {
				log.Error(err, "SIGHUP to dnsmasq failed; allowlist applies at next restart", "pid", child.Pid)
				continue
			}
		}
		log.Info("dhcp-hostsfile updated", "path", path, "entries", countLines(content))
	}
}

// forwardLines copies each line of r into log. A read error other than
// EOF ends forwarding early and is itself logged - that pipe is the
// only channel dnsmasq's diagnostics reach the operator through.
func forwardLines(log logr.Logger, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Info(scanner.Text())
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
