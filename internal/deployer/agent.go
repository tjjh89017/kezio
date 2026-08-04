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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// agentInspectPollInterval is how long Inspect asks the reconciler to
// wait before calling it again while no registration has landed yet. It
// is short enough that a machine which just registered shows up as
// Available within one or two reconciler ticks, and long enough not to
// hammer the API server while a machine spends minutes booting the live
// environment over the network.
const agentInspectPollInterval = 5 * time.Second

// AgentFactory builds Deployers whose Register and Inspect phases are
// driven by a real kezio-agent's registration (internal/agentserver),
// instead of the fabricated data deployer.FakeFactory produces. See
// agentDeployer's doc comment for what each phase actually does, and for
// the honest scope limit on Provision/Deprovision/PowerOn/PowerOff.
type AgentFactory struct {
	// Client reads and writes Machines. It is typically the manager's
	// client (mgr.GetClient()).
	Client client.Client
}

// NewAgentFactory builds an AgentFactory backed by c.
func NewAgentFactory(c client.Client) *AgentFactory {
	return &AgentFactory{Client: c}
}

// New implements the Factory function type.
func (f *AgentFactory) New(machine *keziov1alpha1.Machine) (Deployer, error) {
	return &agentDeployer{
		client: f.Client,
		key:    types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name},
	}, nil
}

// agentDeployer is the real deployer for the registration and inspection
// phases of the state machine. It has nothing of its own to poll or
// drive proactively for Register and Inspect: the actual work (the
// machine booting the live environment and kezio-agent registering) is
// something internal/bootserver and internal/agentserver already handle
// out of band, triggered by the Machine's status.state reaching
// Inspecting (see internal/bootserver's needsNetBoot) - not by anything
// this Deployer calls. Its job is to read back what that out-of-band
// work has recorded on Machine.status and translate it into Result
// values the reconciler understands.
//
// Provision, Deprovision, PowerOn, and PowerOff are not implemented by
// this milestone's work: writing a disk image and controlling a BMC are
// separate, later work items. Provision and Deprovision report that
// honestly with an ErrorMessage - a Machine that reaches Provisioning
// under this Deployer moves to the Error state and stays there,
// retrying with backoff, rather than silently pretending to succeed.
// PowerOn and PowerOff, by contrast, report success and do nothing: no
// BMC driver exists yet to actually call, and unlike Provision/
// Deprovision there is no unsafe fabricated side effect to avoid
// reporting - reconcilePower's steady-state power matching is
// background maintenance, not a step in the deployment flow, so letting
// it succeed as a no-op is what lets a Machine settle in Available
// (or Provisioned) instead of spinning on power-management retries this
// milestone was never asked to implement.
type agentDeployer struct {
	client client.Client
	key    types.NamespacedName
}

// Register arranges the netboot intent for inspection: there is nothing
// to actively do here. internal/bootserver already serves the live-boot
// GRUB config, with a fresh single-use token, to any machine whose
// status.state is Inspecting (see needsNetBoot); reaching that state is
// exactly what the reconciler does immediately after this call returns
// success. What Register does do is reset the bookkeeping from any
// previous inspection cycle: it clears status.hardware and resets the
// AgentRegistered condition to False, so a stale registration from a
// prior cycle (for example, a machine re-enrolled after already having
// deployed once) can never be mistaken by Inspect for a fresh one.
func (d *agentDeployer) Register(ctx context.Context, _ *RegisterData) (Result, error) {
	machine := &keziov1alpha1.Machine{}
	if err := d.client.Get(ctx, d.key, machine); err != nil {
		return Result{}, fmt.Errorf("agent deployer: getting machine for Register: %w", err)
	}

	machine.Status.Hardware = nil
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.MachineConditionAgentRegistered,
		Status:             metav1.ConditionFalse,
		Reason:             "AwaitingAgent",
		Message:            "waiting for kezio-agent to boot and register",
		ObservedGeneration: machine.Generation,
	})
	if err := d.client.Status().Update(ctx, machine); err != nil {
		return Result{}, fmt.Errorf("agent deployer: clearing registration state for Register: %w", err)
	}

	return Result{}, nil
}

// Inspect reports success once kezio-agent has registered and its
// inventory has landed on status.hardware - both written by
// internal/agentserver's registration handler, entirely outside this
// call. Until then it asks the reconciler to poll again.
func (d *agentDeployer) Inspect(ctx context.Context, data *InspectData) (Result, error) {
	machine := &keziov1alpha1.Machine{}
	if err := d.client.Get(ctx, d.key, machine); err != nil {
		return Result{}, fmt.Errorf("agent deployer: getting machine for Inspect: %w", err)
	}

	cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.MachineConditionAgentRegistered)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return Result{RequeueAfter: agentInspectPollInterval}, nil
	}

	data.Hardware = machine.Status.Hardware
	return Result{Dirty: true}, nil
}

// Provision is not implemented yet: writing the deployment plan to disk
// (sfdisk, EZIO AddTorrent, finalize) is separate, later work. A Machine
// that reaches Provisioning under this Deployer moves to the Error state
// with this message and retries with backoff, honestly, instead of
// fabricating a completed deployment.
func (d *agentDeployer) Provision(_ context.Context, _ *ProvisionData) (Result, error) {
	return Result{ErrorMessage: "agent deployer: Provision is not implemented yet"}, nil
}

// Deprovision is not implemented yet, for the same reason as Provision.
// A Machine deletion that reaches this call retries with backoff and its
// finalizer is never removed, until a real implementation lands; this is
// an accepted, documented limit of this milestone, not a bug.
func (d *agentDeployer) Deprovision(_ context.Context, _ *DeprovisionData) (Result, error) {
	return Result{ErrorMessage: "agent deployer: Deprovision is not implemented yet"}, nil
}

// PowerOn is a no-op that reports success: see agentDeployer's doc
// comment for why, unlike Provision/Deprovision, this is the honest
// choice here rather than an ErrorMessage.
func (d *agentDeployer) PowerOn(_ context.Context) (Result, error) {
	return Result{Dirty: true}, nil
}

// PowerOff is a no-op that reports success; see PowerOn.
func (d *agentDeployer) PowerOff(_ context.Context) (Result, error) {
	return Result{Dirty: true}, nil
}
