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
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSource_HTTP(t *testing.T) {
	dl := &fakeDownloader{content: []byte("body")}
	dest := filepath.Join(t.TempDir(), "source.img")

	path, staged, err := ResolveSource(context.Background(), "https://example.com/golden.qcow2", dl, nil, dest)
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if staged {
		t.Error("http source should not be reported as staged")
	}
	if path != dest {
		t.Errorf("path = %q, want %q", path, dest)
	}
	if len(dl.calls) != 1 || dl.calls[0] != "https://example.com/golden.qcow2" {
		t.Errorf("downloader.calls = %v", dl.calls)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "body" {
		t.Errorf("downloaded content = %q, err %v", got, err)
	}
}

func TestResolveSource_Staged(t *testing.T) {
	stagedPath := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(stagedPath, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := &fakeStaging{paths: map[string]string{"golden": stagedPath}}

	path, staged, err := ResolveSource(context.Background(), "kezio-staged://golden", nil, staging, "unused-dest")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if !staged {
		t.Error("kezio-staged source should be reported as staged")
	}
	if path != stagedPath {
		t.Errorf("path = %q, want %q", path, stagedPath)
	}
}

func TestResolveSource_StagedWithoutStaging(t *testing.T) {
	// Staging is optional on Dependencies (wired only when
	// STAGING_ROOT/INGEST_STAGING_PVC is configured - see cmd/ingest's
	// buildFromEnv). A kezio-staged:// source with no staging resolver
	// wired must fail with an actionable error, not dereference the nil
	// interface.
	_, _, err := ResolveSource(context.Background(), "kezio-staged://golden", nil, nil, "unused-dest")
	if err == nil {
		t.Fatal("expected an error when staging is nil for a kezio-staged:// source")
	}
}

func TestResolveSource_UnsupportedScheme(t *testing.T) {
	if _, _, err := ResolveSource(context.Background(), "ftp://example.com/x", nil, nil, "dest"); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
}

func TestStagedNameFromURL(t *testing.T) {
	if got := StagedNameFromURL("kezio-staged://golden"); got != "golden" {
		t.Errorf("StagedNameFromURL = %q, want golden", got)
	}
}
