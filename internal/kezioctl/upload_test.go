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

package kezioctl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedResponse is a canned status/body pair a stub upload name answers
// with, instead of the success path.
type fixedResponse struct {
	status int
	body   string
}

// newUploadTestServer builds an httptest server that mimics
// internal/imageservice's PUT /uploads/{name} handler closely enough for
// Upload's tests: it checks the bearer token, echoes back the request
// body's size and a fixed checksum, and can be told to answer a fixed
// status/body pair for a given name so error-path tests don't need to
// replicate the real server's own logic.
func newUploadTestServer(t *testing.T, wantToken string, fixedErrors map[string]fixedResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected method %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		if fx, ok := fixedErrors[name]; ok {
			http.Error(w, fx.body, fx.status)
			return
		}

		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if r.ContentLength != int64(len(data)) {
			t.Errorf("Content-Length %d does not match body length %d", r.ContentLength, len(data))
		}

		resp := UploadResponse{
			Name:      name,
			URL:       "kezio-staged://" + name,
			SizeBytes: int64(len(data)),
			Checksum:  "sha256:deadbeef",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestUpload_Success(t *testing.T) {
	srv := newUploadTestServer(t, "secret-token", nil)
	defer srv.Close()

	content := []byte("golden image bytes")
	var progress bytes.Buffer
	got, err := Upload(srv.Client(), UploadOptions{
		ServerURL: srv.URL,
		Token:     "secret-token",
		Name:      "ubuntu-2404-golden",
		Progress:  &progress,
	}, bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	if got.Name != "ubuntu-2404-golden" {
		t.Errorf("Name = %q, want %q", got.Name, "ubuntu-2404-golden")
	}
	if got.URL != "kezio-staged://ubuntu-2404-golden" {
		t.Errorf("URL = %q, want kezio-staged://ubuntu-2404-golden", got.URL)
	}
	if got.Checksum != "sha256:deadbeef" {
		t.Errorf("Checksum = %q, want sha256:deadbeef", got.Checksum)
	}
	if got.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, len(content))
	}
	if progress.Len() == 0 {
		t.Error("expected progress output, got none")
	}
}

func TestUpload_TrimsTrailingSlashFromServerURL(t *testing.T) {
	srv := newUploadTestServer(t, "tok", nil)
	defer srv.Close()

	content := []byte("x")
	_, err := Upload(srv.Client(), UploadOptions{
		ServerURL: srv.URL + "/",
		Token:     "tok",
		Name:      "n",
	}, bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
}

func TestUpload_ErrorsSurfaceAsClearMessages(t *testing.T) {
	cases := []struct {
		name       string
		uploadName string
		token      string
		status     int
		body       string
		wantSubstr string
	}{
		{
			name:       "unauthorized",
			uploadName: "n",
			token:      "wrong-token",
			wantSubstr: "not authorized",
		},
		{
			name:       "conflict",
			uploadName: "conflict-name",
			token:      "tok",
			status:     http.StatusConflict,
			body:       "upload name already in use with different content",
			wantSubstr: "already in use with different content",
		},
		{
			name:       "checksum mismatch",
			uploadName: "bad-checksum",
			token:      "tok",
			status:     http.StatusUnprocessableEntity,
			body:       "checksum mismatch",
			wantSubstr: "checksum mismatch",
		},
		{
			name:       "too large",
			uploadName: "too-big",
			token:      "tok",
			status:     http.StatusRequestEntityTooLarge,
			body:       "upload exceeds the maximum allowed size",
			wantSubstr: "maximum upload size",
		},
		{
			name:       "length required",
			uploadName: "no-length",
			token:      "tok",
			status:     http.StatusLengthRequired,
			body:       "Content-Length header required",
			wantSubstr: "known content length",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixed := map[string]fixedResponse{}
			if tc.status != 0 {
				fixed[tc.uploadName] = fixedResponse{tc.status, tc.body}
			}
			srv := newUploadTestServer(t, "tok", fixed)
			defer srv.Close()

			content := []byte("data")
			_, err := Upload(srv.Client(), UploadOptions{
				ServerURL: srv.URL,
				Token:     tc.token,
				Name:      tc.uploadName,
			}, bytes.NewReader(content), int64(len(content)))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestUpload_RequiresServerURLAndName(t *testing.T) {
	if _, err := Upload(nil, UploadOptions{Name: "n"}, bytes.NewReader(nil), 0); err == nil {
		t.Error("expected an error for empty ServerURL")
	}
	if _, err := Upload(nil, UploadOptions{ServerURL: "http://example.invalid"}, bytes.NewReader(nil), 0); err == nil {
		t.Error("expected an error for empty Name")
	}
}
