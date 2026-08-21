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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tjjh89017/kezio/internal/bootd"
)

// writeEFIFixtures populates dir with the two allowlisted EFI binaries
// plus one file that must never be reachable through the route, so every
// test below shares one on-disk layout.
func writeEFIFixtures(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, bootd.ShimFilename), []byte("shim-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile shim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, bootd.GrubFilename), []byte("grub-image-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile grub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kernel"), []byte("not-efi"), 0o600); err != nil {
		t.Fatalf("WriteFile kernel: %v", err)
	}
}

func TestHandleEFI_ServesAllowlistedBinaries(t *testing.T) {
	dir := t.TempDir()
	writeEFIFixtures(t, dir)
	s, _ := newTestServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for name, want := range map[string]string{
		bootd.ShimFilename: "shim-bytes",
		bootd.GrubFilename: "grub-image-bytes",
	} {
		resp, err := http.Get(srv.URL + "/boot/http/" + name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", name, resp.StatusCode)
		}
		if string(body) != want {
			t.Fatalf("%s: body = %q, want %q", name, body, want)
		}
		if ct := resp.Header.Get("Content-Type"); ct != efiContentType {
			t.Fatalf("%s: Content-Type = %q, want %q", name, ct, efiContentType)
		}
		if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(want)) {
			t.Fatalf("%s: Content-Length = %q, want %q", name, cl, strconv.Itoa(len(want)))
		}
	}
}

func TestHandleEFI_RejectsNonAllowlistedAndTraversalNames(t *testing.T) {
	dir := t.TempDir()
	writeEFIFixtures(t, dir)
	// A file outside dir that a traversal attempt might try to reach.
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "passwd"), []byte("root:x:0:0"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	s, _ := newTestServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cases := []string{
		"kernel",              // a real file in dir, but not on the EFI allowlist
		"evil.efi",            // not on the allowlist at all
		"../etc/passwd",       // classic traversal shape
		"..%2Fetc%2Fpasswd",   // encoded traversal shape
		"shimx64.efi/../evil", // trailing traversal after an allowlisted prefix
	}
	for _, name := range cases {
		resp, err := http.Get(srv.URL + "/boot/http/" + name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%q unexpectedly succeeded with status 200", name)
		}
	}
}

func TestHandleEFI_SupportsRangeRequests(t *testing.T) {
	dir := t.TempDir()
	writeEFIFixtures(t, dir)
	s, _ := newTestServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/boot/http/"+bootd.ShimFilename, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Range", "bytes=0-3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if string(body) != "shim" {
		t.Fatalf("body = %q, want %q (first 4 bytes of shim-bytes)", body, "shim")
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-3/10" {
		t.Fatalf("Content-Range = %q, want %q", cr, "bytes 0-3/10")
	}
}
