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

// Package deployer defines the contract the Machine reconciler uses to
// drive one machine through hardware inspection and image provisioning,
// independent of any particular hardware backend.
package deployer

import (
	"context"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// Outcome is the result of one Deployer step call that returned without a
// transient error. The zero value is deliberately invalid (not Complete):
// a caller that forgets to set Result.Outcome must not silently behave as
// if the step succeeded.
type Outcome int

const (
	// The zero value is intentionally unnamed and unused as a valid
	// Outcome - see the Outcome doc comment.
	_ Outcome = iota
	// Complete means the step's goal is reached; the caller advances the
	// Machine to the next state.
	Complete
	// Continuing means the step made progress but is not done; the caller
	// requeues promptly to keep driving it.
	Continuing
	// Busy means the step is blocked on something outside the deployer's
	// control (for example a lock another attempt holds); the caller
	// requeues after Result.RequeueAfter without treating this as an
	// error or touching Machine error state.
	Busy
	// Failed means the step itself determined the machine cannot
	// proceed - not a transient infrastructure hiccup. The caller records
	// Result.ErrorType/ErrorMessage on the Machine and applies backoff.
	Failed
	// Delayed means the step cannot make progress yet for a reason that is
	// not an error (for example an unresolved reference, or an image not
	// ready to serve): the caller requeues after its own fixed short
	// interval and must not touch Machine error state.
	Delayed
)

// String renders o for logs and events. An unrecognized value (including
// the invalid zero value) renders as "Unknown" rather than a bare number.
func (o Outcome) String() string {
	switch o {
	case Complete:
		return "Complete"
	case Continuing:
		return "Continuing"
	case Busy:
		return "Busy"
	case Failed:
		return "Failed"
	case Delayed:
		return "Delayed"
	default:
		return "Unknown"
	}
}

// Result is returned by a successful (non-transient) Deployer step call.
type Result struct {
	Outcome Outcome
	// RequeueAfter is the delay before the next reconcile. Meaningful only
	// when Outcome is Busy; zero means the caller applies its own default
	// backoff.
	RequeueAfter time.Duration
	// ErrorType and ErrorMessage are set only when Outcome is Failed, and
	// map directly onto Machine.status.errorType/errorMessage.
	ErrorType    keziov1alpha2.MachineErrorType
	ErrorMessage string
}

// Deployer drives one Machine through hardware inspection and image
// provisioning. Every step method returns exactly one of two shapes:
//
//   - (Result, nil): the step ran to a defined Outcome (Complete,
//     Continuing, Busy, or Failed). The caller acts on Result and must not
//     treat this as a reconcile error.
//   - (Result{}, err): a transient failure (for example a network blip
//     talking to the BMC or the agent). The caller returns err from
//     Reconcile unchanged for controller-runtime's standard
//     requeue-with-error handling, and must not change Machine's
//     errorType/errorCount for it - transient failures carry no status
//     change by design.
//
// Implementations must be safe to call repeatedly for the same Machine
// across reconciles: the caller keeps no per-attempt deployer state
// between calls.
type Deployer interface {
	// Inspect drives one step of hardware inspection for machine. On
	// Complete, a MachineHardware object named after machine exists and
	// carries the discovered inventory. restartOnFailure is true when the
	// prior attempt at this step recorded
	// Machine.status.errorType == MachineErrorTypeRestart: the
	// implementation must discard any in-progress inspection state and
	// start over rather than resume it.
	Inspect(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error)

	// Provision drives one step of deploying run to machine. On Complete,
	// run finished successfully and kezio-agent has already committed to
	// the after-deploy action (internal/agent/deploy's systemctl reboot
	// or poweroff) as its last act before handing the machine back -
	// booting the deployed image, or for a dataImages-only run reaching
	// its after-deploy power state, races that observation and is not
	// separately confirmed here: the terminal success report lands
	// before that command runs, since nothing sent after it is
	// guaranteed to land. A reboot falls through to the disk (the
	// one-time PXE override armed to boot the live agent was already
	// consumed getting here) rather than net-booting the agent again.
	// restartOnFailure carries the same meaning as in Inspect, scoped to
	// this Provision step: the implementation discards this attempt's
	// in-progress state - including any progress already recorded on run
	// itself - and starts the step over.
	Provision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error)

	// Deprovision drives one step of tearing down machine's deployed
	// state as the first step of the delete walk. On Complete, machine
	// carries no deployer-managed state that must survive the Machine
	// object's removal. restartOnFailure carries the same meaning as in
	// Inspect, scoped to this Deprovision step.
	Deprovision(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error)

	// PowerOff drives one step of powering machine off, the delete walk's
	// last live action before the Machine object is released. On
	// Complete, the machine is (or is believed to be) powered off.
	PowerOff(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error)

	// Reboot drives one step of rebooting machine in response to a reboot
	// annotation. hard selects a hard (forced) reboot over a soft
	// (graceful) one. On Complete, the machine has been commanded to
	// reboot; this does not wait for the machine to finish booting.
	Reboot(ctx context.Context, machine *keziov1alpha2.Machine, hard bool) (Result, error)
}
