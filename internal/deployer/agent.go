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
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// agentInspectPollInterval is how long Inspect asks the reconciler to
// wait before calling it again while no registration has landed yet. It
// is short enough that a machine which just registered shows up as
// Available within one or two reconciler ticks, and long enough not to
// hammer the API server while a machine spends minutes booting the live
// environment over the network.
const agentInspectPollInterval = 5 * time.Second

// agentProvisionPollInterval is how long Provision asks the reconciler to
// wait before calling it again while a deploy is in progress but has not
// yet reached a terminal step. It matches agentInspectPollInterval's
// reasoning: short enough that a deploy which just finished (or just
// failed) is reflected within one or two reconciler ticks, long enough
// not to hammer the API server while a deploy runs for however long
// writing every partition's content over BitTorrent takes.
const agentProvisionPollInterval = 5 * time.Second

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

// agentDeployer is the real deployer for the registration, inspection,
// and provisioning phases of the state machine. It has nothing of its
// own to poll or drive proactively for Register, Inspect, or Provision:
// the actual work (the machine booting the live environment,
// kezio-agent registering, and kezio-agent executing a deploy plan) is
// something internal/bootserver, internal/agentserver, and
// internal/agent/deploy already handle out of band, triggered by the
// Machine's status reaching the right state (Inspecting for netboot,
// Provisioning with resolved disks and Ready images for a deploy plan) -
// not by anything this Deployer calls. Its job is to read back what
// that out-of-band work has recorded on Machine.status and translate it
// into Result values the reconciler understands, the same shape for all
// three: Register and Provision arrange or confirm the out-of-band work
// should proceed, Inspect and Provision poll a condition it writes to.
//
// PowerOn and PowerOff report success and do nothing: no BMC driver
// exists yet to actually call (a later milestone's work), and unlike
// Provision/Deprovision there is no unsafe fabricated side effect to
// avoid reporting - reconcilePower's steady-state power matching is
// background maintenance, not a step in the deployment flow, so letting
// it succeed as a no-op is what lets a Machine settle in Available (or
// Provisioned) instead of spinning on power-management retries this
// milestone was never asked to implement. Deprovision, for the same
// missing-BMC reason, is likewise a no-op that reports success - see its
// own doc comment for why that is the honest choice rather than an
// ErrorMessage.
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

// Provision drives (by polling) the deploy internal/agentserver's GET
// .../next handler already started answering ActionDeploy for, entirely
// out of band, the moment status.provisioning names this deployment -
// already true by the time Provision is called, since
// reconcileProvisioning resolves target disks (recording them into
// status.provisioning) before ever calling Provision. There is nothing
// for Provision itself to drive: internal/agent/deploy.Executor is what
// actually writes partition tables and content on the machine, and it
// reports its progress to POST /agent/machines/<name>/progress
// (internal/agentserver), which mirrors the agent's own whole-plan step
// onto keziov1alpha1.MachineConditionProvisioningProgress's Reason. So,
// the same shape as Inspect polling MachineConditionAgentRegistered,
// Provision polls that condition:
//
//   - No condition yet, or one whose ObservedGeneration lags
//     machine.Generation (a stale report from a previous deployment to
//     this same Machine - see the condition's own doc comment for why
//     nothing proactively resets it between deploys): the current
//     deployment has not reported anything yet. Keep waiting.
//   - Reason is one of agentapi.DeployStepPartitioning,
//     DeployStepWritingContent, DeployStepRunningPostHook, or
//     DeployStepFinalizing (or, from an agent too old to report a
//     whole-plan step at all, one of the agentapi.PartitionPhase*
//     fallback values, or the summarizer's own "Unknown"): the deploy is
//     still running. Keep waiting.
//   - Reason is agentapi.DeployStepRebootingToDisk: the deploy finished.
//     Fill in ResolvedTargetDisk/ResolvedDataImageDisks by reading them
//     back from status.provisioning - the same disks resolveTargetDisks
//     already resolved and recorded before Provision was ever called -
//     rather than re-deriving them, so buildProvisioningStatus's own
//     write afterwards can never disagree with what diskmatch resolved.
//   - Reason is agentapi.DeployStepFailed: the deploy failed. Report
//     Result.ErrorMessage with the condition's Message (the agent's own
//     failure detail - see agentapi.DeployStepFailed's doc comment).
func (d *agentDeployer) Provision(ctx context.Context, data *ProvisionData) (Result, error) {
	machine := &keziov1alpha1.Machine{}
	if err := d.client.Get(ctx, d.key, machine); err != nil {
		return Result{}, fmt.Errorf("agent deployer: getting machine for Provision: %w", err)
	}

	cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil || cond.ObservedGeneration != machine.Generation {
		return Result{RequeueAfter: agentProvisionPollInterval}, nil
	}

	switch cond.Reason {
	case agentapi.DeployStepRebootingToDisk:
		if provisioning := machine.Status.Provisioning; provisioning != nil {
			if provisioning.Image != nil {
				data.ResolvedTargetDisk = provisioning.Image.TargetDisk
			}
			if len(provisioning.DataImages) > 0 {
				data.ResolvedDataImageDisks = make([]string, len(provisioning.DataImages))
				for i, rec := range provisioning.DataImages {
					data.ResolvedDataImageDisks[i] = rec.TargetDisk
				}
			}
		}
		return Result{Dirty: true}, nil
	case agentapi.DeployStepFailed:
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: kezio-agent reported a failed deploy: %s", cond.Message)}, nil
	default:
		return Result{RequeueAfter: agentProvisionPollInterval}, nil
	}
}

// Deprovision releases a deployed machine ahead of its deletion (onDelete
// calls it, then removes the finalizer once it reports success). Its
// honest scope today is a no-op that reports success, for two reasons
// together: first, this Deployer holds no live session or lease outside
// the Machine object itself worth releasing - no BMC driver exists yet
// to power the machine off (a later milestone's work, the same limit
// PowerOn/PowerOff document), and the netboot token Register mints is
// either already consumed or left to expire on its own, never worth
// revoking early. Second, clearing status.provisioning would be
// pointless work: onDelete removes the finalizer and lets the API server
// delete the whole object immediately after this call succeeds, so
// nothing is ever left around to read a cleared status.provisioning
// back from. Reporting success unconditionally is what lets a Machine
// deletion complete instead of wedging behind BMC work this milestone
// does not do.
func (d *agentDeployer) Deprovision(_ context.Context, _ *DeprovisionData) (Result, error) {
	return Result{Dirty: true}, nil
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
