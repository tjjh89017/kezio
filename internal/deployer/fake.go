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

package deployer

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Phase names passed to FailFunc, identifying which Deployer method is
// about to run.
type Phase string

// The phases a FailFunc can target.
const (
	PhaseRegister      Phase = "Register"
	PhaseInspect       Phase = "Inspect"
	PhaseProvision     Phase = "Provision"
	PhaseDeprovision   Phase = "Deprovision"
	PhasePowerOn       Phase = "PowerOn"
	PhasePowerOff      Phase = "PowerOff"
	PhaseForcePowerOff Phase = "ForcePowerOff"
	PhasePowerCycle    Phase = "PowerCycle"
)

// FailFunc decides whether a phase call should fail. Returning a non-empty
// string injects that string as the call's Result.ErrorMessage; an empty
// string lets the call succeed normally. It is consulted with the
// machine's namespaced name and the phase about to run, so a test can
// target one machine, one phase, or both.
type FailFunc func(machine types.NamespacedName, phase Phase) string

// machineState is the in-memory state the fake keeps per machine, across
// the separate Deployer instances FakeFactory.New builds for it over
// successive reconciles.
type machineState struct {
	poweredOn bool
	bootMAC   string
}

// FakeFactory builds fake Deployers that share per-machine state and an
// optional FailFunc, so a test can walk a Machine through the whole state
// machine without a real deployment backend. The zero value is not usable;
// build one with NewFactory.
type FakeFactory struct {
	// Fail, when set, is consulted before every phase call to optionally
	// inject an error. Nil means no injected errors.
	Fail FailFunc

	mu    sync.Mutex
	state map[types.NamespacedName]*machineState
}

// NewFactory returns a ready-to-use FakeFactory.
func NewFactory() *FakeFactory {
	return &FakeFactory{state: make(map[types.NamespacedName]*machineState)}
}

// New implements the deployer.FakeFactory function type, returning a fake
// Deployer scoped to the given machine. It reuses the machine's state
// across calls, keyed by namespace and name, so repeated reconciles see a
// consistent, progressing machine.
func (f *FakeFactory) New(machine *keziov1alpha1.Machine) (Deployer, error) {
	key := types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}

	f.mu.Lock()
	st, ok := f.state[key]
	if !ok {
		st = &machineState{}
		f.state[key] = st
	}
	f.mu.Unlock()

	return &fakeDeployer{key: key, state: st, factory: f}, nil
}

// fakeDeployer is one machine's view of a FakeFactory: it reports plausible
// results for each phase, optionally injecting an error through the
// owning FakeFactory's FailFunc.
type fakeDeployer struct {
	key     types.NamespacedName
	state   *machineState
	factory *FakeFactory
}

func (d *fakeDeployer) fail(phase Phase) string {
	if d.factory.Fail == nil {
		return ""
	}
	return d.factory.Fail(d.key, phase)
}

// Register enrolls the machine. The fake has nothing to set up beyond
// recording the boot MAC address, which Inspect later reuses for the fake
// NIC inventory.
func (d *fakeDeployer) Register(_ context.Context, data *RegisterData) (Result, error) {
	if msg := d.fail(PhaseRegister); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	d.state.bootMAC = data.BootMACAddress
	d.factory.mu.Unlock()

	return Result{Dirty: true}, nil
}

// Inspect fills data.Hardware with plausible fake hardware inventory, so a
// Machine driven by the fake reaches Available with a populated
// status.hardware, the same shape a real agent report would produce.
func (d *fakeDeployer) Inspect(_ context.Context, data *InspectData) (Result, error) {
	if msg := d.fail(PhaseInspect); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	bootMAC := d.state.bootMAC
	d.factory.mu.Unlock()

	data.Hardware = fakeHardware(d.key.Name, bootMAC)
	return Result{Dirty: true}, nil
}

// Provision reports a completed deployment on the first call (no image
// content to move, so no need to poll), filling resolved-disk fields with
// plausible fake device paths.
func (d *fakeDeployer) Provision(_ context.Context, data *ProvisionData) (Result, error) {
	if msg := d.fail(PhaseProvision); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	if data.ImageRef != nil {
		data.ResolvedTargetDisk = "/dev/vda"
	}
	if len(data.DataImages) > 0 {
		data.ResolvedDataImageDisks = make([]string, len(data.DataImages))
		for i := range data.DataImages {
			data.ResolvedDataImageDisks[i] = fmt.Sprintf("/dev/vd%c", 'b'+i)
		}
	}

	return Result{Dirty: true}, nil
}

// Deprovision releases the machine.
func (d *fakeDeployer) Deprovision(_ context.Context, _ *DeprovisionData) (Result, error) {
	if msg := d.fail(PhaseDeprovision); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}
	return Result{Dirty: true}, nil
}

// PowerOn powers the fake machine on.
func (d *fakeDeployer) PowerOn(_ context.Context) (Result, error) {
	if msg := d.fail(PhasePowerOn); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	d.state.poweredOn = true
	d.factory.mu.Unlock()

	return Result{Dirty: true}, nil
}

// PowerOff powers the fake machine off.
func (d *fakeDeployer) PowerOff(_ context.Context) (Result, error) {
	if msg := d.fail(PhasePowerOff); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	d.state.poweredOn = false
	d.factory.mu.Unlock()

	return Result{Dirty: true}, nil
}

// ForcePowerOff powers the fake machine off. The fake models no OS and so
// no shutdown that could be ignored, which is exactly why it lands on the
// same state PowerOff does: what separates the two here is which phase a
// test observes being called, not a different outcome.
func (d *fakeDeployer) ForcePowerOff(_ context.Context) (Result, error) {
	if msg := d.fail(PhaseForcePowerOff); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	d.state.poweredOn = false
	d.factory.mu.Unlock()

	return Result{Dirty: true}, nil
}

// PowerCycle power-cycles the fake machine, landing it powered on -
// the same observable end state as PowerOn, since the fake has no
// distinct "cycling" transition to model.
func (d *fakeDeployer) PowerCycle(_ context.Context) (Result, error) {
	if msg := d.fail(PhasePowerCycle); msg != "" {
		return Result{ErrorMessage: msg}, nil
	}

	d.factory.mu.Lock()
	d.state.poweredOn = true
	d.factory.mu.Unlock()

	return Result{Dirty: true}, nil
}

// fakeHardware builds plausible hardware inventory for a machine, varying
// enough per machine name that two machines in the same test do not look
// identical.
func fakeHardware(machineName, bootMAC string) *keziov1alpha1.MachineHardwareStatus {
	if bootMAC == "" {
		bootMAC = "aa:bb:cc:dd:ee:01"
	}
	rotational := false

	return &keziov1alpha1.MachineHardwareStatus{
		Disks: []keziov1alpha1.MachineHardwareDisk{
			{
				DeviceName:   "/dev/vda",
				SerialNumber: fmt.Sprintf("fake-%s-disk0", machineName),
				Model:        "FakeDisk",
				Vendor:       "kezio",
				SizeBytes:    256 * 1024 * 1024 * 1024,
				Rotational:   &rotational,
			},
		},
		Nics: []keziov1alpha1.MachineHardwareNIC{
			{
				Name:       "eth0",
				MACAddress: bootMAC,
			},
		},
		MemoryBytes: 16 * 1024 * 1024 * 1024,
		CPUCount:    4,
	}
}
