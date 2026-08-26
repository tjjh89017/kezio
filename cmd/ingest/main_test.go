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
	if cfg.AttachMode != "" {
		t.Errorf("AttachMode = %q, want empty (IMAGE_INGEST_ATTACH unset)", cfg.AttachMode)
	}
	if deps.Attacher == nil {
		t.Error("deps.Attacher is nil, want an Attacher wired by default (IMAGE_INGEST_ATTACH unset means nbd)")
	}
}

func TestBuildFromEnv_AttachModeCopySkipsAttacher(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	t.Setenv("SOURCE_URL", "https://example.com/image.qcow2")
	t.Setenv("SOURCE_FORMAT", "qcow2")
	t.Setenv("WORK_DIR", workDir)
	t.Setenv("IMAGE_INGEST_ATTACH", "copy")

	cfg, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}
	if cfg.AttachMode != ingest.AttachModeCopy {
		t.Errorf("AttachMode = %q, want %q", cfg.AttachMode, ingest.AttachModeCopy)
	}
	if deps.Attacher != nil {
		t.Errorf("deps.Attacher = %v, want nil for AttachModeCopy", deps.Attacher)
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
	const contentName = "ubuntu-2404-p1"
	t.Setenv("PARTITION_CONTENT_NAME", contentName)
	t.Setenv("SOURCE_CONTENT_DIR", "/work/partitions/1")

	cfg, err := publishConfigFromEnv()
	if err != nil {
		t.Fatalf("publishConfigFromEnv: %v", err)
	}

	if len(cfg.Partitions) != 1 {
		t.Fatalf("Partitions = %+v, want exactly one partition", cfg.Partitions)
	}
	part := cfg.Partitions[0]
	if part.SourceDir != "/work/partitions/1" {
		t.Errorf("SourceDir = %q, want /work/partitions/1", part.SourceDir)
	}
	if wantDest := ingest.ContentMountPath(contentName); part.DestDir != wantDest {
		t.Errorf("DestDir = %q, want %q", part.DestDir, wantDest)
	}
}

func TestPublishConfigFromEnv_MissingRequiredEnv(t *testing.T) {
	t.Setenv("PARTITION_CONTENT_NAME", "")
	t.Setenv("SOURCE_CONTENT_DIR", "")

	if _, err := publishConfigFromEnv(); err == nil {
		t.Fatal("publishConfigFromEnv: want error when required env is unset, got nil")
	}
}

func TestBoundResult_OversizedSuccessBecomesAFailure(t *testing.T) {
	huge := make([]byte, ingest.TerminationMessageLimit)
	for i := range huge {
		huge[i] = 'x'
	}
	result := boundResult(ingest.Result{
		Success: true,
		Disk:    &ingest.ResultDisk{PartitionTable: "gpt", SfdiskJSON: string(huge)},
	})
	if result.Success {
		t.Fatal("boundResult: want a failure for a result over the termination message limit")
	}
	if result.Error == "" {
		t.Error("result.Error is empty, want a message naming the size limit")
	}
}

func TestBoundResult_ResultThatFitsIsUntouched(t *testing.T) {
	in := ingest.Result{Success: true, Publish: &ingest.ResultPublish{InfoHash: "abc"}}
	if got := boundResult(in); !got.Success || got.Publish == nil {
		t.Errorf("boundResult changed a result that fits: %+v", got)
	}
}
