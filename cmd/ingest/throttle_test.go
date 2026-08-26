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
	"context"
	"os/exec"
	"testing"
)

// TestThrottledCommand_WrapsWhenToolsPresent assumes ionice and nice are
// both on PATH, true of the ingest container's debian:sid-slim base
// (util-linux and coreutils are both already required there) and of most
// CI/dev Linux hosts; it is skipped otherwise rather than asserting
// nothing.
func TestThrottledCommand_WrapsWhenToolsPresent(t *testing.T) {
	if _, err := exec.LookPath("ionice"); err != nil {
		t.Skip("ionice not on PATH")
	}
	if _, err := exec.LookPath("nice"); err != nil {
		t.Skip("nice not on PATH")
	}

	cmd := throttledCommand(context.Background(), "true", "-x")
	wantArgs := []string{"ionice", "-c2", "-n7", "nice", "-n", "19", "true", "-x"}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("Args = %v, want %v", cmd.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Errorf("Args[%d] = %q, want %q (full: %v)", i, cmd.Args[i], want, cmd.Args)
		}
	}
}

// TestThrottledCommand_RunsEvenWithoutThrottlingTools checks the
// best-effort contract holds regardless of what is on PATH: throttling
// is cosmetic to correctness, so the built command must always still run
// binary with exactly its own args as the trailing arguments.
func TestThrottledCommand_RunsEvenWithoutThrottlingTools(t *testing.T) {
	cmd := throttledCommand(context.Background(), "true", "-x")
	args := cmd.Args
	if len(args) < 2 || args[len(args)-2] != "true" || args[len(args)-1] != "-x" {
		t.Errorf("Args = %v, want it to end with [\"true\", \"-x\"]", args)
	}
}
