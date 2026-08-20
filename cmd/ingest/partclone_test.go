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
)

// installFakeBinary creates an executable file named name in dir, so
// exec.LookPath finds it once dir is on PATH.
func installFakeBinary(t *testing.T, dir, name string) {
	t.Helper()
	//nolint:gosec // test fixture executable
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary %s: %v", name, err)
	}
}

func TestPartcloneBinary_KnownFSTypePresentOnPATH(t *testing.T) {
	dir := t.TempDir()
	installFakeBinary(t, dir, "partclone.ext4")
	t.Setenv("PATH", dir)

	got := partcloneBinary("ext4")

	if got != "partclone.ext4" {
		t.Errorf("partcloneBinary(%q) = %q, want partclone.ext4 (present on PATH)", "ext4", got)
	}
}

func TestPartcloneBinary_AliasedFSTypePresentOnPATH(t *testing.T) {
	dir := t.TempDir()
	installFakeBinary(t, dir, "partclone.fat")
	t.Setenv("PATH", dir)

	got := partcloneBinary("msdos")

	if got != "partclone.fat" {
		t.Errorf("partcloneBinary(%q) = %q, want partclone.fat (msdos aliases to fat)", "msdos", got)
	}
}

func TestPartcloneBinary_KnownFSTypeAbsentFromPATHFallsBackToDD(t *testing.T) {
	dir := t.TempDir() // empty: no partclone.* binaries installed
	t.Setenv("PATH", dir)

	got := partcloneBinary("xfs")

	if got != ddFallbackBinary {
		t.Errorf("partcloneBinary(%q) = %q, want %q (not on PATH)", "xfs", got, ddFallbackBinary)
	}
}

func TestPartcloneBinary_EmptyFSTypeFallsBackToDD(t *testing.T) {
	dir := t.TempDir()
	installFakeBinary(t, dir, "partclone.dd")
	t.Setenv("PATH", dir)

	got := partcloneBinary("")

	if got != ddFallbackBinary {
		t.Errorf("partcloneBinary(\"\") = %q, want %q (blkid found no signature)", got, ddFallbackBinary)
	}
}
