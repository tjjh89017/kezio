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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExecPartclone_Argv pins the full argv execPartclone.Clone shells
// out with, for both the fs-specific binaries and the partclone.dd
// fallback - the two families take different flag sets. It exists
// because of two production incidents:
//
//  1. The ingest Job runs as nonroot uid 65532, and partclone always
//     opens its logfile (default /var/log/partclone.log) before doing
//     anything else, exiting immediately if that open fails - which it
//     does under nonroot, since /var/log is root-owned. -L/--logfile
//     must redirect the logfile into the writable work dir alongside
//     targetDir.
//  2. partclone.dd has no -c/--clone flag: its getopt short-option
//     string omits 'c' entirely, unlike partclone.<fs>, which needs
//     -c to select clone mode among clone/restore/dev-to-dev/domain.
//     Passing -c to partclone.dd makes it dump its usage and exit(1)
//     instead of cloning - this broke the BIOS-boot-partition (no filesystem)
//     path of the image-path e2e, which routes to the dd fallback.
//
// This test also pins -T (btfiles - the extent-slice output mode
// internal/store.ValidateContentDir expects), -s (source) and -o
// (targetDir) for both variants, so a regression on any of those flags
// fails loudly instead of surfacing as a runtime partclone exit status.
func TestExecPartclone_Argv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fakes a partclone binary via a POSIX shell script")
	}

	tests := []struct {
		name       string
		fakeBinary string // binary name to fake on PATH
		fsType     string // fsType passed to Clone
		wantClone  bool   // whether -c should appear in argv
	}{
		{
			name:       "fs-specific binary takes -c",
			fakeBinary: "partclone.ext4",
			fsType:     "ext4",
			wantClone:  true,
		},
		{
			name:       "dd fallback binary has no -c",
			fakeBinary: ddFallbackBinary,
			fsType:     "", // no blkid signature -> dd fallback directly
			wantClone:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			recordFile := filepath.Join(binDir, "argv.recorded")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(recordFile) + "\n"
			scriptPath := filepath.Join(binDir, tt.fakeBinary)
			if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
				t.Fatalf("write fake %s: %v", tt.fakeBinary, err)
			}

			t.Setenv("PATH", binDir)

			workDir := t.TempDir()
			source := filepath.Join(workDir, "part-1.raw")
			targetDir := filepath.Join(workDir, "content-1")

			if err := (execPartclone{}).Clone(context.Background(), tt.fsType, source, targetDir); err != nil {
				t.Fatalf("Clone: %v", err)
			}

			recorded, err := os.ReadFile(recordFile)
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			got := strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")

			wantLogPath := filepath.Join(workDir, "partclone.content-1.log")
			want := []string{"-F", "-s", source, "-o", targetDir, "-T", "-L", wantLogPath}
			if tt.wantClone {
				want = append([]string{"-c"}, want...)
			}

			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("argv = %q, want %q", got, want)
			}
		})
	}
}

// shellQuote wraps s in single quotes for embedding in a generated sh
// script, escaping any single quotes s itself contains. t.TempDir()
// paths never contain them in practice, but this keeps the generated
// script correct regardless.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
