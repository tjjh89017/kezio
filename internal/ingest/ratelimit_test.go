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

package ingest

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestNewThrottledWriter_DisabledReturnsUnderlyingWriter(t *testing.T) {
	var buf bytes.Buffer
	got := NewThrottledWriter(&buf, 0)
	if got != io.Writer(&buf) {
		t.Error("expected NewThrottledWriter(w, 0) to return w unchanged")
	}

	got = NewThrottledWriter(&buf, -1)
	if got != io.Writer(&buf) {
		t.Error("expected NewThrottledWriter(w, negative) to return w unchanged")
	}
}

// TestThrottledWriter_LimitsRate writes enough data, at a low enough
// configured rate, that finishing faster than the rate allows would mean
// the limiter did nothing. It only asserts a lower bound on elapsed time,
// never an upper one, so it cannot flake under a slow CI runner.
func TestThrottledWriter_LimitsRate(t *testing.T) {
	var buf bytes.Buffer
	const bytesPerSec = 10_000
	const payloadLen = 4_000 // ~0.4s of budget at bytesPerSec

	w := NewThrottledWriter(&buf, bytesPerSec)
	start := time.Now()
	if _, err := w.Write(make([]byte, payloadLen)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("elapsed = %v, want at least ~0.4s for %d bytes at %d B/s", elapsed, payloadLen, bytesPerSec)
	}
	if buf.Len() != payloadLen {
		t.Errorf("buf.Len() = %d, want %d: throttling must not drop or duplicate bytes", buf.Len(), payloadLen)
	}
}
