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
	"io"
	"time"
)

// throttledWriter wraps an io.Writer, sleeping after each Write so the
// writer's cumulative rate never runs ahead of bytesPerSec. It is not a
// strict traffic shaper - one Write still lands in full before any
// sleep - but that is enough to keep a large sequential copy (the
// download and the per-partition slice extraction, the only file writes
// this package performs directly rather than through an external tool)
// from saturating a shared disk the way an unthrottled one can.
type throttledWriter struct {
	w           io.Writer
	bytesPerSec int64
	written     int64
	start       time.Time
}

// NewThrottledWriter wraps w to cap its write rate at bytesPerSec bytes
// per second. bytesPerSec <= 0 returns w unchanged: throttling is off.
func NewThrottledWriter(w io.Writer, bytesPerSec int64) io.Writer {
	if bytesPerSec <= 0 {
		return w
	}
	return &throttledWriter{w: w, bytesPerSec: bytesPerSec, start: time.Now()}
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n <= 0 {
		return n, err
	}

	t.written += int64(n)
	wantElapsed := time.Duration(float64(t.written) / float64(t.bytesPerSec) * float64(time.Second))
	if actualElapsed := time.Since(t.start); wantElapsed > actualElapsed {
		time.Sleep(wantElapsed - actualElapsed)
	}
	return n, err
}
