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

// Package deployer defines the small interface the Machine reconciler uses
// to drive a machine through the phases of its state machine (enroll,
// inspect, provision, deprovision, power on/off), and a fake implementation
// of it for tests and for the CI walk-through that does not need real
// hardware.
package deployer

import (
	"context"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Result reports the outcome of one Deployer phase call. The reconciler
// uses it to decide whether to persist a status change, when to call the
// phase again, and whether to record a machine-level error.
type Result struct {
	// Dirty is true when the call produced information the reconciler
	// should persist to Machine.status before deciding what happens next.
	Dirty bool
	// RequeueAfter is how long to wait before calling this phase again.
	// Zero means the phase is complete; a non-zero value asks for another
	// poll. Provision is the phase that typically uses this, since it is
	// idempotent and polled until the deployment finishes.
	RequeueAfter time.Duration
	// ErrorMessage is set when the phase itself failed (for example, the
	// BMC rejected the power command, or the deployment plan could not be
	// applied). The reconciler records it on Machine.status.errorMessage,
	// moves the machine to the Error state, and retries with backoff. An
	// empty ErrorMessage means the call succeeded; a non-nil Go error
	// returned alongside it signals an unrelated, transient problem (for
	// example a context cancellation) that the reconciler should just
	// requeue without recording a machine-level error.
	ErrorMessage string
}

// RegisterData carries what the enroll phase needs to establish contact
// with a newly added machine, for example powering it on and preparing it
// for the network boot that inspection needs.
type RegisterData struct {
	// BootMACAddress is the MAC address of the NIC the machine network
	// boots from.
	BootMACAddress string
	// BMC identifies the board management controller that powers and
	// boot-orders the machine. Nil means the machine has no BMC
	// configured.
	BMC *keziov1alpha1.MachineBMC
}

// InspectData carries what the inspect phase needs, and receives the
// hardware inventory the phase collects. Hardware is nil on input; a
// successful call fills it in.
type InspectData struct {
	// Hardware receives the collected inventory on success.
	Hardware *keziov1alpha1.MachineHardwareStatus
}

// ProvisionData carries the resolved deployment plan for the provision
// phase: the OS image, any extra data images, and where each one writes.
// ResolvedTargetDisk and ResolvedDataImageDisks are output fields, nil/empty
// on input; a call that completes the deployment (Result.RequeueAfter == 0,
// no error) fills them in, the same way InspectData.Hardware is filled in.
type ProvisionData struct {
	// ImageRef names the OS image to deploy. Absent when the machine
	// deploys only DataImages.
	ImageRef *keziov1alpha1.NameRef
	// TargetDisk selects the disk the OS image deploys to.
	TargetDisk *keziov1alpha1.TargetDiskHints
	// DataImages are the additional, non-OS images deployed alongside (or
	// instead of) the OS image in the same session.
	DataImages []keziov1alpha1.MachineDataImage
	// Ezio carries the resolved EZIO tuning values for this machine's
	// leecher.
	Ezio *keziov1alpha1.MachineEzioTuning
	// ResolvedTargetDisk receives the device path the OS image was
	// actually written to, once the deployment completes. Empty when
	// ImageRef is absent.
	ResolvedTargetDisk string
	// ResolvedDataImageDisks receives the device path each DataImages
	// entry was actually written to, once the deployment completes, in
	// the same order as DataImages.
	ResolvedDataImageDisks []string
}

// DeprovisionData carries what the deprovision phase needs to release a
// deployed machine, for example while handling Machine deletion.
type DeprovisionData struct{}

// Deployer drives one machine through the phases of the state machine. A
// Factory builds one Deployer per machine, so the phase methods do not
// need to repeat the machine's identity.
type Deployer interface {
	// Register enrolls the machine and prepares it for inspection. It
	// drives the Enrolling -> Inspecting transition.
	Register(ctx context.Context, data *RegisterData) (Result, error)
	// Inspect collects hardware inventory. On success it fills in
	// data.Hardware. It drives the Inspecting -> Available transition.
	Inspect(ctx context.Context, data *InspectData) (Result, error)
	// Provision deploys the requested image(s) to the machine. It is
	// idempotent and polled: repeated calls with the same data continue or
	// confirm an in-progress deployment, until Result.RequeueAfter is
	// zero. It drives the Provisioning -> Provisioned transition.
	Provision(ctx context.Context, data *ProvisionData) (Result, error)
	// Deprovision releases a deployed machine, undoing Provision.
	Deprovision(ctx context.Context, data *DeprovisionData) (Result, error)
	// PowerOn powers the machine on.
	PowerOn(ctx context.Context) (Result, error)
	// PowerOff powers the machine off.
	PowerOff(ctx context.Context) (Result, error)
}

// Factory builds a Deployer scoped to one Machine. The reconciler calls it
// once per reconcile, mirroring how it already resolves the Machine
// object, and uses the returned Deployer for that reconcile's phase call.
type Factory func(machine *keziov1alpha1.Machine) (Deployer, error)
