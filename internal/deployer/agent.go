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
	"errors"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// agentDeployerPXEArmedAnnotation records, as an RFC3339 timestamp, when
// Inspect last set one-time PXE boot and powered the machine on. Deployer
// implementations keep no per-attempt state between calls (see the
// Deployer interface doc comment), and the controller may call Inspect
// again after a manager restart, so "PXE already armed for this pass"
// cannot live in memory - it is read back from this annotation instead.
const agentDeployerPXEArmedAnnotation = "kezio.kojuro.date/agent-pxe-armed-at"

// agentDeployerInspectDeadline bounds how long Inspect waits for
// kezio-agent to register (internal/agentserver) after arming PXE boot.
// Past this, Inspect reports Failed with ErrorType Restart so the
// controller's restartOnFailure re-arms PXE and power-cycles the machine
// on the next attempt, instead of waiting forever on a machine that never
// net-booted.
const agentDeployerInspectDeadline = 5 * time.Minute

// agentDeployerUnsupportedMessage is Provision/Deprovision's
// Result.ErrorMessage: AgentDeployer only drives inspection and power.
const agentDeployerUnsupportedMessage = "provisioning is not available with this deployer"

// agentDeployerUnsupportedRequeueInterval is Provision/Deprovision's
// Result.RequeueAfter.
const agentDeployerUnsupportedRequeueInterval = time.Hour

// AgentDeployer drives hardware inspection and power through a Machine's
// BMC (internal/bmc) and the live kezio-agent registration recorded by
// internal/agentserver. It never resolves DeployRun provisioning: that
// path is driven out of band by internal/agentserver and internal/bootd
// once a later stage exists, so Provision/Deprovision report Delayed
// rather than pretending to make progress.
//
// Inspect's "PXE already armed" fact must survive a manager restart (the
// Deployer interface promises no in-memory state between calls), so it is
// recorded on the Machine as agentDeployerPXEArmedAnnotation rather than
// kept in this struct. status.netBoot is not used for this: it is written
// by the boot config server when the machine actually fetches its grub
// config, a different event than "Inspect commanded a PXE boot", and
// owned by a different component.
type AgentDeployer struct {
	// Client reads and writes Machine, MachineHardware, and the BMC
	// credentials Secret. Required.
	Client client.Client
}

var _ Deployer = (*AgentDeployer)(nil)

// connectBMC resolves machine.spec.bmc.credentialsSecretRef from
// machine's own namespace and connects to its BMC. Errors never include
// the resolved credentials.
func (d *AgentDeployer) connectBMC(ctx context.Context, machine *keziov1alpha2.Machine) (bmc.BMC, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.BMC.CredentialsSecretRef.Name}
	if err := d.Client.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("agent deployer: getting BMC credentials secret %q: %w", key.Name, err)
	}

	creds, err := bmc.CredentialsFromSecretData(secret.Data)
	if err != nil {
		return nil, fmt.Errorf("agent deployer: resolving BMC credentials: %w", err)
	}

	bmcClient, err := bmc.Connect(ctx, machine.Spec.BMC.Address, creds, bmc.Options{})
	if err != nil {
		return nil, fmt.Errorf("agent deployer: connecting to BMC: %w", err)
	}
	return bmcClient, nil
}

// classifyBMCError turns a connectBMC/bmc.BMC error into the Result a
// Deployer step returns for it: a network-level failure (dial refused,
// timeout, DNS) reports Delayed - the BMC may simply be booting or
// briefly unreachable - while every other error (bad credentials, a
// rejected command, an unresolvable Secret) reports Failed with
// ErrorType Transient. err.Error() is safe to surface here: bmc.Connect
// and CredentialsFromSecretData never include credential values in an
// error.
func classifyBMCError(err error) Result {
	if isNetworkUnreachable(err) {
		return Result{Outcome: Delayed}
	}
	return Result{
		Outcome:      Failed,
		ErrorType:    keziov1alpha2.MachineErrorTypeTransient,
		ErrorMessage: err.Error(),
	}
}

// isNetworkUnreachable reports whether err's chain contains a net.Error,
// the shape a dial failure, timeout, or DNS lookup failure takes -
// including through an *url.Error wrapper, since net/http returns one for
// every failed dial and errors.As follows its Unwrap chain.
func isNetworkUnreachable(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// pxeArmedAt reads agentDeployerPXEArmedAnnotation off machine. The second
// return is false when the annotation is absent or fails to parse - both
// treated as "not armed yet", never as an error.
func pxeArmedAt(machine *keziov1alpha2.Machine) (time.Time, bool) {
	raw, ok := machine.Annotations[agentDeployerPXEArmedAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// armPXE stamps agentDeployerPXEArmedAnnotation with the current time,
// mutating machine in place so the caller's copy reflects it immediately.
func (d *AgentDeployer) armPXE(ctx context.Context, machine *keziov1alpha2.Machine) error {
	patch := client.MergeFrom(machine.DeepCopy())
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	machine.Annotations[agentDeployerPXEArmedAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := d.Client.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("agent deployer: recording PXE-armed marker: %w", err)
	}
	return nil
}

// disarmPXE removes agentDeployerPXEArmedAnnotation once inspection
// completes, so a later re-inspect (which restarts the walk at Inspecting
// with no restartOnFailure) starts from "not armed" instead of reading a
// stale timestamp from the previous inspection pass.
func (d *AgentDeployer) disarmPXE(ctx context.Context, machine *keziov1alpha2.Machine) error {
	if _, ok := machine.Annotations[agentDeployerPXEArmedAnnotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	delete(machine.Annotations, agentDeployerPXEArmedAnnotation)
	if err := d.Client.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("agent deployer: clearing PXE-armed marker: %w", err)
	}
	return nil
}

// Inspect implements Deployer. The first pass for a given inspection walk
// (no armed marker yet, or restartOnFailure asking to discard the
// in-progress one) sets one-time PXE boot and powers the machine on -
// PowerCycle instead of PowerOn when the BMC already reports it on, so a
// machine stuck in an old OS is forced to actually reboot into the PXE
// override. Every later pass polls MachineConditionAgentRegistered and the
// same-name MachineHardware, both written out of band by
// internal/agentserver once kezio-agent boots and registers; Inspect
// itself never talks to the agent.
func (d *AgentDeployer) Inspect(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
	armedAt, armed := pxeArmedAt(machine)
	if !armed || restartOnFailure {
		return d.armPXEAndPowerOn(ctx, machine)
	}
	return d.pollAgentRegistration(ctx, machine, armedAt)
}

// armPXEAndPowerOn is Inspect's first-pass branch: see Inspect's doc
// comment.
func (d *AgentDeployer) armPXEAndPowerOn(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error) {
	bmcClient, err := d.connectBMC(ctx, machine)
	if err != nil {
		return classifyBMCError(err), nil
	}

	if err := bmcClient.SetOneTimePXEBoot(ctx); err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: setting one-time PXE boot: %w", err)), nil
	}

	state, err := bmcClient.GetPowerState(ctx)
	if err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: reading BMC power state: %w", err)), nil
	}
	if state == bmc.PowerStateOn {
		err = bmcClient.PowerCycle(ctx)
	} else {
		err = bmcClient.PowerOn(ctx)
	}
	if err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: powering on machine for net boot: %w", err)), nil
	}

	if err := d.armPXE(ctx, machine); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Continuing}, nil
}

// pollAgentRegistration is Inspect's later-pass branch: see Inspect's doc
// comment. armedAt is when armPXEAndPowerOn last ran, read back from
// agentDeployerPXEArmedAnnotation.
func (d *AgentDeployer) pollAgentRegistration(ctx context.Context, machine *keziov1alpha2.Machine, armedAt time.Time) (Result, error) {
	if apimeta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha2.MachineConditionAgentRegistered) {
		var hw keziov1alpha2.MachineHardware
		key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}
		switch err := d.Client.Get(ctx, key, &hw); {
		case err == nil:
			if err := d.disarmPXE(ctx, machine); err != nil {
				return Result{}, err
			}
			return Result{Outcome: Complete}, nil
		case !apierrors.IsNotFound(err):
			return Result{}, fmt.Errorf("agent deployer: getting MachineHardware %q: %w", machine.Name, err)
		}
	}

	if time.Since(armedAt) > agentDeployerInspectDeadline {
		return Result{
			Outcome:      Failed,
			ErrorType:    keziov1alpha2.MachineErrorTypeRestart,
			ErrorMessage: fmt.Sprintf("agent deployer: no agent registration observed within %s of arming PXE boot", agentDeployerInspectDeadline),
		}, nil
	}
	return Result{Outcome: Continuing}, nil
}

// Provision implements Deployer. AgentDeployer does not drive
// provisioning; it always reports Delayed (never Failed) so the
// controller neither loops through its error backoff nor advances the
// Machine, and never touches run.
func (d *AgentDeployer) Provision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error) {
	return Result{Outcome: Delayed, RequeueAfter: agentDeployerUnsupportedRequeueInterval, ErrorMessage: agentDeployerUnsupportedMessage}, nil
}

// Deprovision implements Deployer, mirroring Provision: see its doc
// comment.
func (d *AgentDeployer) Deprovision(ctx context.Context, machine *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
	return Result{Outcome: Delayed, RequeueAfter: agentDeployerUnsupportedRequeueInterval, ErrorMessage: agentDeployerUnsupportedMessage}, nil
}

// PowerOff implements Deployer: a graceful shutdown request, escalated to
// ForcePowerOff if the BMC still reports the machine on afterward - the
// same escalation ForcePowerOff's own doc comment describes. This does
// not wait for the guest OS to actually finish shutting down; it either
// completes within these two BMC calls or reports the failing one.
func (d *AgentDeployer) PowerOff(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error) {
	bmcClient, err := d.connectBMC(ctx, machine)
	if err != nil {
		return classifyBMCError(err), nil
	}

	if err := bmcClient.PowerOff(ctx); err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: requesting graceful power-off: %w", err)), nil
	}

	state, err := bmcClient.GetPowerState(ctx)
	if err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: reading power state after graceful power-off: %w", err)), nil
	}
	if state == bmc.PowerStateOn {
		if err := bmcClient.ForcePowerOff(ctx); err != nil {
			return classifyBMCError(fmt.Errorf("agent deployer: forcing power off: %w", err)), nil
		}
	}
	return Result{Outcome: Complete}, nil
}

// Reboot implements Deployer. hard maps directly to PowerCycle, the one
// BMC action that forces an immediate reset without waiting for an
// orderly shutdown. The BMC interface has no distinct graceful-reset
// command, so the soft path approximates one: request a graceful
// shutdown, then immediately request power-on - best effort, matching
// Reboot's own contract that it does not wait for the machine to finish
// rebooting.
func (d *AgentDeployer) Reboot(ctx context.Context, machine *keziov1alpha2.Machine, hard bool) (Result, error) {
	bmcClient, err := d.connectBMC(ctx, machine)
	if err != nil {
		return classifyBMCError(err), nil
	}

	if hard {
		if err := bmcClient.PowerCycle(ctx); err != nil {
			return classifyBMCError(fmt.Errorf("agent deployer: power-cycling for a hard reboot: %w", err)), nil
		}
		return Result{Outcome: Complete}, nil
	}

	if err := bmcClient.PowerOff(ctx); err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: requesting graceful shutdown for a soft reboot: %w", err)), nil
	}
	if err := bmcClient.PowerOn(ctx); err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: powering back on for a soft reboot: %w", err)), nil
	}
	return Result{Outcome: Complete}, nil
}
