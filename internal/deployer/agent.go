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
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bmc"
	"github.com/tjjh89017/kezio/internal/bootserver"
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

// agentDeployerProvisionStallDeadline bounds how long Provision keeps
// reporting Continuing for a non-terminal DeployRun phase with no new
// progress report accepted (status.lastProgressAt unchanged). The agent
// reports at least once per seeding-status poll tick while content is
// written, but a single long-running command - one mkfs, one post hook -
// reports only when it returns, so this must comfortably exceed the
// longest legitimate silent stretch. Past it, Provision reports Failed
// with ErrorType Restart, turning a run whose agent died (or whose
// terminal report was lost) into a diagnosable, retried failure instead
// of a Machine stuck in Provisioning forever.
const agentDeployerProvisionStallDeadline = 30 * time.Minute

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
// respectively, rather than kept in this struct.
//
// armPXEAndPowerOn and armProvisionBootAndPowerOn also mint that boot's
// registration token, through Tokens, at the same moment they set one-time
// PXE and power the machine on: a token belongs to the boot the arm call
// just triggered, not to however many times internal/bootserver's
// grub.cfg handler ends up being fetched before the machine's kernel
// actually boots (see bootserver.TokenStore's doc comment) - that handler
// only ever reads back what was minted here, off the same status.netBoot
// this struct writes.
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
	// Tokens is the in-process store shared with internal/bootserver.
	// Server (see its own Tokens field and bootserver.TokenStore's doc
	// comment), both wired to the same instance in cmd/main.go. Nil (this
	// package's own unit tests, and any Deployer not wired to a live
	// bootserver) skips minting entirely: the machine is still armed and
	// powered on exactly the same, it just boots with no token for
	// bootserver to hand it.
	Tokens *bootserver.TokenStore
	// TokenTTL bounds how long a token armPXEAndPowerOn/
	// armProvisionBootAndPowerOn mints is accepted, from the moment it is
	// minted. Zero (including a Deployer built with no explicit value)
	// means bootserver.DefaultTokenTTL.
	TokenTTL time.Duration
	// Recorder emits the DHCPPoolExhausted Event on a Machine that could
	// not get a DHCP reservation because its Subnet's lease-mode address
	// pool is exhausted (see reserveAndAwaitDHCP). Nil skips emitting the
	// Event; the Machine still reports Delayed and the Subnet condition
	// is still set either way.
	Recorder record.EventRecorder
}

var _ Deployer = (*AgentDeployer)(nil)

// connectBMC resolves machine.spec.bmc.credentialsSecretRef from
// machine's own namespace and connects to its BMC. Errors never include
// the resolved credentials.
func (d *AgentDeployer) connectBMC(ctx context.Context, machine *keziov1alpha3.Machine) (bmc.BMC, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.BMC.CredentialsSecretRef.Name}
	if err := d.Client.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("agent deployer: getting BMC credentials secret %q: %w", key.Name, err)
	}

	creds, err := bmc.CredentialsFromSecretData(secret.Data)
	if err != nil {
		return nil, fmt.Errorf("agent deployer: resolving BMC credentials: %w", err)
	}

	// Compared against exactly "true" here, the one place the trust
	// decision is made: the Machine webhook admits only "true" or "false",
	// so no other spelling can reach this and read as enabled.
	opts := bmc.Options{
		InsecureSkipVerify: machine.Annotations[keziov1alpha3.MachineAnnotationBMCInsecureSkipVerify] == "true",
	}

	bmcClient, err := bmc.Connect(ctx, machine.Spec.BMC.Address, creds, opts)
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
		ErrorType:    keziov1alpha3.MachineErrorTypeTransient,
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

// tokenTTL returns d.TokenTTL, or bootserver.DefaultTokenTTL when it is
// unset.
func (d *AgentDeployer) tokenTTL() time.Duration {
	if d.TokenTTL > 0 {
		return d.TokenTTL
	}
	return bootserver.DefaultTokenTTL
}

// issueBootToken mints and persists the registration token for the net
// boot armPXEAndPowerOn/armProvisionBootAndPowerOn is about to trigger,
// through d.Tokens - see AgentDeployer's own doc comment for why minting
// belongs here rather than to internal/bootserver's grub.cfg handler. A
// nil d.Tokens, or a Machine whose spec.bootMACAddress does not parse,
// skips minting entirely rather than failing the boot attempt: net boot
// itself does not depend on a token existing, only registration does, and
// a Machine deployer wiring or MAC validation problem is diagnosable from
// the resulting "agent never registered" deadline failure without also
// blocking the machine from powering on at all.
func (d *AgentDeployer) issueBootToken(ctx context.Context, machine *keziov1alpha3.Machine) error {
	if d.Tokens == nil {
		return nil
	}
	mac, ok := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		return nil
	}

	token, status, err := d.Tokens.Issue(mac, time.Now(), d.tokenTTL())
	if err != nil {
		return fmt.Errorf("agent deployer: minting boot token: %w", err)
	}
	// The Secret is written, and must land, before status.netBoot: a
	// Machine whose hash is persisted but whose Secret write failed would
	// tell bootserver.Server's Secret fallback a token exists that it can
	// never actually read back after a restart loses the in-memory entry
	// this same call just made.
	if err := d.writeBootTokenSecret(ctx, machine, mac, token, status.ExpiresAt.Time); err != nil {
		return err
	}
	machine.Status.NetBoot = &status
	if err := d.updateMachineStatusRetryOnConflict(ctx, machine); err != nil {
		return fmt.Errorf("agent deployer: persisting boot token hash: %w", err)
	}
	return nil
}

// writeBootTokenSecret create-or-updates the per-Machine Secret that
// mirrors the token issueBootToken just minted, so bootserver.Server's
// grub.cfg handler can recover the plaintext from a Lookup miss after a
// manager restart (see bootserver.TokenStore's doc comment and
// bootserver.BootTokenSecretName). It always overwrites: exactly one
// token is ever outstanding per Machine (TokenStore.Issue's own doc
// comment), so the Secret carries that same replace-in-place semantics
// rather than accumulating history. Owned by machine via a controller
// reference (machineOwnerReference, shared with FakeDeployer's own use
// on MachineHardware) so it is garbage-collected with it, the same way
// internal/agentserver's MachineHardware write is.
func (d *AgentDeployer) writeBootTokenSecret(ctx context.Context, machine *keziov1alpha3.Machine, mac, token string, expiresAt time.Time) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootserver.BootTokenSecretName(machine.Name),
			Namespace: machine.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, d.Client, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		secret.OwnerReferences = []metav1.OwnerReference{machineOwnerReference(machine)}
		secret.Data = map[string][]byte{
			bootserver.BootTokenSecretKeyToken:     []byte(token),
			bootserver.BootTokenSecretKeyMAC:       []byte(mac),
			bootserver.BootTokenSecretKeyExpiresAt: []byte(expiresAt.UTC().Format(time.RFC3339)),
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("agent deployer: persisting boot token secret for %q: %w", machine.Name, err)
	}
	return nil
}

// updateMachineStatusRetryOnConflict persists machine's in-memory status
// to the API server, retrying past an apiserver conflict (observed in
// production: internal/agentserver concurrently writing
// status.agentSession to the same Machine) by re-Getting the current
// object and reapplying the same desired status onto it before retrying
// the update - the fields being persisted are not themselves wrong on a
// conflict, only the resourceVersion they were computed against is
// stale. Tokens.Issue already happened by the time this is called and is
// safe to leave un-repeated across retries: it always replaces whatever
// entry the boot's MAC already held, so retrying only the write here
// never leaves a stale token outstanding.
//
// On return, machine reflects whatever object version was actually
// persisted (or the latest Get, on a failing final attempt), so a caller
// that keeps mutating machine afterward - armPXE's annotation patch, for
// example - computes its diff against a resourceVersion the API server
// still recognizes.
func (d *AgentDeployer) updateMachineStatusRetryOnConflict(ctx context.Context, machine *keziov1alpha3.Machine) error {
	desired := machine.Status.DeepCopy()
	key := client.ObjectKeyFromObject(machine)
	first := true
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if !first {
			if err := d.Client.Get(ctx, key, machine); err != nil {
				return err
			}
			machine.Status = *desired.DeepCopy()
		}
		first = false
		return d.Client.Status().Update(ctx, machine)
	})
}

// pxeArmedAt reads agentDeployerPXEArmedAnnotation off machine. The second
// return is false when the annotation is absent or fails to parse - both
// treated as "not armed yet", never as an error.
func pxeArmedAt(machine *keziov1alpha3.Machine) (time.Time, bool) {
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
func (d *AgentDeployer) armPXE(ctx context.Context, machine *keziov1alpha3.Machine) error {
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
func (d *AgentDeployer) disarmPXE(ctx context.Context, machine *keziov1alpha3.Machine) error {
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
func provisionBootMarker(machine *keziov1alpha3.Machine) (agentDeployerProvisionBootMarker, bool) {
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
func (d *AgentDeployer) armProvisionBoot(ctx context.Context, machine *keziov1alpha3.Machine) error {
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
func (d *AgentDeployer) disarmProvisionBoot(ctx context.Context, machine *keziov1alpha3.Machine) error {
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
func agentSessionFresh(machine *keziov1alpha3.Machine, baseline string) bool {
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
func (d *AgentDeployer) Inspect(ctx context.Context, machine *keziov1alpha3.Machine, restartOnFailure bool) (Result, error) {
	armedAt, armed := pxeArmedAt(machine)
	if !armed || restartOnFailure {
		return d.armPXEAndPowerOn(ctx, machine)
	}
	return d.pollAgentRegistration(ctx, machine, armedAt)
}

// armPXEAndPowerOn is Inspect's first-pass branch: see Inspect's doc
// comment.
//
// Persistence (the boot token and the PXE-armed marker) is committed
// before either BMC call: a BMC action cannot be retried safely (a
// second PowerCycle is a second, physically observable reboot), while a
// failed status/annotation write is - retried here on a conflict
// (updateMachineStatusRetryOnConflict), or simply by this same function
// running again on the next reconcile, since nothing irreversible
// happened yet. If SetOneTimePXEBoot or the power call itself then
// fails, the marker is cleared again (disarmPXE) rather than left
// "armed" for a boot that never actually got a power command: leaving it
// set would make pollAgentRegistration wait out
// agentDeployerInspectDeadline believing a boot is already under way,
// instead of retrying immediately on the next reconcile.
func (d *AgentDeployer) armPXEAndPowerOn(ctx context.Context, machine *keziov1alpha3.Machine) (Result, error) {
	bmcClient, err := d.connectBMC(ctx, machine)
	if err != nil {
		return classifyBMCError(err), nil
	}

	if err := d.issueBootToken(ctx, machine); err != nil {
		return Result{}, err
	}
	if result, proceed, err := d.reserveAndAwaitDHCP(ctx, machine); err != nil {
		return Result{}, err
	} else if !proceed {
		return result, nil
	}
	if err := d.armPXE(ctx, machine); err != nil {
		return Result{}, err
	}

	if result, ok := d.powerOnForNetBoot(ctx, bmcClient); !ok {
		if err := d.disarmPXE(ctx, machine); err != nil {
			return Result{}, err
		}
		return result, nil
	}
	return Result{Outcome: Continuing}, nil
}

// powerOnForNetBoot issues the BMC actions that actually land a machine
// in the live environment for a one-time PXE boot: SetOneTimePXEBoot,
// then PowerCycle (already on) or PowerOn (off) - PowerCycle is used
// when already on so a machine stuck in an old OS is forced to actually
// reboot into the PXE override. ok is false when any call failed; result
// is then the Result the caller should return, and the caller must treat
// the boot as never armed (undo whatever marker it persisted before
// calling this), since a persisted "armed" marker must never outlive
// whether power was actually requested.
func (d *AgentDeployer) powerOnForNetBoot(ctx context.Context, bmcClient bmc.BMC) (result Result, ok bool) {
	if err := bmcClient.SetOneTimePXEBoot(ctx); err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: setting one-time PXE boot: %w", err)), false
	}

	state, err := bmcClient.GetPowerState(ctx)
	if err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: reading BMC power state: %w", err)), false
	}
	if state == bmc.PowerStateOn {
		err = bmcClient.PowerCycle(ctx)
	} else {
		err = bmcClient.PowerOn(ctx)
	}
	if err != nil {
		return classifyBMCError(fmt.Errorf("agent deployer: powering on machine for net boot: %w", err)), false
	}
	return Result{}, true
}

// pollAgentRegistration is Inspect's later-pass branch: see Inspect's doc
// comment. armedAt is when armPXEAndPowerOn last ran, read back from
// agentDeployerPXEArmedAnnotation.
func (d *AgentDeployer) pollAgentRegistration(ctx context.Context, machine *keziov1alpha3.Machine, armedAt time.Time) (Result, error) {
	if apimeta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionAgentRegistered) {
		var hw keziov1alpha3.MachineHardware
		key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}
		switch err := d.Client.Get(ctx, key, &hw); {
		case err == nil:
			if err := d.disarmPXE(ctx, machine); err != nil {
				return Result{}, err
			}
			if err := d.releaseDHCPReservation(ctx, machine); err != nil {
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
			ErrorType:    keziov1alpha3.MachineErrorTypeRestart,
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
// other phase reports Continuing - bounded by
// agentDeployerProvisionStallDeadline against the run's lastProgressAt,
// so a run whose agent has gone silent (or whose terminal report was
// lost) fails with ErrorType Restart instead of being polled forever.
// Succeeded is recorded from the agent's
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
func (d *AgentDeployer) Provision(ctx context.Context, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun, restartOnFailure bool) (Result, error) {
	if restartOnFailure && run.Status.Phase != "" {
		if err := d.resetProvisionAttempt(ctx, run); err != nil {
			return Result{}, err
		}
	}
	if run.Status.Phase == "" {
		return d.startProvision(ctx, machine, run, restartOnFailure)
	}
	return d.provisionResultFromPhase(ctx, machine, run)
}

// resetProvisionAttempt clears run's in-progress attempt state so it reads
// as freshly created: see Provision's restartOnFailure doc comment.
func (d *AgentDeployer) resetProvisionAttempt(ctx context.Context, run *keziov1alpha3.DeployRun) error {
	run.Status.Phase = ""
	run.Status.Partitions = nil
	run.Status.PhaseTimings = nil
	run.Status.LastProgressAt = nil
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
func (d *AgentDeployer) startProvision(ctx context.Context, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun, restartOnFailure bool) (Result, error) {
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
func (d *AgentDeployer) pollAgentBoot(ctx context.Context, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun, marker agentDeployerProvisionBootMarker) (Result, error) {
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
			ErrorType:    keziov1alpha3.MachineErrorTypeRestart,
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
//
// Persistence and BMC-failure handling mirror armPXEAndPowerOn's own doc
// comment exactly, substituting the provision-boot marker
// (armProvisionBoot/disarmProvisionBoot) for the PXE-armed annotation.
func (d *AgentDeployer) armProvisionBootAndPowerOn(ctx context.Context, machine *keziov1alpha3.Machine) (Result, error) {
	bmcClient, err := d.connectBMC(ctx, machine)
	if err != nil {
		return classifyBMCError(err), nil
	}

	if err := d.issueBootToken(ctx, machine); err != nil {
		return Result{}, err
	}
	if result, proceed, err := d.reserveAndAwaitDHCP(ctx, machine); err != nil {
		return Result{}, err
	} else if !proceed {
		return result, nil
	}
	if err := d.armProvisionBoot(ctx, machine); err != nil {
		return Result{}, err
	}

	if result, ok := d.powerOnForNetBoot(ctx, bmcClient); !ok {
		if err := d.disarmProvisionBoot(ctx, machine); err != nil {
			return Result{}, err
		}
		return result, nil
	}
	return Result{Outcome: Continuing}, nil
}

// commitProvisionPending validates that PlanBuilder can resolve a plan for
// run at all, then records run at DeployRunPhasePending: see Provision's
// doc comment.
func (d *AgentDeployer) commitProvisionPending(ctx context.Context, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun) (Result, error) {
	claim, err := resolveClaimIntent(ctx, d.Client, machine)
	if err != nil {
		return Result{}, fmt.Errorf("agent deployer: resolving MachineClaim for %q: %w", machine.Name, err)
	}
	if _, _, err := d.PlanBuilder.Build(ctx, machine, claim, run); err != nil {
		result, ok := planBuildErrorResult(err)
		if !ok {
			return Result{}, fmt.Errorf("agent deployer: resolving deploy plan for %q: %w", run.Name, err)
		}
		return result, nil
	}

	now := metav1.Now()
	run.Status.Phase = keziov1alpha3.DeployRunPhasePending
	run.Status.PhaseTimings = append(run.Status.PhaseTimings, keziov1alpha3.DeployRunPhaseTiming{
		Phase:     keziov1alpha3.DeployRunPhasePending,
		StartedAt: now,
	})
	// Start the stall clock at the commit itself: an agent that never
	// fetches the plan sends no report, so nothing else would ever set
	// lastProgressAt and the Pending wait would be unbounded.
	run.Status.LastProgressAt = &now
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
			ErrorType:    keziov1alpha3.MachineErrorTypeTransient,
			ErrorMessage: err.Error(),
		}, true
	}

	return Result{}, false
}

// provisionResultFromPhase is Provision's later-pass branch: see
// Provision's doc comment. Succeeded also releases machine's DHCP
// reservation, if any, the same way Inspect's own Complete does.
func (d *AgentDeployer) provisionResultFromPhase(ctx context.Context, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun) (Result, error) {
	switch run.Status.Phase {
	case keziov1alpha3.DeployRunPhaseSucceeded:
		if err := d.releaseDHCPReservation(ctx, machine); err != nil {
			return Result{}, err
		}
		return Result{Outcome: Complete}, nil
	case keziov1alpha3.DeployRunPhaseFailed:
		return Result{
			Outcome:      Failed,
			ErrorType:    keziov1alpha3.MachineErrorTypeRestart,
			ErrorMessage: deployRunFailureMessage(run),
		}, nil
	default:
		return provisionStallResult(run), nil
	}
}

// provisionStallResult reports Continuing for a non-terminal phase still
// making progress, or Failed with ErrorType Restart once no progress
// report has been accepted for agentDeployerProvisionStallDeadline -
// see that constant's doc comment. The baseline is
// status.lastProgressAt; a run written before that field existed falls
// back to the current phase's own start time, and a run with neither
// (only reachable by hand-written status) is left Continuing rather
// than failed on a clock this deployer never started.
func provisionStallResult(run *keziov1alpha3.DeployRun) Result {
	last := run.Status.LastProgressAt
	if last == nil {
		if n := len(run.Status.PhaseTimings); n > 0 {
			last = &run.Status.PhaseTimings[n-1].StartedAt
		}
	}
	if last == nil || time.Since(last.Time) <= agentDeployerProvisionStallDeadline {
		return Result{Outcome: Continuing}
	}
	return Result{
		Outcome:      Failed,
		ErrorType:    keziov1alpha3.MachineErrorTypeRestart,
		ErrorMessage: fmt.Sprintf("agent deployer: no deploy progress observed for %s with DeployRun %q in phase %s; treating the run as stalled", agentDeployerProvisionStallDeadline, run.Name, run.Status.Phase),
	}
}

// deployRunFailureMessage reads the failure detail internal/agentserver
// recorded on run.status.conditions[Succeeded]=False, falling back to a
// generic message if that condition is absent - defensive only, since
// the progress handler always sets it alongside DeployRunPhaseFailed.
func deployRunFailureMessage(run *keziov1alpha3.DeployRun) string {
	cond := apimeta.FindStatusCondition(run.Status.Conditions, keziov1alpha3.DeployRunConditionSucceeded)
	if cond != nil && cond.Message != "" {
		return cond.Message
	}
	return fmt.Sprintf("DeployRun %q failed", run.Name)
}

// Deprovision implements Deployer. It touches neither the BMC nor the
// machine's disk, only releasing machine's DHCP reservation (if any) on
// its Subnet before reporting Complete: every other piece of state this
// deployer manages - agentDeployerPXEArmedAnnotation,
// agentDeployerProvisionBootAnnotation, and status.netBoot - lives on the
// Machine object itself, so it is already gone the moment the object is
// removed, satisfying the interface's Complete contract with nothing left
// to do for those.
//
// kezio deliberately does not erase the deployed system as part of
// deleting a Machine. Deleting a Machine removes it from kezio's
// inventory - the record that this hardware is kezio's to manage - it is
// not a request to destroy what was last written to its disk. Wiping the
// disk on delete would turn an inventory edit into a destructive action
// on hardware kezio no longer even tracks once the object is gone, with
// no way to undo it if the delete was a mistake or the Machine is meant
// to be re-enrolled elsewhere. The disk is left exactly as the last
// Provision (or Inspect, if the machine was never provisioned) wrote it;
// a caller that wants the data actually destroyed must do that before,
// or independently of, deleting the Machine.
func (d *AgentDeployer) Deprovision(ctx context.Context, machine *keziov1alpha3.Machine, restartOnFailure bool) (Result, error) {
	if err := d.releaseDHCPReservation(ctx, machine); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Complete}, nil
}

// PowerOff implements Deployer: a graceful shutdown request, escalated to
// ForcePowerOff if the BMC still reports the machine on afterward - the
// same escalation ForcePowerOff's own doc comment describes. This does
// not wait for the guest OS to actually finish shutting down; it either
// completes within these two BMC calls or reports the failing one.
func (d *AgentDeployer) PowerOff(ctx context.Context, machine *keziov1alpha3.Machine) (Result, error) {
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
func (d *AgentDeployer) Reboot(ctx context.Context, machine *keziov1alpha3.Machine, hard bool) (Result, error) {
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
