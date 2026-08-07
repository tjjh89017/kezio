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

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/tjjh89017/kezio/internal/ingest"
)

func TestBuildFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("IMAGE_NAME", "")
	t.Setenv("SOURCE_URL", "")
	t.Setenv("SOURCE_FORMAT", "")
	t.Setenv("STORE_ROOT", "")

	if _, _, err := buildFromEnv(); err == nil {
		t.Fatal("expected an error when required environment variables are missing")
	}
}

func TestBuildFromEnv_Minimal(t *testing.T) {
	storeRoot := t.TempDir()
	workDir := t.TempDir()
	t.Setenv("IMAGE_NAME", "golden")
	t.Setenv("SOURCE_URL", "https://example.com/golden.qcow2")
	t.Setenv("SOURCE_FORMAT", "qcow2")
	t.Setenv("STORE_ROOT", storeRoot)
	t.Setenv("WORK_DIR", workDir)
	t.Setenv("STAGING_ROOT", "")
	t.Setenv("IMAGE_NAMESPACE", "default")
	t.Setenv("IMAGE_UID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("IMAGE_API_VERSION", "kezio.kojuro.date/v1alpha1")
	t.Setenv("IMAGE_KIND", "Image")

	cfg, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}
	if cfg.ImageName != "golden" || cfg.StoreRoot != storeRoot || cfg.WorkDir != workDir {
		t.Errorf("cfg = %+v", cfg)
	}
	if deps.Staging != nil || deps.StagedRemover != nil {
		t.Errorf("expected no staging dependencies without STAGING_ROOT")
	}
	if deps.Downloader == nil || deps.QemuImg == nil || deps.Sfdisk == nil || deps.Blkid == nil || deps.Partclone == nil {
		t.Errorf("expected every non-staging dependency to be wired: %+v", deps)
	}
	if deps.LayoutWriter == nil {
		t.Error("expected a LayoutWriter to be wired")
	}
}

func TestBuildFromEnv_WithStaging(t *testing.T) {
	storeRoot := t.TempDir()
	stagingRoot := t.TempDir()
	t.Setenv("IMAGE_NAME", "golden")
	t.Setenv("SOURCE_URL", "kezio-staged://golden")
	t.Setenv("SOURCE_FORMAT", "qcow2")
	t.Setenv("STORE_ROOT", storeRoot)
	t.Setenv("WORK_DIR", t.TempDir())
	t.Setenv("STAGING_ROOT", stagingRoot)
	t.Setenv("IMAGE_NAMESPACE", "default")
	t.Setenv("IMAGE_UID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("IMAGE_API_VERSION", "kezio.kojuro.date/v1alpha1")
	t.Setenv("IMAGE_KIND", "Image")

	_, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}
	if deps.Staging == nil || deps.StagedRemover == nil {
		t.Errorf("expected staging dependencies when STAGING_ROOT is set")
	}
}

// TestRealWiring_StagedSourceWithoutStagingRoot exercises the exact call
// chain the ingest Job's kezio-ingest binary runs: buildFromEnv's real,
// exec-backed Dependencies fed straight into ingest.Run, the same as
// cmd/ingest/main.go's run(). It reproduces the deployment configuration
// gap that crashed the image-path e2e's ingest Job: a SOURCE_URL of
// kezio-staged://... reaching ResolveSource with a nil Staging
// dependency because STAGING_ROOT (from INGEST_STAGING_PVC on the
// controller-manager - see Makefile's deploy-image-path target) was
// never set. buildFromEnv itself is correct here - leaving Staging nil
// when STAGING_ROOT is unset is by design, since Staging is optional for
// http(s):// sources (see TestBuildFromEnv_Minimal). The bug was that
// nothing downstream validated the combination "staged source + nil
// Staging" before dereferencing it.
//
// Before the fix, ingest.Run panics with a nil pointer dereference
// inside internal/ingest.ResolveSource; after the fix it returns a
// Result carrying an actionable error instead of crashing the process.
func TestRealWiring_StagedSourceWithoutStagingRoot(t *testing.T) {
	storeRoot := t.TempDir()
	workDir := t.TempDir()
	t.Setenv("IMAGE_NAME", "golden")
	t.Setenv("SOURCE_URL", "kezio-staged://golden")
	t.Setenv("SOURCE_FORMAT", "qcow2")
	t.Setenv("STORE_ROOT", storeRoot)
	t.Setenv("WORK_DIR", workDir)
	t.Setenv("STAGING_ROOT", "")
	t.Setenv("IMAGE_NAMESPACE", "default")
	t.Setenv("IMAGE_UID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("IMAGE_API_VERSION", "kezio.kojuro.date/v1alpha1")
	t.Setenv("IMAGE_KIND", "Image")

	cfg, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}
	if deps.Staging != nil {
		t.Fatalf("test setup invalid: expected deps.Staging to be nil without STAGING_ROOT, got %v", deps.Staging)
	}

	result := ingest.Run(context.Background(), cfg, deps)
	if result.Success {
		t.Fatal("expected ingest.Run to fail for a staged source with no staging resolver wired")
	}
	if !strings.Contains(result.Error, "staging") {
		t.Errorf("result.Error = %q, want a message mentioning the missing staging resolver", result.Error)
	}
}

func TestPartcloneBinary(t *testing.T) {
	if got := partcloneBinary(""); got != ddFallbackBinary {
		t.Errorf("partcloneBinary(\"\") = %q, want %q", got, ddFallbackBinary)
	}
	// A file system name that certainly has no partclone.<name> binary
	// on the test runner's PATH must fall back to dd.
	if got := partcloneBinary("definitely-not-a-real-fs-xyz"); got != ddFallbackBinary {
		t.Errorf("partcloneBinary(unknown) = %q, want %q", got, ddFallbackBinary)
	}
}
