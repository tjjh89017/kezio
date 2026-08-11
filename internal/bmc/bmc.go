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
// picks a driver by the scheme of Machine.spec.bmc.address (e.g.
// "redfish://..." or "ipmi://..."). Concrete drivers register themselves
// via init() (the database/sql pattern) without this package importing
// them back.
//
// Pure library: no Kubernetes types, no cluster API calls. Callers resolve
// CredentialsSecretRef into a Credentials value and pass the already-parsed
// address.
package bmc

import "context"

// PowerState is deliberately coarse: BMCs report transitional states too
// (e.g. Redfish's "PoweringOn"/"PoweringOff"), but a transitional or
// unrecognized state is reported as Unknown rather than guessed at, so a
// caller polling for a change never mistakes an in-flight transition for
// having landed.
type PowerState string

const (
	PowerStateOn      PowerState = "On"
	PowerStateOff     PowerState = "Off"
	PowerStateUnknown PowerState = "Unknown"
)

// BMC drives one machine's board management controller: power control and
// the one-time PXE boot override the deploy flow needs before each
// power-on. Bound to one machine for its lifetime; Connect returns one
// already resolved to the machine's BMC address.
type BMC interface {
	// PowerOn is idempotent.
	PowerOn(ctx context.Context) error
	// PowerOff is an orderly shutdown, not an abrupt cut of power. Idempotent.
	PowerOff(ctx context.Context) error
	// ForcePowerOff cuts power without asking anything on the machine to
	// shut down first, and is the escalation for a machine that cannot
	// answer PowerOff at all: a machine parked in its firmware setup or
	// boot menu runs no OS to handle the ACPI request, so PowerOff is
	// accepted by the BMC and then simply never acted on. Idempotent.
	//
	// It can lose in-flight disk writes on a machine running a deployed
	// OS, so a caller powering a machine down asks PowerOff first and only
	// escalates here once the graceful request is observed not to take
	// effect.
	ForcePowerOff(ctx context.Context) error
	// PowerCycle forces an immediate power-on reset without waiting for an
	// orderly shutdown - one BMC action, used to guarantee a clean boot
	// cycle even when the running OS is unresponsive.
	PowerCycle(ctx context.Context) error
	GetPowerState(ctx context.Context) (PowerState, error)
	// SetOneTimePXEBoot makes only the next boot net-boot, falling back to
	// the normal boot order afterward. Called before PowerOn/PowerCycle
	// whenever the machine needs to net boot the live environment.
	SetOneTimePXEBoot(ctx context.Context) error
}

// Credentials must never be logged or included verbatim in an error or
// debug output (e.g. a naive "%+v" would leak the password). A driver
// wrapping a lower-level client error must redact or omit this before
// returning any error referencing a connection; see Driver's doc comment.
type Credentials struct {
	Username string
	Password string
}
