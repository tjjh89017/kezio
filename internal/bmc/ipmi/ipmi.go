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

// Package ipmi implements internal/bmc.BMC over IPMI, by shelling out to
// the "ipmitool" binary. Importing this package registers the "ipmi"
// scheme with internal/bmc's registry (see this file's init) as a side
// effect - the same import-for-side-effect pattern internal/bmc/redfish
// uses - so a caller only needs a blank import
// (`_ "github.com/tjjh89017/kezio/internal/bmc/ipmi"`) to make bmc.Connect
// resolve that scheme.
//
// # Library choice: exec ipmitool, not a pure-Go IPMI library
//
// The realistic pure-Go alternative is github.com/vmware/goipmi. It was
// evaluated and rejected for this driver: its native, dependency-free
// transport only implements the "lan" interface (IPMI 1.5 RMCP, MD5/plain
// password authentication) - the interface field it exposes for "lanplus"
// (IPMI 2.0 RMCP+, the interface effectively every BMC shipped in the last
// decade requires and defaults to) is not natively implemented at all; it
// silently shells out to the ipmitool binary itself. So the one operation
// mode that matters for real hardware already depends on ipmitool even
// with goipmi in the dependency graph, which erases the "pure Go, no
// external binary" benefit the library would otherwise offer. goipmi has
// also had no release since 2018 and does not track later IPMI 2.0
// cipher-suite work. Given that a working driver needs ipmitool on the
// image either way, this package execs it directly rather than through
// goipmi's own (comparatively poorly tested) tool-transport shim -
// ipmitool is the actively maintained, spec-compliant reference client.
//
// This is a manager-side driver (it runs in the kezio controller, not in
// the live/deploy image), so the tradeoff this choice accepts is
// deliberate: the manager's container image must carry the ipmitool
// binary. As of this driver landing, the manager's Dockerfile still
// builds FROM a distroless base with no package manager and does not
// include it - wiring ipmitool into that image is tracked as separate,
// follow-up work; until it lands, this driver's methods will fail at
// exec time (a clear "executable file not found" error) rather than at
// build or Connect time.
//
// # Command mapping
//
// Every bmc.BMC method issues exactly one ipmitool invocation:
//
//   - PowerOn: "chassis power on".
//   - PowerOff: "chassis power soft", which asks the OS to ACPI-shutdown
//     itself rather than cutting power (ipmitool's "chassis power off" is
//     a hard, non-graceful power-down - the wrong default for the same
//     reason internal/bmc/redfish prefers GracefulShutdown over ForceOff).
//   - PowerCycle: "chassis power cycle" (power off, pause, power back on
//     as one BMC action), mirroring redfish's ForceRestart semantics.
//     Per the IPMI spec this is only guaranteed to act when the chassis
//     is already on; a BMC that rejects it while already off is IPMI
//     protocol behavior this driver does not work around.
//   - GetPowerState: "chassis power status", whose output text is parsed
//     for "is on" / "is off".
//   - SetOneTimePXEBoot: "chassis bootdev pxe" with no options. Under the
//     hood this is the raw Set System Boot Options command (IPMI section
//     28.12) against parameter 5 (Boot Flags), setting the "boot flags
//     valid" bit and leaving the persistent bit clear - valid for the
//     next boot only, which is exactly what this method promises.
//     Passing "options=persistent" would flip that bit and make the
//     override survive across boots, so this driver deliberately never
//     passes it.
//
// # Privilege level and credentials
//
// Every invocation authenticates with -L ADMINISTRATOR: chassis control
// and boot-option changes require it on most BMCs, and a lower privilege
// level would make some of these operations fail unpredictably depending
// on the BMC's configured privilege ceiling for the account. Every
// invocation also uses ipmitool's default IPMI 2.0 cipher suite (no -C
// flag is ever passed) - IPMI's available cipher suites are weaker than
// modern TLS by protocol design, which is inherent to IPMI and not a gap
// this driver can close, but this driver at least never asks for a
// weaker one than ipmitool already defaults to.
//
// address and creds are never logged or included verbatim in an error:
// see execRunner's redaction of -U/-P before formatting any error.
package ipmi

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/tjjh89017/kezio/internal/bmc"
)

func init() {
	bmc.Register("ipmi", connect)
}

// privilegeLevel is the IPMI session privilege level every invocation
// authenticates at. See this package's doc comment.
const privilegeLevel = "ADMINISTRATOR"

// driver implements bmc.BMC by shelling out to ipmitool for one host:port,
// authenticating with one Credentials pair, on every call.
type driver struct {
	run   runner
	host  string
	port  string // "" means ipmitool's default (623)
	creds bmc.Credentials
}

// connect is the bmc.Driver registered for the "ipmi" scheme. Unlike
// internal/bmc/redfish's connect, this does not itself talk to the BMC:
// IPMI has no persistent handshake this driver needs up front (every
// ipmitool invocation opens and tears down its own session), so any
// unreachable-host or bad-credentials error surfaces from the first
// method call instead of from Connect.
func connect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	host, port, err := hostPort(address)
	if err != nil {
		return nil, err
	}
	return &driver{run: execRunner{}, host: host, port: port, creds: creds}, nil
}

// hostPort extracts the BMC's host and optional port from address. address
// must carry only a host[:port] (and no path): an IPMI target is not a
// resource tree the way a Redfish System is, so there is nothing else in
// the address for this driver to interpret.
func hostPort(address *url.URL) (string, string, error) {
	host := address.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("ipmi: address %s has no host", address.Redacted())
	}
	if path := strings.Trim(address.Path, "/"); path != "" {
		return "", "", fmt.Errorf("ipmi: address %s must not include a path", address.Redacted())
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
		return fmt.Errorf("ipmi: power on: %w", err)
	}
	return nil
}

// PowerOff implements bmc.BMC. It issues "chassis power soft" (an ACPI
// soft-shutdown request to the running OS) rather than "chassis power
// off" (an immediate hard power-down): see this package's doc comment for
// the rationale, which mirrors internal/bmc/redfish's PowerOff.
func (d *driver) PowerOff(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "soft"); err != nil {
		return fmt.Errorf("ipmi: power off: %w", err)
	}
	return nil
}

// PowerCycle implements bmc.BMC.
func (d *driver) PowerCycle(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "power", "cycle"); err != nil {
		return fmt.Errorf("ipmi: power cycle: %w", err)
	}
	return nil
}

// GetPowerState implements bmc.BMC.
func (d *driver) GetPowerState(ctx context.Context) (bmc.PowerState, error) {
	out, err := d.ipmitool(ctx, "chassis", "power", "status")
	if err != nil {
		return bmc.PowerStateUnknown, fmt.Errorf("ipmi: reading power state: %w", err)
	}
	return parsePowerStatus(out), nil
}

// parsePowerStatus maps ipmitool's "chassis power status" output (for
// example "Chassis Power is on") to a PowerState. Output this driver does
// not recognize maps to PowerStateUnknown rather than being guessed at,
// per PowerState's doc comment.
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

// SetOneTimePXEBoot implements bmc.BMC. See this package's doc comment
// for why "chassis bootdev pxe" (with no options) is a one-time-only
// override.
func (d *driver) SetOneTimePXEBoot(ctx context.Context) error {
	if _, err := d.ipmitool(ctx, "chassis", "bootdev", "pxe"); err != nil {
		return fmt.Errorf("ipmi: setting one-time PXE boot: %w", err)
	}
	return nil
}
