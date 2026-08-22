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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/bmc"
	"github.com/tjjh89017/kezio/internal/planbuild"
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

// agentDeployerProvisionBootAnnotation records, as JSON
// (agentDeployerProvisionBootMarker), the marker Provision's boot-into-
// agent step needs to survive a manager restart: when it last armed
// one-time PXE boot and powered the machine on for the current DeployRun,
// and the agent session token hash observed at that exact moment.
//
// This is deliberately not agentDeployerPXEArmedAnnotation:
// MachineConditionAgentRegistered is set True once by POST
// /agent/register and never cleared afterward (see internal/agentserver's
// ingestRegistration), so it stays True across every reboot and power-off
// that follows the Inspect that first set it. Provision's boot-into-agent
// step cannot reuse Inspect's "wait for the condition to read True" check
// the way pollAgentRegistration does - it would read True immediately,
// before the machine has even powered back on for this deploy. The
// session token hash baseline lets pollAgentBoot tell a fresh
// registration from this boot apart from whatever stale session an
// earlier Inspect or Provision attempt already left behind: kezio-agent
// mints a brand new session on every registration, unconditionally
// overwriting the old one.
const agentDeployerProvisionBootAnnotation = "kezio.kojuro.date/agent-provision-boot-armed"

// agentDeployerProvisionBootMarker is agentDeployerProvisionBootAnnotation's
// JSON value.
type agentDeployerProvisionBootMarker struct {
	ArmedAt time.Time `json:"armedAt"`
	// BaselineSessionHash is machine.status.agentSession.tokenHash as
	// observed at ArmedAt, or empty when no session existed yet.
	BaselineSessionHash string `json:"baselineSessionHash,omitempty"`
}

// agentDeployerProvisionBootDeadline bounds how long Provision's
// boot-into-agent step waits for kezio-agent to register a fresh session
// after arming PXE boot. Past this, it reports Failed with ErrorType
// Restart so the controller's restartOnFailure re-arms PXE and
// power-cycles the machine on the next attempt, the same reasoning as
// agentDeployerInspectDeadline.
const agentDeployerProvisionBootDeadline = 5 * time.Minute

// agentDeployerUnsupportedMessage is Deprovision's Result.ErrorMessage:
// AgentDeployer does not drive deprovisioning yet.
const agentDeployerUnsupportedMessage = "deprovisioning is not supported by this deployer"

// agentDeployerUnsupportedRequeueInterval is Deprovision's
// Result.RequeueAfter.
const agentDeployerUnsupportedRequeueInterval = time.Hour

// AgentDeployer drives hardware inspection, image provisioning, and power
// through a Machine's BMC (internal/bmc), the live kezio-agent
// registration recorded by internal/agentserver, and the DeployRun phase
// internal/agentserver's POST /agent/progress handler writes as the agent
// executes the plan. AgentDeployer itself never talks to the agent's HTTP
// API directly: internal/agentserver hands out the plan (rebuilding it
// fresh from the same DeployRun on every GET /agent/next - see
// PlanBuilder's doc comment) and records its progress. Provision arms PXE
// boot and power-cycles the machine the same way Inspect does, waits for
// a fresh agent registration, then validates once that a plan can be
// built at all and polls the DeployRun's status.phase.
//
// Both Inspect's "PXE already armed" fact and Provision's own boot marker
// must survive a manager restart (the Deployer interface promises no
// in-memory state between calls), so they are recorded on the Machine as
// agentDeployerPXEArmedAnnotation and agentDeployerProvisionBootAnnotation
// respectively, rather than kept in this struct. status.netBoot is not
// used for this: it is written by the boot config server when the machine
// actually fetches its grub config, a different event than "arm PXE
// boot", and owned by a different component.
type AgentDeployer struct {
	// Client reads and writes Machine, MachineHardware, DeployRun, and the
	// BMC credentials Secret. Required.
	Client client.Client
	// PlanBuilder resolves a Machine's deploy intent into a DeployPlan.
	// Provision calls it, once the live agent has confirmed booted, only
	// to validate that resolution succeeds before committing the
	// DeployRun to Pending and waiting on the agent; the resolved plan
	// itself is discarded here, since internal/agentserver rebuilds an
	// identical one (Build is deterministic in its inputs) for the
	// agent's own GET /agent/next. Required.
	PlanBuilder *planbuild.Builder
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

// provisionBootMarker reads and parses agentDeployerProvisionBootAnnotation
// off machine. The second return is false when the annotation is absent or
// fails to parse - both treated as "not armed yet", never as an error.
func provisionBootMarker(machine *keziov1alpha2.Machine) (agentDeployerProvisionBootMarker, bool) {
	raw, ok := machine.Annotations[agentDeployerProvisionBootAnnotation]
	if !ok {
		return agentDeployerProvisionBootMarker{}, false
	}
	var marker agentDeployerProvisionBootMarker
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return agentDeployerProvisionBootMarker{}, false
	}
	return marker, true
}

// armProvisionBoot stamps agentDeployerProvisionBootAnnotation with the
// current time and the agent session token hash observed right now (empty
// when machine has no session yet), mutating machine in place so the
// caller's copy reflects it immediately.
func (d *AgentDeployer) armProvisionBoot(ctx context.Context, machine *keziov1alpha2.Machine) error {
	baseline := ""
	if machine.Status.AgentSession != nil {
		baseline = machine.Status.AgentSession.TokenHash
	}
	marker := agentDeployerProvisionBootMarker{ArmedAt: time.Now().UTC(), BaselineSessionHash: baseline}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("agent deployer: encoding provision boot marker: %w", err)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	machine.Annotations[agentDeployerProvisionBootAnnotation] = string(data)
	if err := d.Client.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("agent deployer: recording provision-boot marker: %w", err)
	}
	return nil
}

// disarmProvisionBoot removes agentDeployerProvisionBootAnnotation once
// the boot-into-agent step confirms a fresh registration and commits the
// DeployRun to Pending, so a later DeployRun for the same Machine starts
// its own boot-into-agent step from "not armed" instead of reading a stale
// marker left over from this one.
func (d *AgentDeployer) disarmProvisionBoot(ctx context.Context, machine *keziov1alpha2.Machine) error {
	if _, ok := machine.Annotations[agentDeployerProvisionBootAnnotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	delete(machine.Annotations, agentDeployerProvisionBootAnnotation)
	if err := d.Client.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("agent deployer: clearing provision-boot marker: %w", err)
	}
	return nil
}

// agentSessionFresh reports whether machine's current agent session was
// minted after Provision's boot-into-agent step armed PXE boot: a
// registration that happened since then mints a new session token hash
// (internal/agentserver's ingestRegistration always overwrites
// machine.status.agentSession), so any hash other than baseline - the one
// observed at arm time - proves this boot's kezio-agent actually
// registered, rather than a stale session an earlier Inspect or Provision
// attempt already left in place.
func agentSessionFresh(machine *keziov1alpha2.Machine, baseline string) bool {
	session := machine.Status.AgentSession
	return session != nil && session.TokenHash != baseline
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

// Provision implements Deployer. The first pass for a given run
// (status.phase still empty) first gets machine booted into the live
// kezio-agent: it arms one-time PXE boot and powers the machine on
// (armProvisionBootAndPowerOn), then waits for a fresh agent registration
// (pollAgentBoot) before it ever touches run - a Machine reaching
// Provisioning may be powered off, or booted into whatever OS its disk
// carries, so nothing here can be trusted to already be running the live
// agent. Only once that is confirmed does startProvision validate that
// PlanBuilder can resolve a plan at all before committing to wait on the
// agent: a NotReadyError reports Delayed, a DiskSelectionError or
// ValidationError reports Failed with ErrorType/ErrorMessage, and success
// records run at DeployRunPhasePending and reports Continuing. Every later
// pass (run.status.phase no longer empty) reads it, written out of band
// by internal/agentserver's POST /agent/progress handler as the agent
// executes the plan: Succeeded reports Complete, Failed reports Failed
// with ErrorType Restart and the recorded failure message, and every
// other phase reports Continuing. Succeeded is recorded from the agent's
// terminal progress report, sent just before it runs the after-deploy
// reboot/poweroff command - see the Deployer interface's own Provision
// doc comment for what Complete does and does not confirm.
//
// ErrorType Restart for an agent-reported Failed run is deliberate, not
// an oversight: the agent that reported Failed has abandoned its
// attempt (its live process may already be gone), so nothing about
// run's recorded phase/partitions/timings/conditions describes work
// worth resuming - exactly the condition MachineErrorTypeRestart's own
// doc comment describes. The next Provision call therefore arrives with
// restartOnFailure true, which resetProvisionAttempt and startProvision
// turn into a fresh attempt on this same run object: its spec is
// unchanged (the deploy intent it was created for still holds), so
// re-running it from scratch is exactly re-attempting the same goal, not
// a different one. A payload that can never succeed retries forever this
// way; that is surfaced through errorCount/backoff on the
// operationalStatus axis (recordFailure/failedRequeueInterval), the same
// place every other persistent Machine error is surfaced, rather than by
// this deployer refusing to retry.
//
// restartOnFailure asks Provision to discard this attempt's in-progress
// state and start over, mirroring Inspect's own restartOnFailure handling.
// Two pieces of state can be in progress: the provision-boot marker, and
// run's own phase/partitions/timings/conditions once commitProvisionPending
// has written them. resetProvisionAttempt wipes the latter - whatever run
// currently records describes the attempt being abandoned, not the one
// about to start - so run reads as freshly created, and startProvision
// forces a fresh marker (armProvisionBootAndPowerOn always overwrites
// unconditionally) regardless of whether one was already armed, so the
// machine is booted again rather than trusted to still be mid-boot.
func (d *AgentDeployer) Provision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error) {
	if restartOnFailure && run.Status.Phase != "" {
		if err := d.resetProvisionAttempt(ctx, run); err != nil {
			return Result{}, err
		}
	}
	if run.Status.Phase == "" {
		return d.startProvision(ctx, machine, run, restartOnFailure)
	}
	return provisionResultFromPhase(run), nil
}

// resetProvisionAttempt clears run's in-progress attempt state so it reads
// as freshly created: see Provision's restartOnFailure doc comment.
func (d *AgentDeployer) resetProvisionAttempt(ctx context.Context, run *keziov1alpha2.DeployRun) error {
	run.Status.Phase = ""
	run.Status.Partitions = nil
	run.Status.PhaseTimings = nil
	run.Status.Conditions = nil
	if err := d.Client.Status().Update(ctx, run); err != nil {
		return fmt.Errorf("agent deployer: resetting DeployRun %q for restart: %w", run.Name, err)
	}
	return nil
}

// startProvision is Provision's first-pass branch: see Provision's doc
// comment. It dispatches on agentDeployerProvisionBootAnnotation exactly
// as Inspect dispatches on agentDeployerPXEArmedAnnotation: no marker yet,
// or restartOnFailure asking to discard whatever marker is there, arms PXE
// boot and powers on; otherwise a marker present hands off to
// pollAgentBoot.
func (d *AgentDeployer) startProvision(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error) {
	marker, armed := provisionBootMarker(machine)
	if !armed || restartOnFailure {
		return d.armProvisionBootAndPowerOn(ctx, machine)
	}
	return d.pollAgentBoot(ctx, machine, run, marker)
}

// pollAgentBoot is startProvision's later-pass branch: see Provision's
// doc comment. marker is armProvisionBootAndPowerOn's last recorded
// marker, read back from agentDeployerProvisionBootAnnotation. A fresh
// registration proceeds to validate the plan and commit run to Pending;
// otherwise this waits, or gives up past the deadline.
func (d *AgentDeployer) pollAgentBoot(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, marker agentDeployerProvisionBootMarker) (Result, error) {
	if agentSessionFresh(machine, marker.BaselineSessionHash) {
		result, err := d.commitProvisionPending(ctx, machine, run)
		if err != nil {
			return Result{}, err
		}
		// Only a successful commit (run actually moved to Pending) retires
		// the marker: a Failed outcome here is a plan-build problem, not a
		// boot problem, and the agent it already confirmed live must not be
		// power-cycled again just to retry validating the same plan.
		if result.Outcome == Continuing {
			if err := d.disarmProvisionBoot(ctx, machine); err != nil {
				return Result{}, err
			}
		}
		return result, nil
	}

	if time.Since(marker.ArmedAt) > agentDeployerProvisionBootDeadline {
		return Result{
			Outcome:      Failed,
			ErrorType:    keziov1alpha2.MachineErrorTypeRestart,
			ErrorMessage: fmt.Sprintf("agent deployer: no agent registration observed within %s of arming PXE boot for provisioning", agentDeployerProvisionBootDeadline),
		}, nil
	}
	return Result{Outcome: Continuing}, nil
}

// armProvisionBootAndPowerOn is startProvision's first-pass branch:
// arms one-time PXE boot and powers the machine on - PowerCycle instead of
// PowerOn when the BMC already reports it on, mirroring
// armPXEAndPowerOn's reasoning: a machine stuck in an old OS (or the OS
// this same deploy just wrote on a prior attempt) must actually reboot
// into the PXE override, not merely be told it is already running.
func (d *AgentDeployer) armProvisionBootAndPowerOn(ctx context.Context, machine *keziov1alpha2.Machine) (Result, error) {
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

	if err := d.armProvisionBoot(ctx, machine); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Continuing}, nil
}

// commitProvisionPending validates that PlanBuilder can resolve a plan for
// run at all, then records run at DeployRunPhasePending: see Provision's
// doc comment.
func (d *AgentDeployer) commitProvisionPending(ctx context.Context, machine *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun) (Result, error) {
	if _, _, err := d.PlanBuilder.Build(ctx, machine, run); err != nil {
		result, ok := planBuildErrorResult(err)
		if !ok {
			return Result{}, fmt.Errorf("agent deployer: resolving deploy plan for %q: %w", run.Name, err)
		}
		return result, nil
	}

	now := metav1.Now()
	run.Status.Phase = keziov1alpha2.DeployRunPhasePending
	run.Status.PhaseTimings = append(run.Status.PhaseTimings, keziov1alpha2.DeployRunPhaseTiming{
		Phase:     keziov1alpha2.DeployRunPhasePending,
		StartedAt: now,
	})
	if err := d.Client.Status().Update(ctx, run); err != nil {
		return Result{}, fmt.Errorf("agent deployer: recording DeployRun %q pending: %w", run.Name, err)
	}
	return Result{Outcome: Continuing}, nil
}

// planBuildErrorResult maps a planbuild.Builder.Build error to the Result
// Provision reports for it, per the package doc comment's error-shape
// contract. ok is false for any other error - a transient infrastructure
// failure the caller must return as (Result{}, err) instead, so
// controller-runtime's standard requeue-with-error handling applies
// rather than touching Machine error state.
func planBuildErrorResult(err error) (result Result, ok bool) {
	var notReady *planbuild.NotReadyError
	if errors.As(err, &notReady) {
		return Result{Outcome: Delayed}, true
	}

	var diskErr *planbuild.DiskSelectionError
	var validationErr *planbuild.ValidationError
	if errors.As(err, &diskErr) || errors.As(err, &validationErr) {
		return Result{
			Outcome:      Failed,
			ErrorType:    keziov1alpha2.MachineErrorTypeTransient,
			ErrorMessage: err.Error(),
		}, true
	}

	return Result{}, false
}

// provisionResultFromPhase is Provision's later-pass branch: see
// Provision's doc comment.
func provisionResultFromPhase(run *keziov1alpha2.DeployRun) Result {
	switch run.Status.Phase {
	case keziov1alpha2.DeployRunPhaseSucceeded:
		return Result{Outcome: Complete}
	case keziov1alpha2.DeployRunPhaseFailed:
		return Result{
			Outcome:      Failed,
			ErrorType:    keziov1alpha2.MachineErrorTypeRestart,
			ErrorMessage: deployRunFailureMessage(run),
		}
	default:
		return Result{Outcome: Continuing}
	}
}

// deployRunFailureMessage reads the failure detail internal/agentserver
// recorded on run.status.conditions[Succeeded]=False, falling back to a
// generic message if that condition is absent - defensive only, since
// the progress handler always sets it alongside DeployRunPhaseFailed.
func deployRunFailureMessage(run *keziov1alpha2.DeployRun) string {
	cond := apimeta.FindStatusCondition(run.Status.Conditions, keziov1alpha2.DeployRunConditionSucceeded)
	if cond != nil && cond.Message != "" {
		return cond.Message
	}
	return fmt.Sprintf("DeployRun %q failed", run.Name)
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
