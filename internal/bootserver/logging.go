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

package bootserver

import (
	"net/http"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// statusRecorder wraps an http.ResponseWriter to capture the status code
// and byte count a handler actually wrote - neither is readable back off
// the standard interface once the handler returns.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	// Like the real ResponseWriter, only the first call sets the status;
	// a later call (redirect-after-write, a Flush-triggered implicit
	// 200, etc.) must not overwrite what already went on the wire.
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// logRequests wraps next so every request - success or anomaly - leaves
// one record, closing the gap left by routes with no anomaly logging of
// their own (the artifacts and EFI file servers). Always on, not behind a
// flag: this server's request volume (a handful of enrolled machines mid
// boot) makes the per-request cost negligible next to a boot that failed
// silently.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		logf.FromContext(r.Context()).WithName("bootserver").Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"remote", r.RemoteAddr,
			"duration", time.Since(start).String(),
		)
	})
}
