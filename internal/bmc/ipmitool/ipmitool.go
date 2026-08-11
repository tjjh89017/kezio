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

// Package ipmitool implements internal/bmc.BMC over IPMI by shelling out
// to the "ipmitool" binary. Importing it registers the "ipmitool" scheme
// via init (blank-import pattern, same as internal/bmc/redfish).
//
// # Why this driver exists alongside internal/bmc/ipmi
//
// internal/bmc/ipmi (pure-Go, github.com/bougou/go-ipmi) is the
// recommended path: no external binary, works in the default manager
// image. This package is the opt-in fallback for BMC firmware that
// misbehaves against the pure-Go client but works against ipmitool's
// reference implementation. Choose "ipmitool://" only when a specific BMC
// demonstrably needs it; otherwise use "ipmi://".
//
// This is a manager-side driver, so it requires the ipmitool binary in the
// manager's container image. The default manager image is minimal
// distroless and does not carry it; operators needing "ipmitool://" must
// build/use the opt-in ipmitool-enabled image
// (docker/manager-ipmi/Dockerfile). If ipmitool isn't on PATH, methods
// fail with an actionable error (see execRunner.Run) rather than a raw
// exec error.
//
// # Command mapping
//
// Every bmc.BMC method issues exactly one ipmitool invocation:
//
//   - PowerOn: "chassis power on".
//   - PowerOff: "chassis power soft" (ACPI shutdown request to the OS) -
//     "chassis power off" is a hard power-down, the wrong default for the
//     same reason internal/bmc/redfish prefers GracefulShutdown over
//     ForceOff.
//   - ForcePowerOff: "chassis power off", the hard power-down PowerOff
//     deliberately avoids - the escalation for a machine with no OS to
//     answer "chassis power soft" (mirrors redfish's ForceOff).
//   - PowerCycle: "chassis power cycle" (one BMC action), mirroring
//     redfish's ForceRestart. Per spec, only guaranteed to act when the
//     chassis is already on.
//   - GetPowerState: "chassis power status", output parsed for "is on" /
//     "is off".
//   - SetOneTimePXEBoot: "chassis bootdev pxe" with no options - this is
//     the raw Set System Boot Options command (IPMI 28.12) leaving the
//     persistent bit clear, so it is valid for the next boot only.
//     Passing "options=persistent" would make it survive across boots, so
//     this driver never passes it.
//
// # Privilege level and credentials
//
// Every invocation authenticates with -L ADMINISTRATOR (chassis control
// and boot-option changes require it on most BMCs) and never passes -C,
// so it stays on ipmitool's default cipher suite rather than requesting a
// weaker one. address and creds are never logged or included verbatim in
// an error - see execRunner's redaction of -U/-P.
package ipmitool

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/tjjh89017/kezio/internal/bmc"
)

func init() {
	bmc.Register("ipmitool", connect)
}

// privilegeLevel is the IPMI session privilege level every invocation
// authenticates at. See the package doc.
const privilegeLevel = "ADMINISTRATOR"

// driver implements bmc.BMC by shelling out to ipmitool for one host:port,
// authenticating with one Credentials pair, on every call.
type driver struct {
	run   runner
	host  string
	port  string // "" means ipmitool's default (623)
	creds bmc.Credentials
}

// connect is the bmc.Driver registered for the "ipmitool" scheme. Unlike
// redfish's connect, this does not itself talk to the BMC - every
// invocation opens/tears down its own session, so unreachable-host or
// bad-credentials errors surface from the first method call, not Connect.
func connect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	host, port, err := hostPort(address)
	if err != nil {
		return nil, err
	}
	return &driver{run: execRunner{}, host: host, port: port, creds: creds}, nil
}

// hostPort extracts host and optional port. address must carry only
// host[:port], no path: an IPMI target isn't a resource tree the way a
// Redfish System is.
func hostPort(address *url.URL) (string, string, error) {
	host := address.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("ipmitool: address %s has no host", address.Redacted())
	}
	if path := strings.Trim(address.Path, "/"); path != "" {
		return "", "", fmt.Errorf("ipmitool: address %s must not include a path", address.Redacted())
	}
	return host, address.Port(), nil
}

// baseArgs are the ipmitool connection flags common to every invocation
// this driver makes for d: interface, target, credentials, and privilege
// level. Command-specific arguments are appended by each caller.
func (d *driver) baseArgs() []string {
	args := []string{"-I", "lanplus", "-H", d.host}
	if d.port != "" {
		args = append(args, "-p", d.port)
	}
	return append(args, "-U", d.creds.Username, "-P", d.creds.Password, "-L", privilegeLevel)
}

// ipmitool runs one ipmitool subcommand (args, e.g. "chassis", "power",
// "on") against d's target and returns its stdout.
func (d *driver) ipmitool(ctx context.Context, args ...string) (string, error) {
	return d.run.Run(ctx, append(d.baseArgs(), args...)...)
}

// PowerOn implements bmc.BMC.
func (d *driver) PowerOn(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "on"); err != nil {
		return fmt.Errorf("ipmitool: power on: %w", err)
	}
	return nil
}

// PowerOff implements bmc.BMC using "chassis power soft" (ACPI shutdown
// request), not "chassis power off" (hard power-down) - see the package doc.
func (d *driver) PowerOff(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "soft"); err != nil {
		return fmt.Errorf("ipmitool: power off: %w", err)
	}
	return nil
}

// ForcePowerOff implements bmc.BMC using "chassis power off": the BMC
// drops power itself, so it still lands on a machine parked in firmware
// setup that never answers "chassis power soft" - see the package doc.
func (d *driver) ForcePowerOff(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "off"); err != nil {
		return fmt.Errorf("ipmitool: force power off: %w", err)
	}
	return nil
}

// PowerCycle implements bmc.BMC.
func (d *driver) PowerCycle(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "cycle"); err != nil {
		return fmt.Errorf("ipmitool: power cycle: %w", err)
	}
	return nil
}

// GetPowerState implements bmc.BMC.
func (d *driver) GetPowerState(ctx context.Context) (bmc.PowerState, error) {
	out, err := d.ipmitool(ctx, "chassis", "power", "status")
	if err != nil {
		return bmc.PowerStateUnknown, fmt.Errorf("ipmitool: reading power state: %w", err)
	}
	return parsePowerStatus(out), nil
}

// parsePowerStatus maps ipmitool's "chassis power status" output (e.g.
// "Chassis Power is on") to a PowerState; unrecognized output maps to
// PowerStateUnknown.
func parsePowerStatus(out string) bmc.PowerState {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "power is on"):
		return bmc.PowerStateOn
	case strings.Contains(lower, "power is off"):
		return bmc.PowerStateOff
	default:
		return bmc.PowerStateUnknown
	}
}

// SetOneTimePXEBoot implements bmc.BMC. See the package doc for why
// "chassis bootdev pxe" with no options is a one-time-only override.
func (d *driver) SetOneTimePXEBoot(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "bootdev", "pxe"); err != nil {
		return fmt.Errorf("ipmitool: setting one-time PXE boot: %w", err)
	}
	return nil
}
