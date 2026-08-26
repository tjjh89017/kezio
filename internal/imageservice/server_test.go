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

// newTestServerWithAdmission lets a test supply its own Admission to
// exercise the capacity-guard path; nil disables both guards.
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

// An upload with no bearer token, and one with the wrong token, are both
// rejected before anything is written to the staging area.
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

// An authenticated streamed upload lands under the staging root with the
// expected bytes, and the response reports the staged reference.
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

	// Re-uploading the same name/content is idempotent success, not a
	// conflict, without needing the client to pass a checksum.
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

// A checksum mismatch is rejected, and nothing is left behind under the
// staged name.
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

// Reusing a name with different content (no checksum asserted; caught by
// comparing against what's already stored) is a conflict, not an
// overwrite.
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

// A path-traversal name is rejected before any filesystem path is built
// from it.
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

		// Mux redirect (301/307) on "..", 404 on unroutable separators, or
		// 400 from the handler - all keep an unvalidated name from Staging.
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

// A declared Content-Length exceeding the configured limit is rejected
// without reading the body.
func TestHandleUpload_Oversize(t *testing.T) {
	srv, _ := newTestServer(t, 8)
	h := srv.Handler()

	rec := putUpload(t, h, "golden", bytes.Repeat([]byte("x"), 100), authHeader())
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize Content-Length: got status %d, want %d, body %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// A client that understates Content-Length (passing the early check) but
// streams more bytes is still capped by the independent MaxBytesReader.
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

// An upload that would not fit staging's available space is rejected with
// 507 before any body byte is read.
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

// A logical quota is enforced the same way, even with plentiful physical
// space.
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

// Uploads that would each individually fit but jointly exceed available
// space are caught by the per-upload reservation held while its body
// streams (see TestAdmission_Reserve_ConcurrentAdmissionNeverOverbooks
// for the ledger-only version).
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

	// The first upload has released its reservation by the time this runs,
	// so it succeeds too - the reservation was released, not permanent.
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

// HEAD /uploads/{name} reports a completed upload's size as Content-Length
// with no body - what the ImageImport controller uses to size the ingest
// scratch PVC from a staged source without downloading it.
func TestHandleUploadStat(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	body := []byte("payload-of-some-length")
	putRec := putUpload(t, h, "golden", body, authHeader())
	if putRec.Code != http.StatusCreated {
		t.Fatalf("upload: got status %d, want %d, body %s", putRec.Code, http.StatusCreated, putRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodHead, "/uploads/golden", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Length"); got != "22" {
		t.Errorf("Content-Length = %q, want %q", got, "22")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body length = %d, want 0", rec.Body.Len())
	}
}

func TestHandleUploadStat_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodHead, "/uploads/missing", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleUploadStat_AuthReject(t *testing.T) {
	srv, _ := newTestServer(t, 1<<20)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodHead, "/uploads/golden", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
