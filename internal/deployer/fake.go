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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

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
	InspectFunc func(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error)
	// ProvisionFunc, when set, replaces the default Provision behavior.
	ProvisionFunc func(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun) (Result, error)
}

var _ Deployer = (*FakeDeployer)(nil)

// Inspect implements Deployer. The default behavior is deliberately a
// stub: it fabricates one disk and one NIC rather than reading real
// hardware, and never resolves machine.spec.subnetRef (Subnet does not
// exist yet in this stage).
func (f *FakeDeployer) Inspect(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error) {
	if f.InspectFunc != nil {
		return f.InspectFunc(ctx, machine)
	}

	hw := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{
			Name:            machine.Name,
			Namespace:       machine.Namespace,
			OwnerReferences: []metav1.OwnerReference{machineOwnerReference(machine)},
		},
		Spec: keziov1alpha2.MachineHardwareSpec{
			Disks: []keziov1alpha2.MachineHardwareDisk{
				{
					DeviceName: "/dev/vda",
					SizeBytes:  32 << 30, // 32Gi, an arbitrary but plausible fake disk size.
				},
			},
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
func (f *FakeDeployer) Provision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun) (Result, error) {
	if f.ProvisionFunc != nil {
		return f.ProvisionFunc(ctx, machine, run)
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
