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

package ipmitool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// errIpmitoolNotFound is returned (wrapped, with guidance) when execRunner
// cannot resolve "ipmitool" on PATH. It is a distinct sentinel so callers
// or tests can identify this specific failure with errors.Is, separate
// from ipmitool exiting non-zero or a BMC being unreachable.
var errIpmitoolNotFound = errors.New("ipmitool not found in PATH")

// runner executes one ipmitool invocation and returns its stdout. driver
// calls ipmitool through this narrow interface instead of calling os/exec
// directly, so PowerOn/PowerOff/PowerCycle/GetPowerState/SetOneTimePXEBoot
// are unit-testable against a fake runner that records the exact argv
// each method issued, without a real BMC or the ipmitool binary present -
// the same split internal/agent/deploy.Runner uses for the commands it
// shells out to.
type runner interface {
	// Run executes ipmitool with args and returns its stdout. A non-nil
	// error means ipmitool failed to start or exited non-zero; the error
	// text never contains a credential (see execRunner).
	Run(ctx context.Context, args ...string) (string, error)
}

// execRunner implements runner by shelling out to the "ipmitool" binary
// on PATH (or, in tests, whatever binary path is set). It is the runner
// connect wires up for production use.
type execRunner struct {
	// path overrides the binary to execute; empty means "ipmitool" on
	// PATH. It exists so tests can point at a non-ipmitool binary (or a
	// path that does not exist) without needing a real BMC, and is
	// otherwise left unset in production.
	path string
}

// Run implements runner.
func (r execRunner) Run(ctx context.Context, args ...string) (string, error) {
	path := r.path
	if path == "" {
		resolved, err := exec.LookPath("ipmitool")
		if err != nil {
			// The default manager image does not carry ipmitool - only the
			// opt-in ipmitool-enabled image (docker/manager-ipmi/Dockerfile)
			// does. Surface that as an actionable error instead of the raw
			// "executable file not found in $PATH" LookPath gives, so an
			// operator who hits this knows what to do next rather than
			// having to guess: switch this Machine to ipmi:// (the pure-Go
			// driver, works in the default image), or to redfish://, or
			// deploy the ipmitool-enabled manager image if ipmitool://
			// itself is genuinely needed.
			return "", fmt.Errorf("ipmitool: %w: the default manager image does not carry ipmitool; use ipmi:// (pure-Go) or redfish://, or deploy the ipmitool-enabled manager image (see docker/manager-ipmi/Dockerfile): %w", errIpmitoolNotFound, err)
		}
		path = resolved
	}

	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// args holds -U/-P with plaintext BMC credentials; redactArgs
		// strips them before they can end up in this (or any caller's)
		// error message. stderr is ipmitool's/the BMC's own output and
		// is included verbatim - ipmitool does not echo back the
		// password it was given, so this is not a second leak path.
		return "", fmt.Errorf("running %s %s: %s: %w",
			path, strings.Join(redactArgs(args), " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

// redactArgs returns a copy of args with the value following every "-U"
// and "-P" flag replaced by a placeholder, so an argv that embeds BMC
// credentials can be safely included in a log line or error message.
func redactArgs(args []string) []string {
	redacted := make([]string, len(args))
	copy(redacted, args)
	for i := 0; i < len(redacted)-1; i++ {
		if redacted[i] == "-U" || redacted[i] == "-P" {
			redacted[i+1] = "REDACTED"
		}
	}
	return redacted
}
