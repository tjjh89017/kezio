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

// Package bmc defines the driver interface kezio uses to power-manage and
// boot-order a Machine's board management controller, and a registry that
// picks a driver by the scheme of Machine.spec.bmc.address (for example
// "redfish://..." or "ipmi://..."). Concrete drivers (internal/bmc/redfish,
// and later an IPMI driver) register themselves against this package
// without it importing them back, the same pattern database/sql uses for
// its drivers.
//
// This package is a pure library: it holds no reference to any
// Kubernetes type and makes no cluster API calls. The caller resolves the
// Machine's CredentialsSecretRef into a Credentials value and passes the
// already-parsed address; wiring this into the deployer's state machine is
// separate work.
package bmc

import "context"

// PowerState is a coarse power state read back from a BMC. It intentionally
// has only three values: BMCs report additional transitional states (for
// example Redfish's "PoweringOn"/"PoweringOff"), but the deploy flow only
// ever needs to know "is it on", "is it off", or "the BMC didn't say" - a
// transitional or unrecognized state is reported as Unknown rather than
// guessed at, so a caller polling for a state change never mistakes an
// in-flight transition for having already landed.
type PowerState string

const (
	// PowerStateOn means the machine is powered on.
	PowerStateOn PowerState = "On"
	// PowerStateOff means the machine is powered off.
	PowerStateOff PowerState = "Off"
	// PowerStateUnknown means the BMC reported a state this package does
	// not distinguish (a transitional state, or one it does not
	// recognize), or the state could not be determined.
	PowerStateUnknown PowerState = "Unknown"
)

// BMC drives one machine's board management controller: power control and
// the one-time PXE boot override the deploy flow's net-boot cycle needs
// before each power-on. A BMC value is bound to one machine for its
// lifetime; Connect returns one already resolved to the machine's BMC
// address.
type BMC interface {
	// PowerOn powers the machine on. It is idempotent: calling it on an
	// already-powered-on machine succeeds.
	PowerOn(ctx context.Context) error
	// PowerOff powers the machine off gracefully (an orderly shutdown of
	// whatever OS is running, not an abrupt cut of power). It is
	// idempotent: calling it on an already-powered-off machine succeeds.
	PowerOff(ctx context.Context) error
	// PowerCycle forces an immediate power-on reset, without waiting for
	// an orderly shutdown. Unlike PowerOff followed by PowerOn, this is
	// one BMC action: it is what the deploy flow uses to guarantee a
	// clean boot cycle even when the running OS (if any) is unresponsive.
	PowerCycle(ctx context.Context) error
	// GetPowerState reads the machine's current power state.
	GetPowerState(ctx context.Context) (PowerState, error)
	// SetOneTimePXEBoot arranges for the machine's next boot only to
	// network boot, then fall back to its normal boot order afterward.
	// The deploy flow calls this before PowerOn (or PowerCycle) whenever
	// it needs the machine to net boot the live environment.
	SetOneTimePXEBoot(ctx context.Context) error
}

// Credentials is the username/password pair a driver authenticates to a
// BMC with. The caller resolves Machine.spec.bmc.credentialsSecretRef into
// this shape (see CredentialsFromSecretData); this package never reads a
// Kubernetes Secret itself.
//
// It must never be logged or included verbatim in an error or debug output
// - a driver that formats this value (for example via a naive "%+v") would
// leak the password. A driver wrapping a lower-level client error must
// redact or omit this value before returning any error that references a
// connection; see Driver's doc comment.
type Credentials struct {
	Username string
	Password string
}
