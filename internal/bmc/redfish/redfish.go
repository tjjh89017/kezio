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

// Package redfish implements internal/bmc.BMC over the Redfish protocol,
// using github.com/stmcginnis/gofish. Importing this package registers the
// "redfish", "redfishs", and "redfish+http" schemes with internal/bmc's
// registry (see this file's init) as a side effect - the same
// import-for-side-effect pattern database/sql drivers use - so a caller
// only needs a blank import (`_ "github.com/tjjh89017/kezio/internal/bmc/redfish"`)
// to make bmc.Connect resolve those schemes.
package redfish

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/tjjh89017/kezio/internal/bmc"
)

func init() {
	bmc.Register("redfish", connect)
	bmc.Register("redfishs", connect)
	bmc.Register("redfish+http", connect)
}

// driver implements bmc.BMC over one Redfish ComputerSystem.
type driver struct {
	client    *gofish.APIClient
	system    *schemas.ComputerSystem
	systemURI string
}

// connect is the bmc.Driver registered for the Redfish schemes. address's
// host selects the Redfish service endpoint; address's scheme selects
// plain HTTP vs HTTPS (see endpointFor); address's path optionally selects
// which System to bind to, when the BMC exposes more than one (see
// resolveSystem).
func connect(ctx context.Context, address *url.URL, creds bmc.Credentials, opts bmc.Options) (bmc.BMC, error) {
	endpoint, err := endpointFor(address)
	if err != nil {
		return nil, err
	}

	client, err := gofish.ConnectContext(ctx, gofish.ClientConfig{
		Endpoint: endpoint,
		Username: creds.Username,
		Password: creds.Password,
		Insecure: opts.InsecureSkipVerify,
		// BasicAuth avoids creating a server-side session: a controller
		// connects briefly, often many times an hour, and most BMCs cap
		// concurrent Redfish sessions in the single digits.
		BasicAuth: true,
	})
	if err != nil {
		return nil, fmt.Errorf("redfish: %w", err)
	}

	system, err := resolveSystem(client, address.Path)
	if err != nil {
		return nil, err
	}

	return &driver{client: client, system: system, systemURI: system.ODataID}, nil
}

// endpointFor builds the Redfish service root endpoint (scheme and host
// only - the path is handled separately by resolveSystem) from address.
// The "redfish+http" scheme is the one deliberate exception to defaulting
// to HTTPS: it exists for lab/test Redfish services (including this
// package's own tests) reachable only over plain HTTP, and a caller must
// opt into it explicitly by using that scheme rather than "redfish".
func endpointFor(address *url.URL) (string, error) {
	if address.Host == "" {
		return "", fmt.Errorf("redfish: address has no host")
	}

	scheme := "https"
	if strings.EqualFold(address.Scheme, "redfish+http") {
		scheme = "http"
	}

	endpoint := url.URL{Scheme: scheme, Host: address.Host}
	return endpoint.String(), nil
}

// resolveSystem finds the ComputerSystem the driver binds to. When path
// names a specific System resource (contains "/Systems/<id>"), that exact
// resource is fetched. Otherwise the driver lists the Systems collection
// and requires it to contain exactly one member - a BMC managing more
// than one System (for example a chassis manager) must be addressed with
// an explicit path, since there is no other way to tell which System a
// Machine means.
func resolveSystem(client *gofish.APIClient, path string) (*schemas.ComputerSystem, error) {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.Index(path, "/Systems/"); idx >= 0 && len(path) > idx+len("/Systems/") {
		system, err := schemas.GetComputerSystem(client, path)
		if err != nil {
			return nil, fmt.Errorf("redfish: resolving system %q: %w", path, err)
		}
		return system, nil
	}

	systems, err := client.Service.Systems()
	if err != nil {
		return nil, fmt.Errorf("redfish: listing systems: %w", err)
	}
	switch len(systems) {
	case 0:
		return nil, fmt.Errorf("redfish: no systems found")
	case 1:
		return systems[0], nil
	default:
		return nil, fmt.Errorf("redfish: found %d systems, address path must select one (e.g. .../Systems/<id>)", len(systems))
	}
}

// PowerOn implements bmc.BMC.
func (d *driver) PowerOn(_ context.Context) error {
	return d.reset(schemas.OnResetType)
}

// PowerOff implements bmc.BMC using GracefulShutdown rather than ForceOff,
// to avoid losing in-flight disk writes on a Machine already running a
// deployed OS.
func (d *driver) PowerOff(_ context.Context) error {
	return d.reset(schemas.GracefulShutdownResetType)
}

// ForcePowerOff implements bmc.BMC using ForceOff - the same
// ComputerSystem.Reset action, with the ResetType that cuts power
// immediately instead of asking the OS to shut down. This is what
// actually turns off a machine sitting in its firmware boot menu, where
// GracefulShutdown is accepted and then ignored because no OS is running
// to receive the ACPI request.
func (d *driver) ForcePowerOff(_ context.Context) error {
	return d.reset(schemas.ForceOffResetType)
}

// PowerCycle implements bmc.BMC using ForceRestart, an immediate
// power-on reset independent of a running OS - used when a guaranteed
// boot cycle is needed (e.g. to pick up a freshly set one-time PXE boot).
func (d *driver) PowerCycle(_ context.Context) error {
	return d.reset(schemas.ForceRestartResetType)
}

func (d *driver) reset(resetType schemas.ResetType) error {
	if _, err := d.system.Reset(resetType); err != nil {
		return fmt.Errorf("redfish: reset %s: %w", resetType, err)
	}
	return nil
}

// GetPowerState implements bmc.BMC, re-fetching the System resource
// (rather than reusing the cached one from connect time) so a caller
// polling for a state change sees the BMC's current state.
func (d *driver) GetPowerState(_ context.Context) (bmc.PowerState, error) {
	system, err := schemas.GetComputerSystem(d.client, d.systemURI)
	if err != nil {
		return bmc.PowerStateUnknown, fmt.Errorf("redfish: reading power state: %w", err)
	}

	switch system.PowerState {
	case schemas.OnPowerState:
		return bmc.PowerStateOn, nil
	case schemas.OffPowerState:
		return bmc.PowerStateOff, nil
	default:
		return bmc.PowerStateUnknown, nil
	}
}

// SetOneTimePXEBoot implements bmc.BMC by setting the System's boot
// override to PXE for exactly the next boot - the same
// BootSourceOverrideTarget=Pxe/BootSourceOverrideEnabled=Once call
// KubeVirtBMC's Redfish endpoint answers, so this one code path drives
// both a real server's BMC and the e2e test's simulated one.
func (d *driver) SetOneTimePXEBoot(_ context.Context) error {
	boot := &schemas.Boot{
		BootSourceOverrideTarget:  schemas.PxeBootSource,
		BootSourceOverrideEnabled: schemas.OnceBootSourceOverrideEnabled,
	}
	if err := d.system.SetBoot(boot); err != nil {
		return fmt.Errorf("redfish: setting one-time PXE boot: %w", err)
	}
	return nil
}
