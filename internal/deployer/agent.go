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

// New implements the Factory function type. It captures machine.Spec.BMC
// and the insecure-skip-verify annotation (see
// keziov1alpha1.AnnotationBMCInsecureSkipVerify) at build time: every
// phase call for the returned Deployer happens within the same reconcile
// that built it, so this is always the same BMC configuration Register's
// own RegisterData.BMC carries for that reconcile - capturing it here is
// what lets PowerOn/PowerOff (which take no per-call data) reach it too.
func (f *AgentFactory) New(machine *keziov1alpha1.Machine) (Deployer, error) {
	return &agentDeployer{
		client:                f.Client,
		key:                   types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name},
		bmcSpec:               machine.Spec.BMC,
		bmcInsecureSkipVerify: machine.Annotations[keziov1alpha1.AnnotationBMCInsecureSkipVerify] == "true",
	}, nil
}

// agentDeployer is the real deployer for the registration, inspection,
// and provisioning phases of the state machine. It has nothing of its
// own to poll or drive proactively for Inspect or Provision: the actual
// work (the machine booting the live environment, kezio-agent
// registering, and kezio-agent executing a deploy plan) is something
// internal/bootserver, internal/agentserver, and internal/agent/deploy
// already handle out of band, triggered by the Machine's status reaching
// the right state (Inspecting for netboot, Provisioning with resolved
// disks and Ready images for a deploy plan) - not by anything this
// Deployer calls. Inspect and Provision read back what that out-of-band
// work has recorded on Machine.status and translate it into Result
// values the reconciler understands, by polling a condition each writes
// to.
//
// Register, PowerOn, and PowerOff drive power and boot order through
// internal/bmc when the Machine names one (bmcSpec.Address is
// non-empty): see connectBMC. When it does not, they fall back to the
// pre-BMC behavior each documents on its own: Register arranges the
// netboot-wait handoff bootserver's own polling relies on, and
// PowerOn/PowerOff are no-ops, since without a BMC there is nothing this
// Deployer can drive - the machine's power state is either
// human-operated or left to the agent's own systemctl calls. Deprovision
// stays a no-op regardless of a configured BMC; see its own doc comment
// for why.
type agentDeployer struct {
	client client.Client
	key    types.NamespacedName

	// bmcSpec is the Machine's spec.bmc, captured at New() time (see its
	// doc comment). An empty Address means no BMC is configured for this
	// Machine.
	bmcSpec keziov1alpha1.MachineBMC
	// bmcInsecureSkipVerify is the resolved value of
	// keziov1alpha1.AnnotationBMCInsecureSkipVerify, captured at New()
	// time alongside bmcSpec.
	bmcInsecureSkipVerify bool
}

// connectBMC resolves and connects to the Machine's BMC, returning (nil,
// nil) when bmcSpec.Address is empty - the signal every caller uses to
// take the no-BMC fallback path. It resolves
// bmcSpec.CredentialsSecretRef from the same namespace as the Machine
// itself (BMC credential Secrets are not expected to be shared across
// namespaces) into a bmc.Credentials value, then hands off to
// bmc.Connect. Neither the Secret's contents nor the resolved
// Credentials are ever included in a returned error: the Secret lookup
// error names only the Secret (namespace/name), CredentialsFromSecretData
// already redacts its own errors down to which key is missing, and
// bmc.Connect redacts the address itself before including it in an
// error.
func (d *agentDeployer) connectBMC(ctx context.Context) (bmc.BMC, error) {
	if d.bmcSpec.Address == "" {
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

// Register arranges the netboot intent for inspection. It always resets
// the bookkeeping from any previous inspection cycle first: it clears
// status.hardware and resets the AgentRegistered condition to False, so
// a stale registration from a prior cycle (for example, a machine
// re-enrolled after already having deployed once) can never be mistaken
// by Inspect for a fresh one.
//
// What drives the machine to actually net boot depends on whether a BMC
// is configured (bmcSpec.Address non-empty):
//
//   - No BMC: there is nothing further to actively do.
//     internal/bootserver already serves the live-boot GRUB config, with
//     a fresh single-use token, to any machine whose status.state is
//     Inspecting (see needsNetBoot); reaching that state is exactly what
//     the reconciler does immediately after this call returns success.
//     Powering the machine on and pointing it at PXE is left to whoever
//     operates it - the same netboot-wait behavior this Deployer has
//     always had.
//   - A BMC is configured: Register connects to it, arranges a one-time
//     PXE boot (SetOneTimePXEBoot), then reads the machine's current
//     power state to decide how to bring it up for that boot -
//     PowerCycle if it is already on (forcing a clean reset instead of
//     leaving whatever OS is running to ignore the PXE override),
//     PowerOn otherwise. A failure at any BMC step reports
//     Result.ErrorMessage instead of completing Register, so the
//     reconciler moves the machine to Error rather than advancing it to
//     Inspecting behind a machine that was never actually told to boot.
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
// honest scope today is a no-op that reports success regardless of
// whether a BMC is configured, for two reasons together: first, this
// Deployer holds no live session or lease outside the Machine object
// itself worth releasing - powering the machine off is not part of
// releasing it (Deprovision undoes Provision, not Online), and the
// netboot token Register mints is either already consumed or left to
// expire on its own, never worth revoking early. Second, clearing
// status.provisioning would be pointless work: onDelete removes the
// finalizer and lets the API server delete the whole object immediately
// after this call succeeds, so nothing is ever left around to read a
// cleared status.provisioning back from. Reporting success
// unconditionally is what lets a Machine deletion complete without
// waiting on BMC work that would not change the outcome anyway.
func (d *agentDeployer) Deprovision(_ context.Context, _ *DeprovisionData) (Result, error) {
	return Result{Dirty: true}, nil
}

// PowerOn powers the machine on through its BMC when one is configured
// (bmcSpec.Address non-empty). Without a BMC it is a no-op that reports
// success: reconcilePower's steady-state power matching is background
// maintenance, not a step in the deployment flow, and without a BMC
// there is nothing this Deployer can drive - the machine's power state
// is either human-operated or left to the agent's own systemctl calls -
// so letting it succeed as a no-op is what lets a Machine settle in
// Available (or Provisioned) instead of spinning on power-management
// retries it has no way to act on.
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
	return Result{Dirty: true}, nil
}

// PowerOff powers the machine off through its BMC when one is configured;
// see PowerOn for the no-BMC fallback and why it is the honest choice
// there. BMC.PowerOff is a graceful (orderly OS shutdown) power-down, the
// same as an agent-issued "systemctl poweroff" would be - see
// reconcileProvisioning's afterDeploy=PowerOff handling for how this
// interacts with the agent's own shutdown path.
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
	return Result{Dirty: true}, nil
}
