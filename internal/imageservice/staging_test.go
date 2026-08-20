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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStaging(t *testing.T) (*Staging, string) {
	t.Helper()
	root := t.TempDir()
	staging, err := NewStaging(root)
	if err != nil {
		t.Fatalf("NewStaging: %v", err)
	}
	return staging, root
}

// ResolveUpload returns the on-disk path of a completed upload, and
// refuses both a never-uploaded name and a partial upload (data file
// present, completion marker not) - it must never hand the ingest job a
// partial file.
func TestStaging_ResolveUpload(t *testing.T) {
	staging, root := newTestStaging(t)

	body := []byte("golden image bytes")
	if _, err := staging.Receive("golden", bytes.NewReader(body), ""); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	path, err := staging.ResolveUpload("golden")
	if err != nil {
		t.Fatalf("ResolveUpload(completed): %v", err)
	}
	want := filepath.Join(root, "uploads", "golden", "upload.bin")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	if _, err := staging.ResolveUpload("never-uploaded"); err == nil {
		t.Error("ResolveUpload(never uploaded): got nil error, want non-nil")
	}

	// Data file present, no completion marker: the state a crash
	// mid-Receive could leave behind.
	partialDir := filepath.Join(root, "uploads", "partial")
	if err := os.MkdirAll(partialDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(partialDir, "upload.bin"), body, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := staging.ResolveUpload("partial"); err == nil {
		t.Error("ResolveUpload(partial upload, no completion marker): got nil error, want non-nil")
	}
}

// RemoveUpload is idempotent: removing an already-gone (or never
// existed) upload is not an error.
func TestStaging_RemoveUpload(t *testing.T) {
	staging, root := newTestStaging(t)

	body := []byte("golden image bytes")
	if _, err := staging.Receive("golden", bytes.NewReader(body), ""); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	if err := staging.RemoveUpload("golden"); err != nil {
		t.Fatalf("RemoveUpload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "uploads", "golden")); !os.IsNotExist(err) {
		t.Fatalf("upload directory still exists after RemoveUpload, stat err = %v", err)
	}

	if err := staging.RemoveUpload("golden"); err != nil {
		t.Errorf("RemoveUpload on already-removed upload: got %v, want nil", err)
	}
	if err := staging.RemoveUpload("never-existed"); err != nil {
		t.Errorf("RemoveUpload on never-existed upload: got %v, want nil", err)
	}
}

// NewStaging sweeps orphaned temp files left in uploads/.tmp/ by a prior
// process that crashed mid-Receive: since NewStaging runs before this
// process serves any upload, nothing can legitimately still be writing
// there, so every leftover entry is safe to remove on startup.
func TestNewStaging_SweepsOrphanedTempFiles(t *testing.T) {
	root := t.TempDir()
	tmpDir := filepath.Join(root, "uploads", ".tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "upload-orphaned"), []byte("crash-orphaned bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := NewStaging(root); err != nil {
		t.Fatalf("NewStaging: %v", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir(.tmp): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf(".tmp not swept on startup: %v", entries)
	}
}

// Receive's doc promises that a name is never silently repointed at
// different content: concurrent same-name uploads must either agree
// (idempotent) or be rejected with ErrNameConflict, never leave upload.bin
// holding one upload's bytes while upload.sha256 records another's
// checksum. Unserialized check/rename/writeMeta made that promise
// breakable under concurrency; this pins it.
func TestStaging_Receive_ConcurrentSameNameNeverCorrupts(t *testing.T) {
	staging, root := newTestStaging(t)

	const name = "race"
	const workers = 50

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]UploadResult, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := []byte(fmt.Sprintf("content-from-worker-%d", i))
			results[i], errs[i] = staging.Receive(name, bytes.NewReader(body), "")
		}(i)
	}
	close(start)
	wg.Wait()

	finalBytes, err := os.ReadFile(filepath.Join(root, "uploads", name, uploadFileName))
	if err != nil {
		t.Fatalf("read final upload.bin: %v", err)
	}
	sum := sha256.Sum256(finalBytes)
	wantChecksum := ChecksumAlgorithm + ":" + hex.EncodeToString(sum[:])

	meta, err := readMeta(filepath.Join(root, "uploads", name))
	if err != nil {
		t.Fatalf("read upload.sha256: %v", err)
	}
	if meta.Checksum != wantChecksum {
		t.Fatalf("upload.sha256 records %s, but upload.bin actually hashes to %s: name was silently repointed at mismatched content", meta.Checksum, wantChecksum)
	}

	// No caller may be told success (no error) for content that is not
	// what ended up stored: a dishonest success is exactly the silent
	// corruption Receive's doc rules out.
	for i, err := range errs {
		if err == nil && results[i].Checksum != wantChecksum {
			t.Fatalf("worker %d got a successful result with checksum %s, but the name is actually stored with checksum %s: dishonest success", i, results[i].Checksum, wantChecksum)
		}
	}
}

// Receive must never leave a temp file behind in uploads/.tmp, on any of
// its three reachable outcomes: a fresh successful upload (renamed away),
// an idempotent same-content retry, or a checksum-conflict rejection. A
// leaked temp file is invisible to UsedBytes, so leaking on a common path
// (a client retrying after a transient failure) silently eats staging
// capacity forever.
func TestStaging_Receive_NeverLeavesTempFile(t *testing.T) {
	staging, root := newTestStaging(t)
	tmpDir := filepath.Join(root, "uploads", ".tmp")

	assertTmpDirEmpty := func(t *testing.T, when string) {
		t.Helper()
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			t.Fatalf("ReadDir(.tmp) %s: %v", when, err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Errorf(".tmp not empty %s: %v", when, names)
		}
	}

	body := []byte("golden image bytes")
	if _, err := staging.Receive("golden", bytes.NewReader(body), ""); err != nil {
		t.Fatalf("Receive(first upload): %v", err)
	}
	assertTmpDirEmpty(t, "after first successful upload")

	result, err := staging.Receive("golden", bytes.NewReader(body), "")
	if err != nil {
		t.Fatalf("Receive(idempotent retry): %v", err)
	}
	if !result.Idempotent {
		t.Fatalf("Receive(idempotent retry): got Idempotent = false, want true")
	}
	assertTmpDirEmpty(t, "after idempotent same-content retry")

	if _, err := staging.Receive("golden", bytes.NewReader([]byte("different bytes")), ""); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("Receive(conflicting content): got err = %v, want ErrNameConflict", err)
	}
	assertTmpDirEmpty(t, "after checksum-conflict rejection")
}

// UsedBytes sums only completed uploads: an in-progress upload (under
// uploads/.tmp) must not be counted, since Admission's reservation
// ledger - not this scan - accounts for unfinished uploads.
func TestStaging_UsedBytes(t *testing.T) {
	staging, root := newTestStaging(t)

	if got, err := staging.UsedBytes(); err != nil || got != 0 {
		t.Fatalf("UsedBytes on empty staging: got (%d, %v), want (0, nil)", got, err)
	}

	first := []byte("golden image bytes")
	if _, err := staging.Receive("golden", bytes.NewReader(first), ""); err != nil {
		t.Fatalf("Receive(golden): %v", err)
	}
	second := []byte("a second, differently sized upload")
	if _, err := staging.Receive("other", bytes.NewReader(second), ""); err != nil {
		t.Fatalf("Receive(other): %v", err)
	}

	// A partial upload sitting under uploads/.tmp (simulating one still
	// in flight) must not be counted.
	if err := os.WriteFile(filepath.Join(root, "uploads", ".tmp", "in-progress"), []byte("partial-bytes-not-yet-complete"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := staging.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}
	want := int64(len(first) + len(second))
	if got != want {
		t.Errorf("UsedBytes = %d, want %d (in-progress upload must be excluded)", got, want)
	}
}
