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

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingLogf collects every message logged through it, safe for
// concurrent use by the forwarding and reaper goroutines spawnEzio
// starts.
type recordingLogf struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recordingLogf) log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recordingLogf) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	copy(out, r.msgs)
	return out
}

// writeFakeEzio writes an executable shell script standing in for the
// ezio binary, so spawnEzio's piping and reaping behavior is testable
// without a real ezio daemon or a gRPC handshake (spawnEzio itself never
// dials - see its doc comment).
func writeFakeEzio(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ezio")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitFor polls cond until it reports true or five seconds pass.
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

func containsMsg(msgs []string, substr string) bool {
	for _, m := range msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

func TestSpawnEzio_ForwardsStdoutAndStderrAttributedToEzio(t *testing.T) {
	rec := &recordingLogf{}
	l := ezioLauncher{
		Logf:   rec.log,
		Binary: writeFakeEzio(t, "echo out-line\necho err-line 1>&2\n"),
	}

	stop, err := l.spawnEzio(nil)
	if err != nil {
		t.Fatalf("spawnEzio: %v", err)
	}
	defer func() { _ = stop() }()

	waitFor(t, "forwarded stdout and stderr", func() bool {
		msgs := rec.messages()
		return containsMsg(msgs, "out-line") && containsMsg(msgs, "err-line")
	})

	msgs := rec.messages()
	if !containsMsg(msgs, "ezio stdout: out-line") {
		t.Errorf("stdout line not attributed to ezio: %v", msgs)
	}
	if !containsMsg(msgs, "ezio stderr: err-line") {
		t.Errorf("stderr line not attributed to ezio: %v", msgs)
	}

	waitFor(t, "clean exit logged", func() bool {
		return containsMsg(rec.messages(), "local ezio daemon exited cleanly")
	})
}

func TestSpawnEzio_CrashDoesNotLoseLastLines(t *testing.T) {
	rec := &recordingLogf{}
	l := ezioLauncher{
		Logf: rec.log,
		Binary: writeFakeEzio(t, `echo last-stdout-line
echo last-stderr-line 1>&2
exit 7
`),
	}

	stop, err := l.spawnEzio(nil)
	if err != nil {
		t.Fatalf("spawnEzio: %v", err)
	}
	defer func() { _ = stop() }()

	waitFor(t, "exit outcome logged", func() bool {
		return containsMsg(rec.messages(), "local ezio daemon exited:")
	})

	msgs := rec.messages()
	if !containsMsg(msgs, "last-stdout-line") {
		t.Errorf("crash lost the last stdout line: %v", msgs)
	}
	if !containsMsg(msgs, "last-stderr-line") {
		t.Errorf("crash lost the last stderr line: %v", msgs)
	}
	if !containsMsg(msgs, "exit status 7") {
		t.Errorf("exit outcome does not name the exit status: %v", msgs)
	}
}

func TestSpawnEzio_KillDoesNotLoseLinesWrittenBeforeIt(t *testing.T) {
	rec := &recordingLogf{}
	l := ezioLauncher{
		Logf: rec.log,
		Binary: writeFakeEzio(t, `echo before-kill
trap 'exit 0' TERM
while :; do sleep 0.05; done
`),
	}

	stop, err := l.spawnEzio(nil)
	if err != nil {
		t.Fatalf("spawnEzio: %v", err)
	}

	waitFor(t, "line written before the kill", func() bool {
		return containsMsg(rec.messages(), "before-kill")
	})

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	waitFor(t, "exit outcome logged after kill", func() bool {
		return containsMsg(rec.messages(), "local ezio daemon exited")
	})
	if !containsMsg(rec.messages(), "before-kill") {
		t.Errorf("killing the process lost a line written before the kill: %v", rec.messages())
	}
}

func TestForwardLines_LogsScannerErrorButKeepsLinesReadBeforeIt(t *testing.T) {
	rec := &recordingLogf{}
	l := ezioLauncher{Logf: rec.log}
	boom := errors.New("boom")

	l.forwardLines("ezio stdout", &erroringReader{lines: []string{"hello\n"}, err: boom})

	msgs := rec.messages()
	if !containsMsg(msgs, "ezio stdout: hello") {
		t.Errorf("forwardLines dropped the line read before the failure: %v", msgs)
	}
	if !containsMsg(msgs, "reading ezio stdout failed") {
		t.Errorf("forwardLines did not log the scanner error: %v", msgs)
	}
}

// erroringReader yields lines then a non-EOF error, standing in for an
// ezio stdout/stderr pipe that fails mid-read rather than closing
// cleanly.
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
