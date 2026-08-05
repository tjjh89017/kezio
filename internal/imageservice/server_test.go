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

package imageservice

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "s3cr3t-test-token" //nolint:gosec // test fixture, not a real credential

func newTestServer(t *testing.T, maxUploadBytes int64) (*Server, string) {
	t.Helper()
	return newTestServerWithAdmission(t, maxUploadBytes, nil)
}

// newTestServerWithAdmission is like newTestServer but lets a test supply
// its own Admission (for example, one built with a fake statfsFunc) to
// exercise the capacity-guard path in handleUpload. admission == nil
// disables both capacity guards, matching newTestServer.
func newTestServerWithAdmission(t *testing.T, maxUploadBytes int64, admission *Admission) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	staging, err := NewStaging(root)
	if err != nil {
		t.Fatalf("NewStaging: %v", err)
	}
	auth, err := NewAuthenticator(testToken)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return NewServer(staging, auth, maxUploadBytes, admission, nil), root
}

func putUpload(t *testing.T, h http.Handler, name string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/uploads/"+name, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func authHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

// Auth reject: an upload with no bearer token, and one with the wrong
// token, are both rejected before anything is written to the staging
// area.
func TestHandleUpload_AuthReject(t *testing.T) {
	srv, root := newTestServer(t, 1<<20)
	h := srv.Handler()

	body := []byte("payload")

	rec := putUpload(t, h, "golden", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no Authorization header: got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	rec = putUpload(t, h, "golden", body, map[string]string{"Authorization": "Bearer wrong-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	if _, err := os.Stat(filepath.Join(root, "uploads", "golden")); !os.IsNotExist(err) {
		t.Fatalf("rejected upload must not be written to the staging area, stat err = %v", err)
	}
}

// Happy path: an authenticated streamed upload lands under the staging
// root with the expected bytes, and the response reports the staged
// reference the client uses as Image.spec.source.url.
func TestHandleUpload_HappyPath(t *testing.T) {
	srv, root := newTestServer(t, 1<<20)
	h := srv.Handler()

	body := []byte("golden image bytes")
	rec := putUpload(t, h, "ubuntu-2404-golden", body, authHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d, body %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.URL != "kezio-staged://ubuntu-2404-golden" {
		t.Errorf("URL = %q, want kezio-staged://ubuntu-2404-golden", resp.URL)
	}
	if resp.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d", resp.SizeBytes, len(body))
	}
	if resp.Checksum != sha256Hex(body) {
		t.Errorf("Checksum = %q, want %q", resp.Checksum, sha256Hex(body))
	}
	if resp.Idempotent {
		t.Error("first upload must not be reported as idempotent")
	}

	stored, err := os.ReadFile(filepath.Join(root, "uploads", "ubuntu-2404-golden", "upload.bin"))
	if err != nil {
		t.Fatalf("read stored upload: %v", err)
	}
	if !bytes.Equal(stored, body) {
		t.Errorf("stored bytes = %q, want %q", stored, body)
	}

	// Re-uploading the same name with the same content is an idempotent
	// success, not a conflict, and does not require the client to also
	// pass the checksum.
	rec = putUpload(t, h, "ubuntu-2404-golden", body, authHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent re-upload: got status %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode idempotent response: %v", err)
	}
	if !resp.Idempotent {
		t.Error("re-upload with identical content must be reported as idempotent")
	}
}

// A client-asserted checksum that does not match the bytes actually
// streamed is rejected, and nothing is left behind under the staged name.
func TestHandleUpload_ChecksumMismatch(t *testing.T) {
	srv, root := newTestServer(t, 1<<20)
	h := srv.Handler()

	body := []byte("golden image bytes")
	headers := authHeader()
	headers[ChecksumHeader] = "sha256:" + strings.Repeat("0", 64)

	rec := putUpload(t, h, "golden", body, headers)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d, body %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(root, "uploads", "golden")); !os.IsNotExist(err) {
		t.Fatalf("checksum-mismatched upload must not leave a directory behind, stat err = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "uploads", ".tmp"))
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("partial-upload cleanup left %d entries in .tmp: %v", len(entries), entries)
	}
}

// Reusing a name with different content (no checksum asserted, so the
// mismatch is only caught by comparing against what is already stored) is
// a conflict, not a silent overwrite.
func TestHandleUpload_NameConflictDifferentContent(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	rec := putUpload(t, h, "golden", []byte("version one"), authHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("first upload: got status %d, want %d", rec.Code, http.StatusCreated)
	}

	rec = putUpload(t, h, "golden", []byte("version two, different bytes"), authHeader())
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting re-upload: got status %d, want %d, body %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

// A name that attempts path traversal is rejected before any filesystem
// path is built from it, and the request never reaches the staging area.
func TestHandleUpload_PathTraversal(t *testing.T) {
	srv, root := newTestServer(t, 1<<20)
	h := srv.Handler()

	for _, name := range []string{
		"..%2f..%2fetc%2fpasswd",
		"..",
		"a%2fb",
		"a/b",
	} {
		req := httptest.NewRequest(http.MethodPut, "/uploads/"+name, bytes.NewReader([]byte("x")))
		req.ContentLength = 1
		for k, v := range authHeader() {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// The mux either cleans/redirects a path with a literal ".."
		// segment before routing (301/307), fails to route a
		// path-separator-bearing name to the {name} handler at all
		// (404), or the handler itself rejects the name (400). All three
		// keep the request from ever reaching Staging with an
		// unvalidated name.
		switch rec.Code {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect:
		default:
			t.Errorf("name %q: got status %d, want 400, 404, 301, or 307, body %s", name, rec.Code, rec.Body.String())
		}
	}

	if _, err := os.Stat(filepath.Join(root, "uploads", "etc")); !os.IsNotExist(err) {
		t.Fatalf("path traversal must not escape the uploads directory, stat err = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".tmp" {
			t.Errorf("uploads dir has unexpected entry %q from a rejected traversal attempt", e.Name())
		}
	}
}

// An upload whose declared Content-Length exceeds the configured limit is
// rejected without reading the body, and one that lies about its length
// (a shorter Content-Length than the bytes actually sent) is still capped
// by the second, independent MaxBytesReader layer.
func TestHandleUpload_Oversize(t *testing.T) {
	srv, _ := newTestServer(t, 8)
	h := srv.Handler()

	rec := putUpload(t, h, "golden", bytes.Repeat([]byte("x"), 100), authHeader())
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize Content-Length: got status %d, want %d, body %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// A client that understates its Content-Length (so the header passes the
// early check) but then streams more bytes than it declared is still
// capped, by the independent http.MaxBytesReader layer.
func TestHandleUpload_OversizeLiedContentLength(t *testing.T) {
	srv, _ := newTestServer(t, 8)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPut, "/uploads/golden", bytes.NewReader(bytes.Repeat([]byte("x"), 100)))
	req.ContentLength = 5 // understated: passes the <= max precheck, real body does not
	for k, v := range authHeader() {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want %d, body %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

func TestHandleUpload_MissingContentLength(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPut, "/uploads/golden", bytes.NewReader([]byte("x")))
	req.ContentLength = -1
	for k, v := range authHeader() {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusLengthRequired {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusLengthRequired)
	}
}

// An upload whose declared Content-Length would not fit the staging
// volume's available space is rejected with 507 Insufficient Storage,
// before any body byte is read, and nothing is written to the staging
// area.
func TestHandleUpload_InsufficientStagingSpace(t *testing.T) {
	admission := NewAdmission("unused", noUsedBytes, 0)
	admission.statfs = fakeStatfs(4) // far too little for the body below, even with no headroom

	srv, root := newTestServerWithAdmission(t, 1<<20, admission)
	h := srv.Handler()

	body := bytes.Repeat([]byte("x"), 100)
	rec := putUpload(t, h, "golden", body, authHeader())
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("got status %d, want %d, body %s", rec.Code, http.StatusInsufficientStorage, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(root, "uploads", "golden")); !os.IsNotExist(err) {
		t.Fatalf("rejected-for-space upload must not be written to the staging area, stat err = %v", err)
	}
}

// A configured logical quota is enforced the same way: an upload that
// would push staged usage over the quota is rejected with 507, even
// though physical space is plentiful.
func TestHandleUpload_QuotaExceeded(t *testing.T) {
	usedBytes := func() (int64, error) { return 90, nil }
	admission := NewAdmission("unused", usedBytes, 100) // 90 already used, quota 100
	admission.statfs = fakeStatfs(1 << 40)

	srv, _ := newTestServerWithAdmission(t, 1<<20, admission)
	h := srv.Handler()

	rec := putUpload(t, h, "golden", bytes.Repeat([]byte("x"), 20), authHeader()) // 90+20 > 100
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("got status %d, want %d, body %s", rec.Code, http.StatusInsufficientStorage, rec.Body.String())
	}
}

// Concurrent uploads that would each individually fit but jointly exceed
// available space are caught: the reservation each admitted upload holds
// while its body streams is what a plain per-request statfs check would
// miss (see TestAdmission_Reserve_ConcurrentAdmissionNeverOverbooks for
// the ledger-only version of this scenario). Here the second upload is
// only actually admitted once the first has released its reservation by
// completing, since httptest.NewRequest'd bodies stream fully before this
// handler returns; the ledger is still what makes that release
// deterministic and race-free.
func TestHandleUpload_ConcurrentUploadsRespectSharedReservation(t *testing.T) {
	admission := NewAdmission("unused", noUsedBytes, 0)
	// Exactly enough for one of the two uploads below, plus headroom -
	// not enough for both at once.
	admission.statfs = fakeStatfs(10<<20 + StagingSpaceHeadroom)

	srv, _ := newTestServerWithAdmission(t, 100<<20, admission)
	h := srv.Handler()

	first := bytes.Repeat([]byte("a"), 10<<20)
	rec := putUpload(t, h, "first", first, authHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("first upload: got status %d, want %d, body %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// The first upload's request has already completed (and released its
	// reservation) by the time this second call runs, so it succeeds too
	// - demonstrating the reservation was correctly released rather than
	// permanently exhausting the volume's reported capacity.
	second := bytes.Repeat([]byte("b"), 10<<20)
	rec = putUpload(t, h, "second", second, authHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("second upload after first released: got status %d, want %d, body %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestHandleHealthz_Unauthenticated(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}
}
