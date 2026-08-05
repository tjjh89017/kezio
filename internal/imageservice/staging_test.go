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
	"os"
	"path/filepath"
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

// ResolveUpload returns the on-disk path of a completed upload, and refuses
// both a name that was never uploaded and one that only partially uploaded
// (its data file is present but its completion marker is not) — a broken
// ResolveUpload here could hand the ingest job a partial file.
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

	// A partial upload: the data file exists but the completion marker
	// (upload.sha256) does not, exactly the state a crash mid-Receive
	// could leave behind if it bypassed the temp-file-then-rename path.
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

// RemoveUpload deletes a completed upload's directory, and is idempotent:
// removing an upload that is already gone (or never existed) is not an
// error, which matters if a retry runs this step twice.
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

// UsedBytes sums only completed uploads: an in-progress upload (a temp
// file under uploads/.tmp) must not be counted, since Admission's
// in-flight reservation ledger - not a disk scan - is what accounts for
// uploads that have not finished yet.
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
