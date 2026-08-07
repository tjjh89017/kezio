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

package bootserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// captureLog runs handler through logRequests against req and returns the
// single log line it produced (funcr's raw "key"=value args string, one
// call per request since the handler under test logs exactly once).
func captureLog(t *testing.T, handler http.Handler, req *http.Request) (rec *httptest.ResponseRecorder, logged string) {
	t.Helper()
	logger := funcr.New(func(prefix, args string) { logged = args }, funcr.Options{})
	req = req.WithContext(logf.IntoContext(req.Context(), logger))
	rec = httptest.NewRecorder()
	logRequests(handler).ServeHTTP(rec, req)
	return rec, logged
}

func TestLogRequests_SuccessfulRequestIsRecorded(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	req := httptest.NewRequest(http.MethodGet, "/boot/artifacts/vmlinuz", nil)
	req.RemoteAddr = "192.0.2.10:5000"

	rec, logged := captureLog(t, handler, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("logRequests altered the response: status=%d body=%q", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`"method"="GET"`,
		`"path"="/boot/artifacts/vmlinuz"`,
		`"status"=200`,
		`"bytes"=5`,
		`"remote"="192.0.2.10:5000"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("logged line missing %s; got: %s", want, logged)
		}
	}
}

func TestLogRequests_ImplicitOKStatusIsRecorded(t *testing.T) {
	// A handler that never calls WriteHeader (writeBootLocal-style
	// success without an explicit status) still gets an implicit 200
	// from net/http; the log line must reflect that, not a zero value.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-aa:bb:cc:dd:ee:ff", nil)
	_, logged := captureLog(t, handler, req)

	if !strings.Contains(logged, `"status"=200`) {
		t.Errorf("logged line missing implicit 200 status; got: %s", logged)
	}
}

func TestLogRequests_NotFoundStatusIsRecorded(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	req := httptest.NewRequest(http.MethodGet, "/boot/nope", nil)
	rec, logged := captureLog(t, handler, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if !strings.Contains(logged, `"status"=404`) {
		t.Errorf("logged line missing 404 status; got: %s", logged)
	}
}

func TestStatusRecorder_FirstWriteHeaderWins(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want the first WriteHeader call (200)", rec.status)
	}
}

func TestStatusRecorder_WriteCountsBytesAndDefaultsStatus(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 || rec.bytes != 5 {
		t.Errorf("bytes = %d (Write returned %d), want 5", rec.bytes, n)
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want implicit 200 after a Write with no prior WriteHeader", rec.status)
	}
}

func TestLogRequests_ServerHandlerLogsGrubConfigSuccess(t *testing.T) {
	// End-to-end through the real Server.Handler wiring, not just the
	// middleware in isolation: confirms handleGrubConfig's success path
	// (writeBootLocal, no anomaly of its own to log) now leaves a
	// record through the wrap in Handler().
	srv, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-aa:bb:cc:dd:ee:ff", nil)
	req.RemoteAddr = "198.51.100.7:9000"
	_, logged := captureLog(t, srv.Handler(), req)

	for _, want := range []string{`"method"="GET"`, `"path"="/boot/grub.cfg-aa:bb:cc:dd:ee:ff"`, `"status"=200`, `"remote"="198.51.100.7:9000"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("logged line missing %s; got: %s", want, logged)
		}
	}
}
