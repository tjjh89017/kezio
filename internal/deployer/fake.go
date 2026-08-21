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

package deployer

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// FakeFailAnnotation is a test-only override, read only by FakeDeployer's
// default (non-Func) behavior: it never affects a real deployer. Value
// syntax is "<step>:<count>", where step is "provision" or "deprovision"
// and count is a non-negative integer or the literal "forever". The
// matching step fails (Result{Outcome: Failed}) while
// machine.Status.ErrorCount is below count, then reverts to the default
// always-succeed behavior once errorCount reaches it - "forever" fails
// unconditionally. This lets the fast e2e lane script a failing step (and
// a permanently dead BMC) from outside the manager process, entirely
// through the Machine object the controller already reads every reconcile;
// a Machine without this annotation is unaffected, so production's
// always-succeed default is unchanged.
const FakeFailAnnotation = "kezio.kojuro.date/fake-fail"

// fakeFailForever is the FakeFailAnnotation count value that fails its step
// unconditionally, instead of a bounded number of times.
const fakeFailForever = "forever"

// FakeDisksAnnotation is a test-only override, read only by FakeDeployer's
// default (non-Func) Inspect: it never affects a real deployer. The only
// recognized value is "2", which fabricates two distinct disks
// (/dev/vda, /dev/vdb - differing serial, WWN, model, and size) instead of
// the default single /dev/vda, so a test Machine's targetDisk hints have
// something to disambiguate. A Machine without this annotation, or with
// any other value, gets the unchanged single-disk inventory production and
// every other e2e lane rely on.
const FakeDisksAnnotation = "kezio.kojuro.date/fake-disks"

// fakeSingleDisk is the default synthetic inventory: one plausible but
// otherwise featureless disk, unchanged since before FakeDisksAnnotation
// existed.
var fakeSingleDisk = []keziov1alpha2.MachineHardwareDisk{
	{
		DeviceName: "/dev/vda",
		SizeBytes:  32 << 30, // 32Gi, an arbitrary but plausible fake disk size.
	},
}

// fakeTwoDisks is the FakeDisksAnnotation="2" inventory: two disks that
// differ in every field diskmatch.Match can hint on, so a test can write a
// hint that matches both (ambiguous), a hint that matches only the second
// (a unique disambiguating match), or a hint that matches neither (no
// match). Both share Rotational=false so a "rotational: false" hint alone
// is deliberately ambiguous between them.
var fakeTwoDisks = []keziov1alpha2.MachineHardwareDisk{
	{
		DeviceName:   "/dev/vda",
		SizeBytes:    32 << 30, // 32Gi
		SerialNumber: "fake-disk-0",
		WWN:          "0x5000000000000000",
		Model:        "FakeDisk0",
		Vendor:       "FakeVendor",
		Rotational:   boolPtr(false),
	},
	{
		DeviceName:   "/dev/vdb",
		SizeBytes:    64 << 30, // 64Gi, deliberately different from the first disk.
		SerialNumber: "fake-disk-1",
		WWN:          "0x5000000000000001",
		Model:        "FakeDisk1",
		Vendor:       "FakeVendor",
		Rotational:   boolPtr(false),
	},
}

// boolPtr returns a pointer to v, for MachineHardwareDisk.Rotational
// literals.
func boolPtr(v bool) *bool { return &v }

// fakeDisks selects Inspect's synthetic disk inventory for machine:
// fakeTwoDisks when FakeDisksAnnotation is exactly "2", fakeSingleDisk
// otherwise (absent, or any other value).
func fakeDisks(machine *keziov1alpha2.Machine) []keziov1alpha2.MachineHardwareDisk {
	if machine.Annotations[FakeDisksAnnotation] == "2" {
		return fakeTwoDisks
	}
	return fakeSingleDisk
}

// fakeFailOverride inspects machine's FakeFailAnnotation for step and, when
// it applies, returns the scripted Failed Result to use instead of the
// default behavior. The second return value is false when the annotation is
// absent, names a different step, or has failed to parse - each of those
// falls through to the default behavior unchanged.
func fakeFailOverride(machine *keziov1alpha2.Machine, step string) (Result, bool) {
	raw, ok := machine.Annotations[FakeFailAnnotation]
	if !ok {
		return Result{}, false
	}
	annStep, countRaw, found := strings.Cut(raw, ":")
	if !found || annStep != step {
		return Result{}, false
	}

	if countRaw != fakeFailForever {
		count, err := strconv.Atoi(countRaw)
		if err != nil || count < 0 || int(machine.Status.ErrorCount) >= count {
			return Result{}, false
		}
	}

	return Result{
		Outcome:      Failed,
		ErrorType:    keziov1alpha2.MachineErrorTypeTransient,
		ErrorMessage: fmt.Sprintf("fake deployer: scripted failure via %s=%q", FakeFailAnnotation, raw),
	}, true
}

// deployRunPhaseOrder is the sequence FakeDeployer.Provision walks a
// DeployRun through by default, one phase per call. It excludes
// DeployRunPhaseFailed: the default fake never fails on its own.
var deployRunPhaseOrder = []string{
	keziov1alpha2.DeployRunPhasePending,
	keziov1alpha2.DeployRunPhasePartitioning,
	keziov1alpha2.DeployRunPhaseWritingContent,
	keziov1alpha2.DeployRunPhaseRunningPostHook,
	keziov1alpha2.DeployRunPhaseFinalizing,
	keziov1alpha2.DeployRunPhaseSucceeded,
}

// FakeDeployer is a Deployer that never dials real hardware and never
// imports internal/bmc: Inspect fabricates a MachineHardware inventory in
// one step, and Provision walks a DeployRun through its phases one step
// per call, both using Client to write the objects a real deployer would
// populate from hardware. It backs envtest and the fast e2e lane, and its
// InspectFunc/ProvisionFunc fields let a test script any Outcome
// (including Busy, Failed, or a transient error) without a second
// implementation of Deployer.
type FakeDeployer struct {
	// Client is used to write the synthetic MachineHardware (Inspect) and
	// DeployRun status (Provision) the default behavior produces. Required
	// unless both InspectFunc and ProvisionFunc are set.
	Client client.Client

	// InspectFunc, when set, replaces the default Inspect behavior.
	InspectFunc func(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error)
	// ProvisionFunc, when set, replaces the default Provision behavior.
	ProvisionFunc func(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error)
	// DeprovisionFunc, when set, replaces the default Deprovision behavior.
	DeprovisionFunc func(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error)
	// PowerOffFunc, when set, replaces the default PowerOff behavior.
	PowerOffFunc func(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error)
	// RebootFunc, when set, replaces the default Reboot behavior.
	RebootFunc func(ctx context.Context, machine *keziov1alpha2.Machine, hard bool) (Result, error)
}

var _ Deployer = (*FakeDeployer)(nil)

// Inspect implements Deployer. The default behavior is deliberately a
// stub: it fabricates one disk (two, opt-in via FakeDisksAnnotation) and
// one NIC rather than reading real hardware, and never resolves
// machine.spec.subnetRef (Subnet does not exist yet in this stage).
func (f *FakeDeployer) Inspect(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
	if f.InspectFunc != nil {
		return f.InspectFunc(ctx, machine, restartOnFailure)
	}

	hw := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{
			Name:            machine.Name,
			Namespace:       machine.Namespace,
			OwnerReferences: []metav1.OwnerReference{machineOwnerReference(machine)},
		},
		Spec: keziov1alpha2.MachineHardwareSpec{
			Disks: fakeDisks(machine),
			Nics: []keziov1alpha2.MachineHardwareNIC{
				{Name: "eth0", MACAddress: machine.Spec.BootMACAddress},
			},
			MemoryBytes: 4 << 30, // 4Gi
			CPUCount:    2,
		},
	}

	if err := f.Client.Create(ctx, hw); err != nil && !apierrors.IsAlreadyExists(err) {
		return Result{}, fmt.Errorf("fake deployer: writing synthetic MachineHardware for %q: %w", machine.Name, err)
	}

	return Result{Outcome: Complete}, nil
}

// Provision implements Deployer. The default behavior advances run one
// phase per call (Pending -> ... -> Succeeded), recording a phase timing
// each step, and never resolves machine.spec.imageRef/dataImages/
// postHookRefs (Image and PostHook do not exist yet in this stage) - it
// treats every run as trivially deployable.
func (f *FakeDeployer) Provision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error) {
	if f.ProvisionFunc != nil {
		return f.ProvisionFunc(ctx, machine, run, restartOnFailure)
	}
	if result, fail := fakeFailOverride(machine, "provision"); fail {
		return result, nil
	}

	next, done, err := nextDeployRunPhase(run.Status.Phase)
	if err != nil {
		return Result{}, fmt.Errorf("fake deployer: %w", err)
	}

	now := metav1.Now()
	if n := len(run.Status.PhaseTimings); n > 0 && run.Status.PhaseTimings[n-1].FinishedAt == nil {
		run.Status.PhaseTimings[n-1].FinishedAt = &now
	}
	run.Status.Phase = next
	run.Status.PhaseTimings = append(run.Status.PhaseTimings, keziov1alpha2.DeployRunPhaseTiming{
		Phase:     next,
		StartedAt: now,
	})

	if done {
		run.Status.PhaseTimings[len(run.Status.PhaseTimings)-1].FinishedAt = &now
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha2.DeployRunConditionSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             "FakeDeploySucceeded",
			Message:            "fake deployer completed this run without touching real hardware",
			ObservedGeneration: run.Generation,
		})
	}

	if err := f.Client.Status().Update(ctx, run); err != nil {
		return Result{}, fmt.Errorf("fake deployer: writing DeployRun %q status: %w", run.Name, err)
	}

	if !done {
		return Result{Outcome: Continuing}, nil
	}
	return Result{Outcome: Complete}, nil
}

// Deprovision implements Deployer. The default behavior is an instant
// Complete: this stage has no deployer-managed deployed state (no real BMC
// or agent session) for the default fake to tear down.
func (f *FakeDeployer) Deprovision(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
	if f.DeprovisionFunc != nil {
		return f.DeprovisionFunc(ctx, machine, restartOnFailure)
	}
	if result, fail := fakeFailOverride(machine, "deprovision"); fail {
		return result, nil
	}
	return Result{Outcome: Complete}, nil
}

// PowerOff implements Deployer. The default behavior is an instant
// Complete: the default fake never dials a real BMC to change power state.
func (f *FakeDeployer) PowerOff(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error) {
	if f.PowerOffFunc != nil {
		return f.PowerOffFunc(ctx, machine)
	}
	return Result{Outcome: Complete}, nil
}

// Reboot implements Deployer. The default behavior is an instant Complete:
// the default fake never dials a real BMC to reboot the machine.
func (f *FakeDeployer) Reboot(ctx context.Context, machine *keziov1alpha2.Machine, hard bool) (Result, error) {
	if f.RebootFunc != nil {
		return f.RebootFunc(ctx, machine, hard)
	}
	return Result{Outcome: Complete}, nil
}

// nextDeployRunPhase returns the phase after current in
// deployRunPhaseOrder, and whether that phase is the terminal one. An
// unrecognized current phase (including "", the not-yet-started case) is
// treated as the phase before Pending.
func nextDeployRunPhase(current string) (next string, done bool, err error) {
	index := 0
	for i, phase := range deployRunPhaseOrder {
		if phase == current {
			index = i + 1
			break
		}
	}
	if current != "" && index == 0 {
		return "", false, fmt.Errorf("run status.phase %q is not one the fake deployer can advance from", current)
	}
	if index >= len(deployRunPhaseOrder) {
		return "", false, fmt.Errorf("run status.phase %q is already terminal", current)
	}
	return deployRunPhaseOrder[index], index == len(deployRunPhaseOrder)-1, nil
}

// machineOwnerReference builds the owner reference FakeDeployer stamps on
// the MachineHardware it writes. Built by hand instead of through
// controllerutil.SetControllerReference so the fake needs no
// runtime.Scheme dependency.
func machineOwnerReference(machine *keziov1alpha2.Machine) metav1.OwnerReference {
	blockOwnerDeletion := true
	controller := true
	return metav1.OwnerReference{
		APIVersion:         keziov1alpha2.GroupVersion.String(),
		Kind:               "Machine",
		Name:               machine.Name,
		UID:                machine.UID,
		BlockOwnerDeletion: &blockOwnerDeletion,
		Controller:         &controller,
	}
}
