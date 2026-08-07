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
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// fakeRunner is a runner that records every argv it was asked to execute
// and returns canned output/error, so a test can assert on the exact
// ipmitool invocation a driver method issued without a real ipmitool
// binary or BMC.
type fakeRunner struct {
	calls  [][]string
	output string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	call := make([]string, len(args))
	copy(call, args)
	f.calls = append(f.calls, call)
	return f.output, f.err
}

func newTestDriver(run *fakeRunner) *driver {
	return &driver{
		run:   run,
		host:  "10.0.0.10",
		port:  "",
		creds: bmc.Credentials{Username: "admin", Password: "hunter2"},
	}
}

// lastCall returns the last argv fakeRunner recorded, failing the test if
// none was recorded.
func lastCall(t *testing.T, run *fakeRunner) []string {
	t.Helper()
	if len(run.calls) == 0 {
		t.Fatal("runner was not invoked")
	}
	return run.calls[len(run.calls)-1]
}

// hasSubsequence reports whether want appears, in order and contiguously,
// somewhere in got - used to check the command-specific tail of an argv
// without over-specifying the connection flags that precede it.
func hasSubsequence(got, want []string) bool {
	if len(want) > len(got) {
		return false
	}
	for i := 0; i+len(want) <= len(got); i++ {
		match := true
		for j, w := range want {
			if got[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestPowerOnIssuesChassisPowerOn(t *testing.T) {
	run := &fakeRunner{}
	d := newTestDriver(run)

	if err := d.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}

	call := lastCall(t, run)
	if !hasSubsequence(call, []string{"chassis", "power", "on"}) {
		t.Errorf("argv = %v, want it to end with chassis power on", call)
	}
}

func TestPowerOffIssuesChassisPowerSoft(t *testing.T) {
	run := &fakeRunner{}
	d := newTestDriver(run)

	if err := d.PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}

	call := lastCall(t, run)
	if !hasSubsequence(call, []string{"chassis", "power", "soft"}) {
		t.Errorf("argv = %v, want it to end with chassis power soft (graceful ACPI shutdown), not power off (hard)", call)
	}
}

func TestPowerCycleIssuesChassisPowerCycle(t *testing.T) {
	run := &fakeRunner{}
	d := newTestDriver(run)

	if err := d.PowerCycle(context.Background()); err != nil {
		t.Fatalf("PowerCycle() error = %v", err)
	}

	call := lastCall(t, run)
	if !hasSubsequence(call, []string{"chassis", "power", "cycle"}) {
		t.Errorf("argv = %v, want it to end with chassis power cycle", call)
	}
}

func TestSetOneTimePXEBootIssuesBootdevPxeWithoutPersistentOption(t *testing.T) {
	run := &fakeRunner{}
	d := newTestDriver(run)

	if err := d.SetOneTimePXEBoot(context.Background()); err != nil {
		t.Fatalf("SetOneTimePXEBoot() error = %v", err)
	}

	call := lastCall(t, run)
	if !hasSubsequence(call, []string{"chassis", "bootdev", "pxe"}) {
		t.Errorf("argv = %v, want it to end with chassis bootdev pxe", call)
	}
	for _, arg := range call {
		if strings.Contains(arg, "persistent") {
			t.Errorf("argv = %v, must not request a persistent boot override for a one-time PXE boot", call)
		}
	}
}

func TestGetPowerStateParsesChassisPowerStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bmc.PowerState
	}{
		{name: "on", output: "Chassis Power is on\n", want: bmc.PowerStateOn},
		{name: "off", output: "Chassis Power is off\n", want: bmc.PowerStateOff},
		{name: "unrecognized output maps to unknown", output: "Chassis Power Control: Unknown\n", want: bmc.PowerStateUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &fakeRunner{output: tt.output}
			d := newTestDriver(run)

			got, err := d.GetPowerState(context.Background())
			if err != nil {
				t.Fatalf("GetPowerState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("GetPowerState() = %v, want %v", got, tt.want)
			}

			call := lastCall(t, run)
			if !hasSubsequence(call, []string{"chassis", "power", "status"}) {
				t.Errorf("argv = %v, want it to end with chassis power status", call)
			}
		})
	}
}

func TestBaseArgsUsesLanplusAndAdministratorPrivilege(t *testing.T) {
	d := newTestDriver(&fakeRunner{})

	args := d.baseArgs()
	if !hasSubsequence(args, []string{"-I", "lanplus"}) {
		t.Errorf("baseArgs() = %v, want -I lanplus", args)
	}
	if !hasSubsequence(args, []string{"-L", "ADMINISTRATOR"}) {
		t.Errorf("baseArgs() = %v, want -L ADMINISTRATOR", args)
	}
	if !hasSubsequence(args, []string{"-H", d.host}) {
		t.Errorf("baseArgs() = %v, want -H %s", args, d.host)
	}
	for _, arg := range args {
		if arg == "-C" {
			t.Errorf("baseArgs() = %v, must not override the cipher suite", args)
		}
	}
}

func TestBaseArgsOmitsPortFlagWhenAddressHasNoPort(t *testing.T) {
	d := newTestDriver(&fakeRunner{})
	args := d.baseArgs()
	for _, arg := range args {
		if arg == "-p" {
			t.Errorf("baseArgs() = %v, want no -p flag when no port was given", args)
		}
	}
}

func TestBaseArgsIncludesPortFlagWhenAddressHasPort(t *testing.T) {
	d := newTestDriver(&fakeRunner{})
	d.port = "6230"

	if !hasSubsequence(d.baseArgs(), []string{"-p", "6230"}) {
		t.Errorf("baseArgs() = %v, want -p 6230", d.baseArgs())
	}
}

func TestErrorsDoNotContainCredentials(t *testing.T) {
	const password = "correct-horse-battery-staple"
	run := &fakeRunner{err: errFromRunner()}
	d := &driver{run: run, host: "10.0.0.10", creds: bmc.Credentials{Username: "admin", Password: password}}

	methods := map[string]func() error{
		"PowerOn":           func() error { return d.PowerOn(context.Background()) },
		"PowerOff":          func() error { return d.PowerOff(context.Background()) },
		"PowerCycle":        func() error { return d.PowerCycle(context.Background()) },
		"SetOneTimePXEBoot": func() error { return d.SetOneTimePXEBoot(context.Background()) },
		"GetPowerState": func() error {
			_, err := d.GetPowerState(context.Background())
			return err
		},
	}

	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("call succeeded, want an error to check for leaked credentials")
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("%s error leaked the password: %q", name, err.Error())
			}
		})
	}
}

func errFromRunner() error {
	return &runnerError{"simulated ipmitool failure"}
}

type runnerError struct{ msg string }

func (e *runnerError) Error() string { return e.msg }

func TestHostPortParsesHostAndOptionalPort(t *testing.T) {
	tests := []struct {
		name     string
		address  string
		wantHost string
		wantPort string
	}{
		{name: "host only", address: "ipmitool://10.0.0.10", wantHost: "10.0.0.10", wantPort: ""},
		{name: "host and port", address: "ipmitool://10.0.0.10:6230", wantHost: "10.0.0.10", wantPort: "6230"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.address)
			if err != nil {
				t.Fatalf("parsing test address: %v", err)
			}
			host, port, err := hostPort(u)
			if err != nil {
				t.Fatalf("hostPort() error = %v", err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("hostPort() = (%q, %q), want (%q, %q)", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestHostPortRejectsAddressWithoutHost(t *testing.T) {
	u, err := url.Parse("ipmitool://")
	if err != nil {
		t.Fatalf("parsing test address: %v", err)
	}
	if _, _, err := hostPort(u); err == nil {
		t.Fatal("hostPort() with no host succeeded, want an error")
	}
}

func TestHostPortRejectsAddressWithPath(t *testing.T) {
	u, err := url.Parse("ipmitool://10.0.0.10/some/path")
	if err != nil {
		t.Fatalf("parsing test address: %v", err)
	}
	if _, _, err := hostPort(u); err == nil {
		t.Fatal("hostPort() with a path succeeded, want an error")
	}
}

// TestConnectRegistersIPMIToolScheme confirms importing this package makes
// bmc.Connect resolve "ipmitool://" to this driver.
func TestConnectRegistersIPMIToolScheme(t *testing.T) {
	b, err := bmc.Connect(context.Background(), "ipmitool://10.0.0.10:6230", bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err != nil {
		t.Fatalf("bmc.Connect() error = %v", err)
	}
	if b == nil {
		t.Fatal("bmc.Connect() returned a nil BMC with no error")
	}
	if _, ok := b.(*driver); !ok {
		t.Errorf("bmc.Connect() returned a %T, want *ipmitool.driver", b)
	}
}

func TestConnectRejectsAddressWithoutHost(t *testing.T) {
	_, err := bmc.Connect(context.Background(), "ipmitool:///some/path", bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err == nil {
		t.Fatal("bmc.Connect() with no host succeeded, want an error")
	}
}
