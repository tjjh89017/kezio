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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

func TestBuildFromEnv_Valid(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	t.Setenv("SOURCE_URL", "https://example.com/image.qcow2")
	t.Setenv("SOURCE_FORMAT", "qcow2")
	t.Setenv("SOURCE_CHECKSUM", "sha256:deadbeef")
	t.Setenv("WORK_DIR", workDir)
	t.Setenv("STAGING_ROOT", "")

	cfg, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}

	if cfg.SourceURL != "https://example.com/image.qcow2" {
		t.Errorf("SourceURL = %q, want the configured URL", cfg.SourceURL)
	}
	if cfg.SourceFormat != "qcow2" {
		t.Errorf("SourceFormat = %q, want qcow2", cfg.SourceFormat)
	}
	if cfg.SourceChecksum != "sha256:deadbeef" {
		t.Errorf("SourceChecksum = %q, want sha256:deadbeef", cfg.SourceChecksum)
	}
	if cfg.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want %q", cfg.WorkDir, workDir)
	}
	if info, statErr := os.Stat(workDir); statErr != nil || !info.IsDir() {
		t.Errorf("buildFromEnv did not create WorkDir %s: %v", workDir, statErr)
	}

	if deps.Downloader == nil || deps.QemuImg == nil || deps.Sfdisk == nil || deps.Blkid == nil || deps.Partclone == nil {
		t.Errorf("buildFromEnv left a required dependency nil: %+v", deps)
	}
	if deps.Staging != nil {
		t.Errorf("deps.Staging = %v, want nil when STAGING_ROOT is unset", deps.Staging)
	}
}

func TestBuildFromEnv_MissingRequiredEnv(t *testing.T) {
	t.Setenv("SOURCE_URL", "")
	t.Setenv("SOURCE_FORMAT", "")

	if _, _, err := buildFromEnv(); err == nil {
		t.Fatal("buildFromEnv: want error when SOURCE_URL/SOURCE_FORMAT are unset, got nil")
	}
}

func TestPublishConfigFromEnv_Valid(t *testing.T) {
	hash := store.InfoHash{0x01, 0x02, 0x03}
	t.Setenv("TRACKER_URL", "http://tracker.example.com/announce")
	t.Setenv("PARTITION_CONTENT_HASH", hash.String())
	t.Setenv("SOURCE_CONTENT_DIR", "/work/partitions/1")

	cfg, err := publishConfigFromEnv()
	if err != nil {
		t.Fatalf("publishConfigFromEnv: %v", err)
	}

	if cfg.TrackerURL != "http://tracker.example.com/announce" {
		t.Errorf("TrackerURL = %q, want the configured tracker", cfg.TrackerURL)
	}
	if len(cfg.Partitions) != 1 {
		t.Fatalf("Partitions = %+v, want exactly one partition", cfg.Partitions)
	}
	part := cfg.Partitions[0]
	if part.SourceDir != "/work/partitions/1" {
		t.Errorf("SourceDir = %q, want /work/partitions/1", part.SourceDir)
	}
	if wantDest := ingest.ContentMountPath(hash); part.DestDir != wantDest {
		t.Errorf("DestDir = %q, want %q", part.DestDir, wantDest)
	}
}

func TestPublishConfigFromEnv_MissingRequiredEnv(t *testing.T) {
	t.Setenv("TRACKER_URL", "")
	t.Setenv("PARTITION_CONTENT_HASH", "")
	t.Setenv("SOURCE_CONTENT_DIR", "")

	if _, err := publishConfigFromEnv(); err == nil {
		t.Fatal("publishConfigFromEnv: want error when required env is unset, got nil")
	}
}

func TestPublishConfigFromEnv_BadHash(t *testing.T) {
	t.Setenv("TRACKER_URL", "http://tracker.example.com/announce")
	t.Setenv("PARTITION_CONTENT_HASH", "not-a-valid-hash")
	t.Setenv("SOURCE_CONTENT_DIR", "/work/partitions/1")

	if _, err := publishConfigFromEnv(); err == nil {
		t.Fatal("publishConfigFromEnv: want error for invalid PARTITION_CONTENT_HASH, got nil")
	}
}
