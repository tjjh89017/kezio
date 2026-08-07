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

// Package ipmi implements internal/bmc.BMC over IPMI using the pure-Go
// client github.com/bougou/go-ipmi. Importing this package registers the
// "ipmi" scheme with internal/bmc's registry (see init) - same
// blank-import pattern as internal/bmc/redfish.
//
// # Library choice
//
// The other visible pure-Go option, github.com/vmware/goipmi, only
// natively implements the legacy "lan" interface (IPMI 1.5 RMCP); for
// "lanplus" (IPMI 2.0/RMCP+, what essentially every current BMC requires)
// it silently shells out to the ipmitool binary itself. bougou/go-ipmi
// implements IPMI 2.0/RMCP+ session establishment (RAKP, cipher suite
// negotiation) natively, so this driver needs no external binary and ships
// in the default manager image.
//
// # The ipmitool:// fallback
//
// internal/bmc/ipmitool provides an exec-ipmitool "ipmitool://" scheme as
// an operator-selectable escape hatch: IPMI's inconsistent vendor firmware
// means a specific BMC can misbehave against this pure-Go client while
// working against ipmitool's reference implementation. Use "ipmitool://"
// only when a BMC demonstrably needs it (requires the opt-in
// ipmitool-enabled manager image, see that package's doc comment);
// otherwise use "ipmi://".
//
// # Command mapping
//
// Every bmc.BMC method opens one IPMI 2.0/RMCP+ session (ADMINISTRATOR
// privilege, see dial), issues one command, then closes it - no session
// held across calls (same as ipmitool and redfish), since a controller
// connects briefly and disconnects often many times an hour and bmc.BMC
// has no Close hook to release one.
//
//   - PowerOn: Chassis Control, "power up".
//   - PowerOff: Chassis Control, "soft shutdown" - an ACPI request to the
//     running OS, not an immediate power cut (matches redfish's
//     GracefulShutdown and ipmitool's "chassis power soft" preference).
//   - PowerCycle: Chassis Control, "power cycle" (one BMC action),
//     mirroring redfish's ForceRestart. Per spec, only guaranteed to act
//     when the chassis is already on.
//   - GetPowerState: Get Chassis Status; PowerIsOn maps directly to
//     On/Off - IPMI has no transitional third state the way Redfish does,
//     so PowerStateUnknown here only means the read itself failed.
//   - SetOneTimePXEBoot: Set System Boot Options (section 28.12, Boot
//     Flags) with BootDeviceSelectorForcePXE + BIOSBootTypeEFI (ipmitool's
//     "options=efiboot") + persist=false, so the override applies to
//     exactly the next boot.
//
// # Privilege level and credentials
//
// Sessions open at ADMINISTRATOR privilege (see dial): chassis control and
// boot-option changes require it on most BMCs. address and creds are never
// logged or included verbatim in an error - see Credentials' doc comment
// in internal/bmc.
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

// driver implements bmc.BMC over IPMI for one host:port. It opens a fresh
// session (via dial) for every method call rather than holding one open
// across calls - see the package doc.
type driver struct {
	host  string
	port  int
	creds bmc.Credentials
	dial  dialFunc
}

// connect is the bmc.Driver registered for the "ipmi" scheme. Like
// ipmitool's connect (and unlike redfish's), it does not itself talk to
// the BMC - any unreachable-host or bad-credentials error surfaces from
// the first method call instead of from Connect.
func connect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	host, port, err := hostPort(address)
	if err != nil {
		return nil, err
	}
	return &driver{host: host, port: port, creds: creds, dial: dial}, nil
}

// hostPort extracts host and optional port (defaulting to defaultPort).
// address must carry only host[:port], no path: an IPMI target isn't a
// resource tree the way a Redfish System is.
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

// withSession opens a session, runs fn, and always closes it. The Close
// error is discarded: fn's own error is what a caller needs to see, and a
// session that failed to tear down after a completed operation isn't
// actionable.
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

// PowerOff implements bmc.BMC using a "soft shutdown" chassis control
// (ACPI request to the OS), not a hard power-down - see the package doc.
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

// powerState maps IPMI's "power is on" flag to a PowerState. IPMI has no
// transitional third state (unlike Redfish's "PoweringOn"), so this never
// itself returns PowerStateUnknown - only a failed read does (GetPowerState).
func powerState(on bool) bmc.PowerState {
	if on {
		return bmc.PowerStateOn
	}
	return bmc.PowerStateOff
}

// SetOneTimePXEBoot implements bmc.BMC. See the package doc for why
// BootDeviceSelectorForcePXE + BIOSBootTypeEFI + persist=false is a
// one-time EFI PXE boot override.
func (d *driver) SetOneTimePXEBoot(ctx context.Context) error {
	err := d.withSession(ctx, func(s session) error {
		return s.SetBootDevice(ctx, ipmilib.BootDeviceSelectorForcePXE, ipmilib.BIOSBootTypeEFI, false)
	})
	if err != nil {
		return fmt.Errorf("ipmi: setting one-time PXE boot: %w", err)
	}
	return nil
}
