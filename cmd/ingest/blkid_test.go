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
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// runExitCode runs a shell that exits with code, returning the resulting
// *exec.ExitError the way a real blkid invocation would produce it - the
// seam interpretBlkidError is tested against, without shelling out to
// blkid itself.
func runExitCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatalf("sh -c exit %d: want a non-nil error", code)
	}
	return err
}

func TestInterpretBlkidError_NoSignatureExitCodeIsNotAnError(t *testing.T) {
	runErr := runExitCode(t, blkidNoSignatureExitCode)

	value, found, err := interpretBlkidError(runErr, "TYPE", "/work/part1")

	if err != nil {
		t.Fatalf("interpretBlkidError: unexpected error %v", err)
	}
	if found {
		t.Errorf("found = true, want false for exit code %d (no signature)", blkidNoSignatureExitCode)
	}
	if value != "" {
		t.Errorf("value = %q, want empty", value)
	}
}

func TestInterpretBlkidError_OtherExitCodeIsAnError(t *testing.T) {
	const otherCode = 1
	runErr := runExitCode(t, otherCode)

	value, found, err := interpretBlkidError(runErr, "TYPE", "/work/part1")

	if err == nil {
		t.Fatal("interpretBlkidError: want error for a non-no-signature exit code, got nil")
	}
	if found {
		t.Errorf("found = true, want false on error")
	}
	if value != "" {
		t.Errorf("value = %q, want empty on error", value)
	}
	if !strings.Contains(err.Error(), "TYPE") || !strings.Contains(err.Error(), "/work/part1") {
		t.Errorf("error %q does not name the tag/path it failed for", err)
	}
}
