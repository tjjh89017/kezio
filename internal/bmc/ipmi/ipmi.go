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

// Package ipmi implements internal/bmc.BMC over IPMI, using the pure-Go
// client github.com/bougou/go-ipmi. Importing this package registers the
// "ipmi" scheme with internal/bmc's registry (see this file's init) as a
// side effect - the same import-for-side-effect pattern internal/bmc/redfish
// uses - so a caller only needs a blank import
// (`_ "github.com/tjjh89017/kezio/internal/bmc/ipmi"`) to make bmc.Connect
// resolve that scheme.
//
// # Library choice: bougou/go-ipmi, not an exec-ipmitool shim
//
// An earlier version of this driver shelled out to the "ipmitool" binary,
// because the most visible pure-Go alternative at the time,
// github.com/vmware/goipmi, was a poor fit: its native, dependency-free
// transport only implements the "lan" interface (IPMI 1.5 RMCP, MD5/plain
// password authentication) - the interface field it exposes for "lanplus"
// (IPMI 2.0 RMCP+, the interface effectively every BMC shipped in the last
// decade requires and defaults to) is not natively implemented at all; it
// silently shells out to the ipmitool binary itself. That evaluation still
// stands: goipmi remains the wrong choice for this driver.
//
// github.com/bougou/go-ipmi is different: it implements IPMI 2.0/RMCP+
// session establishment (RAKP, cipher suite negotiation) natively in Go,
// with no external binary in its call path, and is an actively maintained
// project tracking current IPMI cipher-suite work. That closes the gap
// goipmi left open, so this driver now talks IPMI directly instead of
// through a subprocess: no binary needs to ship in the manager's container
// image, and every kezio deployment gets ipmi:// support from the default
// image rather than only from an opt-in variant.
//
// # The ipmitool:// fallback
//
// internal/bmc/ipmitool keeps the previous exec-ipmitool implementation
// alive under a separate "ipmitool://" scheme. It exists as an
// operator-selectable escape hatch: IPMI's long history of inconsistent
// vendor firmware means a specific BMC can misbehave against this pure-Go
// client while working fine against ipmitool's battle-tested reference
// implementation. Reach for "ipmitool://" only when a BMC demonstrably
// needs it (and after building/deploying the opt-in ipmitool-enabled
// manager image it requires, see internal/bmc/ipmitool's doc comment);
// every other IPMI Machine should use "ipmi://".
//
// # Command mapping
//
// Every bmc.BMC method opens one IPMI 2.0/RMCP+ session (see dial),
// authenticated at the ADMINISTRATOR privilege level, issues exactly one
// IPMI command over it, then closes the session - mirroring both
// internal/bmc/ipmitool (which opens and tears down its own session on
// every ipmitool invocation) and internal/bmc/redfish's rationale for not
// holding a session open across calls (see that package's connect doc
// comment): a kezio controller connects briefly, does one action, and
// disconnects, often many times an hour across reconciles, and holding a
// session open here with no Close hook on the bmc.BMC interface itself
// would leak it.
//
//   - PowerOn: Chassis Control, "power up".
//   - PowerOff: Chassis Control, "soft shutdown" - an ACPI soft-shutdown
//     request to the running OS, not an immediate power cut (the wrong
//     default for the same reason internal/bmc/redfish prefers
//     GracefulShutdown over ForceOff, and internal/bmc/ipmitool prefers
//     "chassis power soft" over "chassis power off").
//   - PowerCycle: Chassis Control, "power cycle" (power off, pause, power
//     back on as one BMC action), mirroring redfish's ForceRestart
//     semantics. Per the IPMI spec this is only guaranteed to act when the
//     chassis is already on; a BMC that rejects it while already off is
//     IPMI protocol behavior this driver does not work around.
//   - GetPowerState: Get Chassis Status, whose PowerIsOn field maps
//     directly to PowerStateOn/PowerStateOff - IPMI's chassis status has no
//     transitional third state the way Redfish does, so this driver never
//     needs to report PowerStateUnknown except when the read itself fails.
//   - SetOneTimePXEBoot: Set System Boot Options (IPMI section 28.12,
//     parameter 5, Boot Flags) requesting BootDeviceSelectorForcePXE with
//     BIOSBootTypeEFI (the equivalent of ipmitool's
//     "options=efiboot") and persist=false - which sets the "boot flags
//     valid" bit for exactly the next boot and leaves the persistent bit
//     clear, so the override does not survive past that one boot.
//
// # Privilege level and credentials
//
// Every session is opened at the ADMINISTRATOR privilege level (see dial):
// chassis control and boot-option changes require it on most BMCs, and a
// lower privilege level would make some of these operations fail
// unpredictably depending on the BMC's configured privilege ceiling for the
// account.
//
// address and creds are never logged or included verbatim in an error:
// bougou/go-ipmi's own errors do not embed the password (verified against
// its source), and this driver's own wrapping never formats Credentials
// directly - see Credentials' doc comment in internal/bmc.
package ipmi

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	ipmilib "github.com/bougou/go-ipmi"

	"github.com/tjjh89017/kezio/internal/bmc"
)

func init() {
	bmc.Register("ipmi", connect)
}

// defaultPort is IPMI's well-known UDP port, used when address carries
// none.
const defaultPort = 623

// driver implements bmc.BMC over IPMI for one host:port, authenticating
// with one Credentials pair. It opens a fresh session (via dial) for every
// bmc.BMC method call rather than holding one open across calls - see this
// package's doc comment for why.
type driver struct {
	host  string
	port  int
	creds bmc.Credentials
	dial  dialFunc
}

// connect is the bmc.Driver registered for the "ipmi" scheme. Like
// internal/bmc/ipmitool's connect (and unlike internal/bmc/redfish's), this
// does not itself talk to the BMC: every method call opens and closes its
// own session (see this package's doc comment), so any unreachable-host or
// bad-credentials error surfaces from the first method call instead of from
// Connect.
func connect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	host, port, err := hostPort(address)
	if err != nil {
		return nil, err
	}
	return &driver{host: host, port: port, creds: creds, dial: dial}, nil
}

// hostPort extracts the BMC's host and optional port from address,
// defaulting the port to defaultPort. address must carry only a
// host[:port] (and no path): an IPMI target is not a resource tree the way
// a Redfish System is, so there is nothing else in the address for this
// driver to interpret.
func hostPort(address *url.URL) (string, int, error) {
	host := address.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("ipmi: address %s has no host", address.Redacted())
	}
	if path := strings.Trim(address.Path, "/"); path != "" {
		return "", 0, fmt.Errorf("ipmi: address %s must not include a path", address.Redacted())
	}

	port := defaultPort
	if p := address.Port(); p != "" {
		parsed, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Errorf("ipmi: address %s has an invalid port: %w", address.Redacted(), err)
		}
		port = parsed
	}
	return host, port, nil
}

// withSession opens a session via d.dial, runs fn against it, and closes it
// afterward regardless of fn's outcome. The session's Close error is
// intentionally discarded: fn's own error (a completed power/boot
// operation that failed) is what a caller needs to see, and a session that
// merely failed to tear down cleanly after a completed operation is not
// something a caller can act on.
func (d *driver) withSession(ctx context.Context, fn func(session) error) error {
	s, err := d.dial(ctx, d.host, d.port, d.creds)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close(ctx) }()
	return fn(s)
}

// PowerOn implements bmc.BMC.
func (d *driver) PowerOn(ctx context.Context) error {
	err := d.withSession(ctx, func(s session) error {
		_, err := s.ChassisControl(ctx, ipmilib.ChassisControlPowerUp)
		return err
	})
	if err != nil {
		return fmt.Errorf("ipmi: power on: %w", err)
	}
	return nil
}

// PowerOff implements bmc.BMC. It issues a "soft shutdown" chassis control
// (an ACPI soft-shutdown request to the running OS) rather than a hard
// power-down: see this package's doc comment for the rationale, which
// mirrors internal/bmc/redfish's and internal/bmc/ipmitool's PowerOff.
func (d *driver) PowerOff(ctx context.Context) error {
	err := d.withSession(ctx, func(s session) error {
		_, err := s.ChassisControl(ctx, ipmilib.ChassisControlSoftShutdown)
		return err
	})
	if err != nil {
		return fmt.Errorf("ipmi: power off: %w", err)
	}
	return nil
}

// PowerCycle implements bmc.BMC.
func (d *driver) PowerCycle(ctx context.Context) error {
	err := d.withSession(ctx, func(s session) error {
		_, err := s.ChassisControl(ctx, ipmilib.ChassisControlPowerCycle)
		return err
	})
	if err != nil {
		return fmt.Errorf("ipmi: power cycle: %w", err)
	}
	return nil
}

// GetPowerState implements bmc.BMC.
func (d *driver) GetPowerState(ctx context.Context) (bmc.PowerState, error) {
	state := bmc.PowerStateUnknown
	err := d.withSession(ctx, func(s session) error {
		status, err := s.GetChassisStatus(ctx)
		if err != nil {
			return err
		}
		state = powerState(status.PowerIsOn)
		return nil
	})
	if err != nil {
		return bmc.PowerStateUnknown, fmt.Errorf("ipmi: reading power state: %w", err)
	}
	return state, nil
}

// powerState maps IPMI's Get Chassis Status "power is on" flag to a
// PowerState. IPMI's chassis status is always definitively on or off (no
// transitional third state the way Redfish reports "PoweringOn"), so this
// mapping is total and never itself needs to return PowerStateUnknown - the
// only source of PowerStateUnknown for this driver is a failed read (see
// GetPowerState).
func powerState(on bool) bmc.PowerState {
	if on {
		return bmc.PowerStateOn
	}
	return bmc.PowerStateOff
}

// SetOneTimePXEBoot implements bmc.BMC. See this package's doc comment for
// why BootDeviceSelectorForcePXE + BIOSBootTypeEFI + persist=false is a
// one-time-only, EFI-compatible PXE boot override.
func (d *driver) SetOneTimePXEBoot(ctx context.Context) error {
	err := d.withSession(ctx, func(s session) error {
		return s.SetBootDevice(ctx, ipmilib.BootDeviceSelectorForcePXE, ipmilib.BIOSBootTypeEFI, false)
	})
	if err != nil {
		return fmt.Errorf("ipmi: setting one-time PXE boot: %w", err)
	}
	return nil
}
