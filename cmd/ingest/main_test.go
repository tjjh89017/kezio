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

import "testing"

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

	_, deps, err := buildFromEnv()
	if err != nil {
		t.Fatalf("buildFromEnv: %v", err)
	}
	if deps.Staging == nil || deps.StagedRemover == nil {
		t.Errorf("expected staging dependencies when STAGING_ROOT is set")
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
