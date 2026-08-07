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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// agentInspectPollInterval balances API-server load against reflecting a
// fresh registration within one or two reconciler ticks.
const agentInspectPollInterval = 5 * time.Second

// agentProvisionPollInterval: same tradeoff as agentInspectPollInterval,
// for a deploy in progress.
const agentProvisionPollInterval = 5 * time.Second

// AgentFactory builds Deployers whose Register and Inspect phases are
// driven by a real kezio-agent's registration (internal/agentserver),
// instead of the fabricated data deployer.FakeFactory produces.
type AgentFactory struct {
	// Client reads and writes Machines. It is typically the manager's
	// client (mgr.GetClient()).
	Client client.Client
}

// NewAgentFactory builds an AgentFactory backed by c.
func NewAgentFactory(c client.Client) *AgentFactory {
	return &AgentFactory{Client: c}
}

// New implements the Factory function type. It captures machine.Spec.BMC
// and the insecure-skip-verify annotation at build time, since PowerOn/
// PowerOff take no per-call data and need it from somewhere.
func (f *AgentFactory) New(machine *keziov1alpha1.Machine) (Deployer, error) {
	return &agentDeployer{
		client:                f.Client,
		key:                   types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name},
		bmcSpec:               machine.Spec.BMC,
		bmcInsecureSkipVerify: machine.Annotations[keziov1alpha1.AnnotationBMCInsecureSkipVerify] == "true",
	}, nil
}

// agentDeployer is the real deployer for registration, inspection, and
// provisioning. Inspect and Provision do not drive any work themselves:
// internal/bootserver, internal/agentserver, and internal/agent/deploy
// handle netboot, agent registration, and deploy execution out of band,
// triggered purely by Machine.status reaching the right state. Inspect
// and Provision just poll the conditions that out-of-band work writes.
//
// Register/PowerOn/PowerOff/PowerCycle drive power and boot order through
// internal/bmc when the Machine names one (bmcSpec non-nil, non-empty
// Address; see connectBMC); without one they fall back to no-BMC
// behavior documented on each method. Deprovision is always a no-op
// regardless of BMC configuration (see its own doc comment).
type agentDeployer struct {
	client client.Client
	key    types.NamespacedName

	// bmcSpec is the Machine's spec.bmc, captured at New() time (see its
	// doc comment). Nil (or a non-nil value with an empty Address) means
	// no BMC is configured for this Machine.
	bmcSpec *keziov1alpha1.MachineBMC
	// bmcInsecureSkipVerify is the resolved value of
	// keziov1alpha1.AnnotationBMCInsecureSkipVerify, captured at New()
	// time alongside bmcSpec.
	bmcInsecureSkipVerify bool
}

// connectBMC resolves and connects to the Machine's BMC, returning (nil,
// nil) when bmcSpec is nil or its Address is empty - the signal every
// caller uses to take the no-BMC fallback path. CredentialsSecretRef is
// resolved from the Machine's own namespace (BMC secrets are not shared
// across namespaces). Errors never leak secret contents or credentials.
func (d *agentDeployer) connectBMC(ctx context.Context) (bmc.BMC, error) {
	if d.bmcSpec == nil || d.bmcSpec.Address == "" {
		return nil, nil
	}

	secretKey := types.NamespacedName{Namespace: d.key.Namespace, Name: d.bmcSpec.CredentialsSecretRef.Name}
	secret := &corev1.Secret{}
	if err := d.client.Get(ctx, secretKey, secret); err != nil {
		return nil, fmt.Errorf("agent deployer: getting BMC credentials secret %s: %w", secretKey, err)
	}

	creds, err := bmc.CredentialsFromSecretData(secret.Data)
	if err != nil {
		return nil, fmt.Errorf("agent deployer: resolving BMC credentials: %w", err)
	}

	bmcClient, err := bmc.Connect(ctx, d.bmcSpec.Address, creds, bmc.Options{InsecureSkipVerify: d.bmcInsecureSkipVerify})
	if err != nil {
		return nil, fmt.Errorf("agent deployer: connecting to BMC: %w", err)
	}
	return bmcClient, nil
}

// Register arranges the netboot intent for inspection. It always clears
// status.hardware and resets AgentRegistered to False first, so a stale
// registration from a prior cycle (e.g. a re-enrolled machine) is never
// mistaken by Inspect for a fresh one.
//
// No BMC: nothing further to do - internal/bootserver serves the
// live-boot GRUB config to any Inspecting machine (see needsNetBoot);
// powering on and pointing at PXE is left to the operator.
// BMC configured: connects, sets one-time PXE boot, then PowerCycles if
// already on (forcing a reset so the running OS can't ignore the PXE
// override) or PowerOns otherwise. Any BMC step failing reports
// Result.ErrorMessage instead of completing, so the reconciler goes to
// Error rather than advancing to Inspecting behind a machine never told
// to boot.
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

	bmcClient, err := d.connectBMC(ctx)
	if err != nil {
		return Result{ErrorMessage: err.Error()}, nil
	}
	if bmcClient == nil {
		return Result{}, nil
	}

	if err := bmcClient.SetOneTimePXEBoot(ctx); err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: setting one-time PXE boot: %v", err)}, nil
	}

	state, err := bmcClient.GetPowerState(ctx)
	if err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: reading BMC power state: %v", err)}, nil
	}

	if state == bmc.PowerStateOn {
		err = bmcClient.PowerCycle(ctx)
	} else {
		err = bmcClient.PowerOn(ctx)
	}
	if err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: powering on machine for net boot: %v", err)}, nil
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

// Provision polls keziov1alpha1.MachineConditionProvisioningProgress,
// which mirrors the agent's whole-plan deploy step (reported via POST
// /agent/machines/<name>/progress, internal/agentserver) while
// internal/agent/deploy.Executor does the actual work out of band. The
// deploy itself is already driven by agentserver's GET .../next once
// status.provisioning names it (set before Provision is ever called).
//
//   - No condition, or ObservedGeneration lags machine.Generation (stale
//     report from a prior deployment): keep waiting.
//   - DeployStepPartitioning/WritingContent/RunningPostHook/Finalizing
//     (or a pre-whole-plan-step agent's PartitionPhase* fallback, or
//     "Unknown"): still running, keep waiting.
//   - DeployStepRebootingToDisk: finished. Fill in
//     ResolvedTargetDisk/ResolvedDataImageDisks by reading them back from
//     status.provisioning (already resolved by resolveTargetDisks before
//     Provision ran) rather than re-deriving, so they can never disagree
//     with what diskmatch resolved.
//   - DeployStepFailed: report Result.ErrorMessage with the condition's
//     Message (the agent's own failure detail).
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

// Deprovision releases a deployed machine ahead of deletion (onDelete
// calls it, then removes the finalizer on success). Always a no-op that
// reports success, BMC or not: nothing here holds a live session/lease
// worth releasing (powering off is not part of Deprovision - it undoes
// Provision, not Online), and the object is deleted immediately after
// this succeeds, so there's nothing left to read a cleared
// status.provisioning back from anyway.
func (d *agentDeployer) Deprovision(_ context.Context, _ *DeprovisionData) (Result, error) {
	return Result{Dirty: true}, nil
}

// PowerOn powers the machine on through its BMC when configured. Without
// a BMC it's a no-op reporting success, so a Machine can settle in
// Available/Provisioned instead of retrying power management it has no
// way to act on. Result.PoweredOn is filled from a post-action
// GetPowerState read (observedPowerState), not assumed from success.
func (d *agentDeployer) PowerOn(ctx context.Context) (Result, error) {
	bmcClient, err := d.connectBMC(ctx)
	if err != nil {
		return Result{ErrorMessage: err.Error()}, nil
	}
	if bmcClient == nil {
		return Result{Dirty: true}, nil
	}
	if err := bmcClient.PowerOn(ctx); err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: powering on machine: %v", err)}, nil
	}
	return Result{Dirty: true, PoweredOn: observedPowerState(ctx, bmcClient)}, nil
}

// PowerOff powers the machine off through its BMC when configured (see
// PowerOn for the no-BMC fallback and Result.PoweredOn). BMC.PowerOff is
// a graceful shutdown, equivalent to an agent-issued "systemctl
// poweroff".
func (d *agentDeployer) PowerOff(ctx context.Context) (Result, error) {
	bmcClient, err := d.connectBMC(ctx)
	if err != nil {
		return Result{ErrorMessage: err.Error()}, nil
	}
	if bmcClient == nil {
		return Result{Dirty: true}, nil
	}
	if err := bmcClient.PowerOff(ctx); err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: powering off machine: %v", err)}, nil
	}
	return Result{Dirty: true, PoweredOn: observedPowerState(ctx, bmcClient)}, nil
}

// PowerCycle forces an immediate power-on reset through its BMC when
// configured (see PowerOn for the no-BMC fallback). Drives
// reconcileProvisioning's AfterDeploy=Reboot handling and
// reconcileInspecting's stuck-machine recovery.
func (d *agentDeployer) PowerCycle(ctx context.Context) (Result, error) {
	bmcClient, err := d.connectBMC(ctx)
	if err != nil {
		return Result{ErrorMessage: err.Error()}, nil
	}
	if bmcClient == nil {
		return Result{Dirty: true}, nil
	}
	if err := bmcClient.PowerCycle(ctx); err != nil {
		return Result{ErrorMessage: fmt.Sprintf("agent deployer: power-cycling machine: %v", err)}, nil
	}
	return Result{Dirty: true, PoweredOn: observedPowerState(ctx, bmcClient)}, nil
}

// observedPowerState converts a read-back BMC power state to the *bool
// shape Result.PoweredOn / Machine.status.poweredOn share: true/false for
// On/Off, nil for Unknown or a failed read. Nil is not an error - the
// action itself already succeeded; only the follow-up read was
// inconclusive, so the reconciler falls back to the commanded state.
func observedPowerState(ctx context.Context, bmcClient bmc.BMC) *bool {
	state, err := bmcClient.GetPowerState(ctx)
	if err != nil {
		return nil
	}
	switch state {
	case bmc.PowerStateOn:
		on := true
		return &on
	case bmc.PowerStateOff:
		off := false
		return &off
	default:
		return nil
	}
}
