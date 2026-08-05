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
	"os"
	"path/filepath"
	"strings"
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

const longRunningScript = `trap 'exit 0' TERM INT
echo fake-dnsmasq-started
while :; do sleep 0.05; done
`

func TestDnsmasq_WritesConfigAndInitialHostsfile(t *testing.T) {
	d, runDir := newTestDnsmasq(t, longRunningScript)

	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logf.Log))
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	waitFor(t, "config file", func() bool {
		_, err := os.Stat(filepath.Join(runDir, "dnsmasq.conf"))
		return err == nil
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
