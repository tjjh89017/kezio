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

package ipmi

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestExecRunnerRedactsCredentialsOnError drives execRunner's error path
// (pointed at a binary that does not exist, so no real ipmitool or BMC is
// needed) and checks the resulting error never contains the plaintext
// password that was passed on the (simulated) command line.
func TestExecRunnerRedactsCredentialsOnError(t *testing.T) {
	const password = "correct-horse-battery-staple"
	r := execRunner{path: "/nonexistent-ipmitool-binary-for-kezio-tests"}

	_, err := r.Run(context.Background(), "-I", "lanplus", "-H", "10.0.0.10", "-U", "admin", "-P", password, "-L", "ADMINISTRATOR", "chassis", "power", "on")
	if err == nil {
		t.Fatal("Run() against a nonexistent binary succeeded, want an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("Run() error leaked the password: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("Run() error = %q, want it to show the argv with credentials redacted", err.Error())
	}
}

// TestExecRunnerReturnsFriendlyErrorWhenIpmitoolNotFound points PATH at an
// empty directory (so exec.LookPath("ipmitool") fails the way it would on
// the default, Redfish-only manager image, which never carries the
// ipmitool binary) and checks execRunner.Run returns a friendly, actionable
// error instead of a raw "executable file not found" message.
func TestExecRunnerReturnsFriendlyErrorWhenIpmitoolNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	r := execRunner{}
	_, err := r.Run(context.Background(), "-I", "lanplus", "-H", "10.0.0.10", "chassis", "power", "on")
	if err == nil {
		t.Fatal("Run() with no ipmitool on PATH succeeded, want an error")
	}
	if !errors.Is(err, errIpmitoolNotFound) {
		t.Errorf("Run() error = %q, want it to wrap errIpmitoolNotFound", err.Error())
	}
	if !strings.Contains(err.Error(), "redfish://") {
		t.Errorf("Run() error = %q, want it to point operators at the redfish:// alternative", err.Error())
	}
	if !strings.Contains(err.Error(), "docker/manager-ipmi/Dockerfile") {
		t.Errorf("Run() error = %q, want it to name the ipmitool-enabled image variant", err.Error())
	}
}

// TestExecRunnerFindsIpmitoolOnPath is the inverse of the not-found case:
// with a fake "ipmitool" executable placed on PATH, execRunner must resolve
// and run it rather than reporting errIpmitoolNotFound.
func TestExecRunnerFindsIpmitoolOnPath(t *testing.T) {
	dir := t.TempDir()
	fake := dir + "/ipmitool"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake ipmitool: %v", err)
	}
	t.Setenv("PATH", dir)

	r := execRunner{}
	if _, err := r.Run(context.Background(), "chassis", "power", "on"); err != nil {
		t.Fatalf("Run() error = %v, want the fake ipmitool on PATH to be found and run", err)
	}
}

func TestRedactArgsReplacesUserAndPasswordValues(t *testing.T) {
	args := []string{"-I", "lanplus", "-H", "10.0.0.10", "-U", "admin", "-P", "hunter2", "-L", "ADMINISTRATOR", "chassis", "power", "on"}
	got := redactArgs(args)

	joined := strings.Join(got, " ")
	if strings.Contains(joined, "hunter2") {
		t.Errorf("redactArgs() = %v, still contains the password", got)
	}
	if strings.Contains(joined, "admin") {
		t.Errorf("redactArgs() = %v, still contains the username", got)
	}
	// The original slice must be untouched: a caller that logs args
	// itself after calling redactArgs must not observe them mutated.
	if args[7] != "hunter2" || args[5] != "admin" {
		t.Errorf("redactArgs() mutated its input: %v", args)
	}
}

func TestRedactArgsHandlesFlagWithNoFollowingValue(t *testing.T) {
	// A malformed/truncated argv ending in a bare "-P" must not panic.
	got := redactArgs([]string{"chassis", "power", "on", "-P"})
	if len(got) != 4 {
		t.Errorf("redactArgs() = %v, want 4 elements", got)
	}
}
