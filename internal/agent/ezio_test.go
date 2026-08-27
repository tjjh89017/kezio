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

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
)

// captureLogf returns a Logf-compatible function that records every
// formatted line, and a way to read back everything recorded so far
// without racing the copier goroutines that call it.
func captureLogf() (logf func(format string, args ...any), lines func() []string) {
	var mu sync.Mutex
	var got []string
	logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf(format, args...))
	}
	lines = func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(got))
		copy(out, got)
		return out
	}
	return logf, lines
}

func writeFakeBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ezio")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

func TestStartWithLoggedOutput_StreamsStdoutAndStderr(t *testing.T) {
	path := writeFakeBinary(t, "#!/bin/sh\n"+
		"echo out-line-1\n"+
		"echo err-line-1 >&2\n"+
		"echo out-line-2\n"+
		"sleep 0.2\n")

	logf, lines := captureLogf()
	cmd := exec.Command(path)
	stop, err := startWithLoggedOutput(cmd, logf)
	if err != nil {
		t.Fatalf("startWithLoggedOutput: %v", err)
	}
	defer func() { _ = stop() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := lines()
		if containsAll(got, "ezio: out-line-1", "ezio: err-line-1", "ezio: out-line-2") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe expected prefixed lines, got: %v", lines())
}

func TestStartWithLoggedOutput_StopReapsProcess(t *testing.T) {
	path := writeFakeBinary(t, "#!/bin/sh\n"+
		"echo hello\n"+
		"sleep 30\n")

	logf, lines := captureLogf()
	cmd := exec.Command(path)
	stop, err := startWithLoggedOutput(cmd, logf)
	if err != nil {
		t.Fatalf("startWithLoggedOutput: %v", err)
	}

	if err := stop(); err != nil {
		// Kill on an already-signaled process can return an error on
		// some platforms; what matters is that stop() did not hang and
		// the process is actually gone (checked below).
		t.Logf("stop: %v", err)
	}

	if cmd.ProcessState == nil {
		t.Fatal("stop returned before the process was reaped (cmd.ProcessState is nil)")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || (!status.Exited() && !status.Signaled()) {
		t.Fatalf("process state = %v, want exited or signaled", cmd.ProcessState)
	}

	found := false
	for _, l := range lines() {
		if strings.HasPrefix(l, "ezio: exited:") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an 'ezio: exited: ...' log line, got: %v", lines())
	}
}

func containsAll(lines []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, l := range lines {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func int32ptr(v int32) *int32 { return &v }

func writeFakeMeminfo(t *testing.T, memTotalKB uint64) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	content := fmt.Sprintf("MemTotal:       %d kB\nMemFree:        1024 kB\n", memTotalKB)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fake meminfo: %v", err)
	}
	return path
}

func TestAutoCacheSizeMB(t *testing.T) {
	const gib = uint64(1) << 30
	cases := []struct {
		name          string
		memTotalBytes uint64
		want          int32
	}{
		{"2GiB", 2 * gib, 64},
		{"4GiB", 4 * gib, 1024},
		{"8GiB", 8 * gib, 3072},
		{"16GiB", 16 * gib, 7168},
		{"64GiB", 64 * gib, 8192},
		{"4GiB plus 1000kB rounds down", 4*gib + 1000*1024, 1024},
		{"below reserve clamps to minimum", gib, 64},
		{"zero clamps to minimum", 0, 64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AutoCacheSizeMB(c.memTotalBytes); got != c.want {
				t.Errorf("AutoCacheSizeMB(%d) = %d, want %d", c.memTotalBytes, got, c.want)
			}
		})
	}
}

// TestLaunch_PassesOverrideFlags exercises Launch's flag-building end to
// end against a fake binary that echoes its own argv, asserting
// --cache-size/--aio-threads/--port are passed verbatim when the launch
// config sets them, and that an explicit CacheSizeMB is logged as an
// override rather than computed automatically.
func TestLaunch_PassesOverrideFlags(t *testing.T) {
	path := writeFakeBinary(t, "#!/bin/sh\n"+
		"echo args: \"$@\"\n"+
		"sleep 1\n")
	logf, lines := captureLogf()

	l := ExecEzioLauncher{
		Binary:       path,
		ReadyTimeout: 150 * time.Millisecond,
		Logf:         logf,
	}
	cfg := deploy.EzioLaunchConfig{
		CacheSizeMB: int32ptr(777),
		AioThreads:  int32ptr(4),
		Port:        int32ptr(6890),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handle, err := l.Launch(ctx, cfg)
	if err == nil {
		// The fake binary is not a real ezio daemon, so Launch always
		// fails its health check; Launch has already stopped the process
		// by the time it returns an error, so Stop here only guards
		// against a future change making Launch succeed unexpectedly.
		_ = handle.Stop()
	}

	want := "ezio: args: --listen " + DefaultEzioListenAddress + " --cache-size 777 --aio-threads 4 --port 6890"
	if !containsAll(lines(), want) {
		t.Fatalf("exec args line %q not found, got: %v", want, lines())
	}
	if !containsAll(lines(), "ezio: cache size 777 MB (override)") {
		t.Errorf("expected an override cache-size log line, got: %v", lines())
	}
}

// TestLaunch_AutoCacheSizeFromMeminfo asserts that an unset CacheSizeMB
// makes Launch compute one from MeminfoPath and pass it as --cache-size,
// while AioThreads/Port stay omitted since the launch config leaves them
// unset.
func TestLaunch_AutoCacheSizeFromMeminfo(t *testing.T) {
	path := writeFakeBinary(t, "#!/bin/sh\n"+
		"echo args: \"$@\"\n"+
		"sleep 1\n")
	meminfoPath := writeFakeMeminfo(t, 4*1024*1024) // 4 GiB
	logf, lines := captureLogf()

	l := ExecEzioLauncher{
		Binary:       path,
		ReadyTimeout: 150 * time.Millisecond,
		MeminfoPath:  meminfoPath,
		Logf:         logf,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handle, err := l.Launch(ctx, deploy.EzioLaunchConfig{})
	if err == nil {
		_ = handle.Stop()
	}

	want := "ezio: args: --listen " + DefaultEzioListenAddress + " --cache-size 1024"
	if !containsAll(lines(), want) {
		t.Fatalf("exec args line %q not found, got: %v", want, lines())
	}
	if !containsAll(lines(), "ezio: cache size 1024 MB (auto, MemTotal 4294967296 bytes)") {
		t.Errorf("expected an auto cache-size log line, got: %v", lines())
	}
}

// TestLaunch_NoMeminfoOmitsCacheFlag asserts that an unreadable
// MeminfoPath falls back to omitting --cache-size entirely (ezio's own
// built-in default) rather than failing the launch outright.
func TestLaunch_NoMeminfoOmitsCacheFlag(t *testing.T) {
	path := writeFakeBinary(t, "#!/bin/sh\n"+
		"echo args: \"$@\"\n"+
		"sleep 1\n")
	logf, lines := captureLogf()

	l := ExecEzioLauncher{
		Binary:       path,
		ReadyTimeout: 150 * time.Millisecond,
		MeminfoPath:  filepath.Join(t.TempDir(), "does-not-exist"),
		Logf:         logf,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handle, err := l.Launch(ctx, deploy.EzioLaunchConfig{})
	if err == nil {
		_ = handle.Stop()
	}

	want := "ezio: args: --listen " + DefaultEzioListenAddress
	if !containsAll(lines(), want) {
		t.Fatalf("exec args line %q not found, got: %v", want, lines())
	}
	for _, l := range lines() {
		if strings.Contains(l, "args:") && strings.Contains(l, "--cache-size") {
			t.Errorf("expected no --cache-size flag, got args line: %q", l)
		}
	}
}
