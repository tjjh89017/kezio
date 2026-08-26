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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
