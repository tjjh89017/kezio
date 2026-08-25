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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/deployer"
	"github.com/tjjh89017/kezio/internal/planbuild"
)

// Requeue intervals for the states a Deployer step can leave a Machine in
// without changing the Machine object itself (a DeployRun phase advance
// writes to the DeployRun, not the Machine, so nothing here re-triggers the
// watch that normally drives the next reconcile).
const (
	continuingRequeueInterval  = time.Second
	defaultBusyRequeueInterval = 10 * time.Second
	failedRequeueInterval      = 30 * time.Second
)

// delayedRequeueInterval is the fixed requeue delay for
// deployer.Delayed - a package variable, not a const, so a test can
// shorten it instead of waiting out the real interval.
var delayedRequeueInterval = 15 * time.Second

// credentialsSecretAbsentRequeueInterval is the fixed requeue delay when
// the BMC credentials Secret a Machine references does not exist yet - a
// package variable, not a const, so a test can shorten it.
var credentialsSecretAbsentRequeueInterval = 10 * time.Second

// jitter adds 0-25% to a success-path requeue interval (continuing, busy,
// delayed - a deployer step that ran without failing): the same fixed
// interval on every Machine would otherwise line them up into a lockstep
// reconcile storm. It is a package variable, not a plain function, so a
// test can replace it with the identity function and keep exact interval
// assertions. The failed-outcome backoff (failedRequeueInterval) is
// deliberately not jittered here - it is error backoff, not a
// success-path requeue.
var jitter = func(d time.Duration) time.Duration {
	return d + time.Duration(rand.Float64()*0.25*float64(d))
}

// deleteStageMaxErrorCount is the delete walk's per-stage give-up
// threshold: a stage gives up once its errorCount exceeds this value,
// advancing to the next stage instead of retrying forever against a dead
// BMC.
const deleteStageMaxErrorCount = 3

// machineDeleteStageGiveUpTotal counts every time a Machine delete stage
// gave up after exceeding deleteStageMaxErrorCount and advanced anyway,
// labeled by stage name. Registered once at package load: safe against the
// duplicate-registration panic that a per-reconciler registration would
// risk if more than one MachineReconciler is built in the same process.
var machineDeleteStageGiveUpTotal = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
	Name: "kezio_machine_delete_stage_giveups_total",
	Help: "Total number of Machine delete-walk stages that exceeded their retry limit and advanced anyway.",
}, []string{"stage"})

// machineControllerFieldOwner is the Server-Side Apply field manager identity
// for every Machine status write this reconciler makes. Kept as a stable
// string (not derived from a binary name or hostname) so managedFields stays
// readable and consistent across restarts and replicas.
const machineControllerFieldOwner = "kezio-machine-controller"

// MachineReconciler reconciles a Machine object
type MachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Deployer drives hardware inspection and image provisioning. Required.
	Deployer deployer.Deployer
	// Recorder emits Kubernetes Events on Machine, notably the
	// accept/refuse outcome of a re-inspect annotation. Required.
	Recorder record.EventRecorder
	// Builder resolves a Machine's deploy intent into the DeployPlan
	// snapshot (ResolvedDisks/HooksHash) a fresh DeployRun records, and
	// lets the provisioning trigger see a hooks-only change (a PostHook
	// or postHookRefs edit with the same imageRef/dataImages). Optional:
	// nil keeps the pre-snapshot fast lane - every DeployRun is created
	// with an empty ResolvedDisks/HooksHash, exactly as before this field
	// existed. A caller that wants snapshot resolution (cmd/main.go's
	// production wiring) must set it explicitly; existing envtest/
	// FakeDeployer callers that construct a MachineReconciler directly
	// intentionally leave it unset.
	Builder *planbuild.Builder
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machinehardwares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var machine keziov1alpha3.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Absolute: paused returns immediately even while the Machine has a
	// deletion timestamp, before the finalizer/onDelete dispatch below.
	if isPaused(&machine) {
		return ctrl.Result{}, nil
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, &machine)
	}

	if !controllerutil.ContainsFinalizer(&machine, keziov1alpha3.MachineFinalizer) {
		controllerutil.AddFinalizer(&machine, keziov1alpha3.MachineFinalizer)
		if err := r.Update(ctx, &machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.onChange(ctx, &machine)
}

// onDelete drives the delete walk while machine has a deletion timestamp:
// deprovision, then power off, then release. It never resolves the BMC
// credentials Secret through the forward path's blocking gate - a
// deletion in progress must proceed with empty credentials rather than
// wedge on a secret that is absent or unreadable.
func (r *MachineReconciler) onDelete(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, keziov1alpha3.MachineFinalizer) {
		return ctrl.Result{}, nil
	}

	switch machine.Status.State {
	case keziov1alpha3.MachineStateDeprovisioning:
		return r.reconcileDeprovisioning(ctx, machine)
	case keziov1alpha3.MachineStatePoweringOff:
		return r.reconcilePoweringOff(ctx, machine)
	default:
		// Any other recorded state (including empty) means the delete
		// walk has not started yet: enter it at the first stage.
		return r.advanceDeleteStage(ctx, machine, keziov1alpha3.MachineStateDeprovisioning)
	}
}

// reconcileDeprovisioning runs the deprovision delete stage, or skips it
// straight to the next stage when the detached annotation is present: a
// detached Machine never dials the BMC-adjacent deployer, on the forward
// walk or the delete walk alike.
func (r *MachineReconciler) reconcileDeprovisioning(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if isDetached(machine) {
		return r.advanceDeleteStage(ctx, machine, keziov1alpha3.MachineStatePoweringOff)
	}
	step := func(ctx context.Context, m *keziov1alpha3.Machine) (deployer.Result, error) {
		return r.Deployer.Deprovision(ctx, m, restartOnFailure(m))
	}
	return r.runDeleteStage(ctx, machine, step, "deprovision", keziov1alpha3.MachineStatePoweringOff)
}

// reconcilePoweringOff runs the power-off delete stage, or skips it straight
// to release when detached - see reconcileDeprovisioning.
func (r *MachineReconciler) reconcilePoweringOff(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if isDetached(machine) {
		return r.advanceDeleteStage(ctx, machine, "")
	}
	step := func(ctx context.Context, m *keziov1alpha3.Machine) (deployer.Result, error) {
		return r.Deployer.PowerOff(ctx, m)
	}
	// An empty nextState tells runDeleteStage/advanceDeleteStage this is
	// the last stage: Complete or give-up both go straight to release.
	return r.runDeleteStage(ctx, machine, step, "power-off", "")
}

// runDeleteStage calls one delete-walk step and applies its outcome. A
// transient error (non-nil err) is returned unchanged, exactly like the
// forward walk: no errorCount/state change, controller-runtime retries with
// its own backoff. Complete advances to nextState (or releases the Machine
// when nextState is empty). Continuing/Busy go through clearRetryStatus
// (restarting always false: no delete-stage step returns a
// Restart-classified Failed today) so a prior Delayed outcome does not
// outlive it, matching applyNonCompleteOutcome; Delayed itself requeues
// without touching error state. Failed goes through
// recordDeleteStageFailure, which applies the give-up threshold.
func (r *MachineReconciler) runDeleteStage(ctx context.Context, machine *keziov1alpha3.Machine, step func(context.Context, *keziov1alpha3.Machine) (deployer.Result, error), stageName, nextState string) (ctrl.Result, error) {
	result, err := step(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch result.Outcome {
	case deployer.Complete:
		return r.advanceDeleteStage(ctx, machine, nextState)
	case deployer.Continuing:
		return r.clearRetryStatus(ctx, machine, false, ctrl.Result{RequeueAfter: jitter(continuingRequeueInterval)})
	case deployer.Busy:
		requeueAfter := jitter(defaultBusyRequeueInterval)
		if result.RequeueAfter > 0 {
			requeueAfter = result.RequeueAfter
		}
		return r.clearRetryStatus(ctx, machine, false, ctrl.Result{RequeueAfter: requeueAfter})
	case deployer.Delayed:
		return r.markDelayed(ctx, machine, delayedRequeueInterval)
	case deployer.Failed:
		return r.recordDeleteStageFailure(ctx, machine, result, stageName, nextState)
	default:
		return ctrl.Result{}, fmt.Errorf("machine %q: delete stage %q returned unrecognized outcome %v", machine.Name, stageName, result.Outcome)
	}
}

// recordDeleteStageFailure applies the same two-axis error bookkeeping as
// the forward walk's recordFailure, then checks the give-up threshold: once
// errorCount exceeds deleteStageMaxErrorCount, the stage gives up rather
// than retry forever against, for example, a dead BMC. Giving up logs at
// error level, bumps machineDeleteStageGiveUpTotal, and advances exactly as
// a Complete outcome would - a dead stage must never block the machine's
// removal.
func (r *MachineReconciler) recordDeleteStageFailure(ctx context.Context, machine *keziov1alpha3.Machine, result deployer.Result, stageName, nextState string) (ctrl.Result, error) {
	applyFailure(machine, result.ErrorType, result.ErrorMessage)
	giveUp := machine.Status.ErrorCount > deleteStageMaxErrorCount
	stampLastUpdated(machine)

	var onSuccess []func()
	if giveUp {
		onSuccess = append(onSuccess, func() {
			logf.FromContext(ctx).Error(errors.New("delete stage exceeded its retry limit"),
				"giving up on Machine delete stage and advancing", "machine", machine.Name, "stage", stageName, "errorCount", machine.Status.ErrorCount)
			machineDeleteStageGiveUpTotal.WithLabelValues(stageName).Inc()
		})
	}
	if err := r.applyMachineStatus(ctx, machine, onSuccess...); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording delete stage %q failure: %w", machine.Name, stageName, err)
	}
	if !giveUp {
		return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
	}
	return r.advanceDeleteStage(ctx, machine, nextState)
}

// advanceDeleteStage moves the delete walk to nextState, resetting
// errorCount/operationalStatus the same way setState does for the forward
// walk - stage exit is the reset point, whether the stage exited by
// succeeding or by giving up. An empty nextState means there is no next
// stage: release the Machine instead of writing a state. The status write
// that records nextState does not itself re-trigger the watch (the Machine
// update predicate ignores status-only changes), so this requeues
// explicitly to run the next delete stage.
func (r *MachineReconciler) advanceDeleteStage(ctx context.Context, machine *keziov1alpha3.Machine, nextState string) (ctrl.Result, error) {
	if nextState == "" {
		return r.releaseMachine(ctx, machine)
	}

	machine.Status.State = nextState
	markOperational(machine)
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: advancing delete walk to %q: %w", machine.Name, nextState, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

// releaseMachine is the delete walk's terminal step: release the
// credentials Secret's sub-finalizer before the Machine's own finalizer,
// since once the Machine is actually removed nothing maps a dangling
// Secret owner reference back to a live reconcile that could still clear
// it.
func (r *MachineReconciler) releaseMachine(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if err := r.releaseCredentialsSecretFinalizer(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(machine, keziov1alpha3.MachineFinalizer)
	return ctrl.Result{}, r.Update(ctx, machine)
}

// onChange drives one step of the state walk per call: Enrolling ->
// Inspecting -> Available -> Provisioning -> Provisioned, plus the
// Available/Provisioned re-trigger. It never advances more than one step:
// the status-only writes along the way do not re-trigger the Machine watch,
// so every step that must run again returns an explicit requeue.
func (r *MachineReconciler) onChange(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if isDetached(machine) {
		return r.reconcileDetached(ctx, machine)
	}
	if machine.Status.OperationalStatus == keziov1alpha3.MachineOperationalStatusDetached {
		return r.clearDetached(ctx, machine)
	}

	if requested, hard := rebootRequested(ctx, machine); requested {
		return r.reconcileReboot(ctx, machine, hard)
	}

	if reInspectRequested(machine) {
		return r.reconcileReInspect(ctx, machine)
	}

	if hasUnknownErrorType(machine) {
		// A live currentRunRef only makes sense during an active
		// Provisioning walk; re-enrolling abandons that walk, so the ref
		// must go with it - otherwise the machine could reach
		// Available/Provisioned still pointing at an orphaned DeployRun.
		machine.Status.CurrentRunRef = nil
		return r.setState(ctx, machine, keziov1alpha3.MachineStateEnrolling)
	}

	if machine.Spec.ClaimRef == nil && releaseEligible(machine.Status.State) {
		machine.Status.CurrentRunRef = nil
		return r.setState(ctx, machine, keziov1alpha3.MachineStateReleased, func() {
			r.Recorder.Event(machine, corev1.EventTypeNormal, "MachineReleased",
				"claimRef is gone: moving to Released with the disk left as it is")
		})
	}

	switch machine.Status.State {
	case "":
		return r.reconcileEmptyStatus(ctx, machine)
	case keziov1alpha3.MachineStateEnrolling:
		return r.setState(ctx, machine, keziov1alpha3.MachineStateInspecting)
	case keziov1alpha3.MachineStateInspecting:
		return r.reconcileInspecting(ctx, machine)
	case keziov1alpha3.MachineStateAvailable, keziov1alpha3.MachineStateProvisioned:
		return r.reconcileIdle(ctx, machine)
	case keziov1alpha3.MachineStateProvisioning:
		return r.reconcileProvisioning(ctx, machine)
	default:
		return ctrl.Result{}, fmt.Errorf("machine %q: unknown status.state %q", machine.Name, machine.Status.State)
	}
}

// machineStatusApplyBody is the Server-Side Apply request body
// applyMachineStatus sends: identity plus status, deliberately with no Spec
// field at all. MachineSpec has several non-omitempty-eligible required
// fields (non-pointer structs, and string fields with no omitempty), so
// marshaling any *keziov1alpha3.Machine value - even one built with a
// zero-valued Spec - puts a populated "spec" object in the request body.
// Sent to the status subresource, the API server validates that body
// against the whole resource's schema, and a zero-valued required Spec
// fails that validation. A distinct type with no Spec field sidesteps this
// rather than fighting it field by field.
//
// Metadata is a narrow stand-in for metav1.ObjectMeta (name/namespace only,
// the only fields this body ever sets), not the real type: controller-gen's
// CRD generator treats any type embedding both metav1.TypeMeta and
// metav1.ObjectMeta as a Kubernetes object needing a CRD, and this package
// carries no group/version to give one, which produced a stray empty
// config/crd/bases/_.yaml on every `make manifests` run.
type machineStatusApplyBody struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        machineStatusApplyBodyMetadata `json:"metadata,omitempty"`
	Status          keziov1alpha3.MachineStatus    `json:"status"`
}

// machineStatusApplyBodyMetadata mirrors the JSON shape of the
// metav1.ObjectMeta fields machineStatusApplyBody actually sets.
type machineStatusApplyBodyMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// applyMachineStatus writes machine.Status through Server-Side Apply under
// machineControllerFieldOwner, and - only once the write succeeds - runs
// each of onSuccess in order. onSuccess is where a status write's event or
// metric emission belongs: a callback describes a state that is true only
// once the write has actually landed, so a failed write must drop every
// queued callback along with the error.
//
// The apply body carries machine.Status wholesale (not just the fields this
// call intends to change): MachineStatus has two non-pointer struct fields
// (GoodCredentials, TriedCredentials) that encoding/json's omitempty cannot
// elide, so a genuinely field-scoped Apply body is not reachable through the
// generated Go types without an ApplyConfiguration or a hand-built
// unstructured object. Sending the full, current status (never a
// mostly-zero placeholder) keeps every field's last real value intact
// across writes; it does mean this owns every populated status field on
// every write, not a disjoint subset. That is safe today because the
// Machine controller is the only writer of Machine.status - a future writer
// of disjoint fields needs a narrower patch shape than this one.
func (r *MachineReconciler) applyMachineStatus(ctx context.Context, machine *keziov1alpha3.Machine, onSuccess ...func()) error {
	body := machineStatusApplyBody{
		TypeMeta: metav1.TypeMeta{APIVersion: keziov1alpha3.GroupVersion.String(), Kind: "Machine"},
		Metadata: machineStatusApplyBodyMetadata{Name: machine.Name, Namespace: machine.Namespace},
		Status:   machine.Status,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("machine %q: encoding status apply body: %w", machine.Name, err)
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	if err := r.Status().Patch(ctx, machine, patch, client.FieldOwner(machineControllerFieldOwner), client.ForceOwnership); err != nil {
		return err
	}
	for _, cb := range onSuccess {
		cb()
	}
	return nil
}

// setState patches machine.status.state and clears any error, leaving every
// other status field untouched. It also clears MachineConditionStatusLossHold
// if present: any forward move out of the empty state means the hold (if any)
// has already been resolved by reconcileEmptyStatus/reconcileStatusLossHold.
// The status write itself does not re-trigger the watch (the Machine update
// predicate ignores status-only changes), so this requeues explicitly to run
// the next walk step. onSuccess callbacks (an event, typically) run only
// after the write succeeds - see applyMachineStatus.
func (r *MachineReconciler) setState(ctx context.Context, machine *keziov1alpha3.Machine, state string, onSuccess ...func()) (ctrl.Result, error) {
	machine.Status.State = state
	markOperational(machine)
	machine.Status.ErrorCount = 0
	meta.RemoveStatusCondition(&machine.Status.Conditions, keziov1alpha3.MachineConditionStatusLossHold)
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine, onSuccess...); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to %q: %w", machine.Name, state, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileEmptyStatus handles status.state == "": either a Machine that has
// never been reconciled before, or one already caught in the status-loss
// hold from an earlier reconcile (statusLossHeld persists across reconciles
// even after stampLastUpdated makes status.lastUpdated non-nil, since the
// hold's own entry is itself a status write).
//
// Detecting the hold on a truly fresh Machine is the tension this guards
// against: a Machine created with spec.imageRef already set looks
// byte-for-byte identical, at this exact point, to one whose status a
// restore just wiped (status.lastUpdated absent, spec carrying intent). The
// two are told apart by asking whether any DeployRun already exists for this
// Machine's name: a genuinely fresh Machine cannot have one (this
// reconciler is the only thing that ever creates one, and it has not run
// for this Machine yet), while a Machine that was actually provisioned
// before the restore still owns its DeployRun history, GC retention aside.
func (r *MachineReconciler) reconcileEmptyStatus(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if statusLossHeld(machine) {
		return r.reconcileStatusLossHold(ctx, machine)
	}

	if machine.Status.LastUpdated == nil {
		claim, err := resolveClaimIntent(ctx, r.Client, machine)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !isEmptyDeployPayload(claim) {
			suspect, err := r.hasPriorDeployRuns(ctx, machine)
			if err != nil {
				return ctrl.Result{}, err
			}
			if suspect {
				return r.enterStatusLossHold(ctx, machine)
			}
		}
	}

	return r.setState(ctx, machine, keziov1alpha3.MachineStateEnrolling)
}

// hasPriorDeployRuns reports whether at least one DeployRun already names
// machine in its spec.machineRef, in machine's own namespace.
func (r *MachineReconciler) hasPriorDeployRuns(ctx context.Context, machine *keziov1alpha3.Machine) (bool, error) {
	var runs keziov1alpha3.DeployRunList
	if err := r.List(ctx, &runs, client.InNamespace(machine.Namespace)); err != nil {
		return false, fmt.Errorf("machine %q: listing DeployRuns for status-loss detection: %w", machine.Name, err)
	}
	for _, run := range runs.Items {
		if run.Spec.MachineRef.Name == machine.Name {
			return true, nil
		}
	}
	return false, nil
}

// statusLossHeld reports whether machine currently carries an active
// status-loss hold.
func statusLossHeld(machine *keziov1alpha3.Machine) bool {
	return meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionStatusLossHold)
}

// enterStatusLossHold records the hold and emits a Warning Event. No
// requeue: nothing here resolves on its own, only an operator editing the
// confirm annotation (or the spec) does, and that arrives through the
// normal watch.
func (r *MachineReconciler) enterStatusLossHold(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:    keziov1alpha3.MachineConditionStatusLossHold,
		Status:  metav1.ConditionTrue,
		Reason:  "PriorDeployRunsFound",
		Message: "status was never recorded but spec carries deploy intent and DeployRuns already exist for this machine; holding until " + keziov1alpha3.MachineAnnotationConfirmStatusLoss + " is set",
	})
	stampLastUpdated(machine)
	onSuccess := func() {
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "StatusLossHold",
			"status was never recorded but spec carries deploy intent and DeployRuns already exist for this machine; holding until the %q annotation is set", keziov1alpha3.MachineAnnotationConfirmStatusLoss)
	}
	if err := r.applyMachineStatus(ctx, machine, onSuccess); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: entering status-loss hold: %w", machine.Name, err)
	}
	return ctrl.Result{}, nil
}

// reconcileStatusLossHold is reached on every reconcile while
// MachineConditionStatusLossHold is True. Absent the confirm annotation it
// returns without any write or requeue - the hold only lifts through an
// operator-driven annotation/spec edit, which retriggers this reconciler on
// its own through the watch. Once observed, the annotation is consumed
// (never left in place) before the walk resumes at Enrolling.
func (r *MachineReconciler) reconcileStatusLossHold(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if _, confirmed := machine.Annotations[keziov1alpha3.MachineAnnotationConfirmStatusLoss]; !confirmed {
		return ctrl.Result{}, nil
	}

	if err := r.consumeConfirmStatusLossAnnotation(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return r.setState(ctx, machine, keziov1alpha3.MachineStateEnrolling, func() {
		r.Recorder.Event(machine, corev1.EventTypeNormal, "StatusLossHoldCleared",
			"confirm-status-loss annotation accepted: resuming the normal state walk from Enrolling")
	})
}

// consumeConfirmStatusLossAnnotation removes
// MachineAnnotationConfirmStatusLoss from machine, mirroring
// consumeReInspectAnnotation's consume-then-delete discipline.
func (r *MachineReconciler) consumeConfirmStatusLossAnnotation(ctx context.Context, machine *keziov1alpha3.Machine) error {
	patch := client.MergeFrom(machine.DeepCopy())
	delete(machine.Annotations, keziov1alpha3.MachineAnnotationConfirmStatusLoss)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: consuming confirm-status-loss annotation: %w", machine.Name, err)
	}
	return nil
}

// reconcileInspecting drives one step of hardware inspection, or - when the
// inspect-disable annotation is set - skips inspection entirely: no
// MachineHardware is created, and spec.bootMACAddress (mandatory in that
// case, per the Machine webhook) carries the identity instead.
func (r *MachineReconciler) reconcileInspecting(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if inspectDisabled(machine) {
		return r.setState(ctx, machine, keziov1alpha3.MachineStateAvailable)
	}

	secret, blocked, gateResult, err := r.resolveCredentialsSecret(ctx, machine)
	if blocked {
		return gateResult, err
	}
	if err := r.recordTriedCredentials(ctx, machine, secret); err != nil {
		return ctrl.Result{}, err
	}
	restarting := restartOnFailure(machine)
	result, err := r.Deployer.Inspect(ctx, machine, restarting)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.Outcome != deployer.Failed {
		if err := r.recordGoodCredentials(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}
	if result.Outcome == deployer.Complete {
		return r.setState(ctx, machine, keziov1alpha3.MachineStateAvailable)
	}
	return r.applyNonCompleteOutcome(ctx, machine, result, restarting)
}

// reInspectAcceptable reports whether machine's current status.state honors
// a re-inspect annotation right now. Available always accepts. Provisioned
// accepts only when the deploy payload is empty - this is the only
// Provisioned -> Available path, and it is what lets a released machine
// re-enter service. A Provisioned machine with a set deploy payload always
// refuses, and every other state refuses.
func (r *MachineReconciler) reInspectAcceptable(ctx context.Context, machine *keziov1alpha3.Machine) (bool, error) {
	switch machine.Status.State {
	case keziov1alpha3.MachineStateAvailable:
		return true, nil
	case keziov1alpha3.MachineStateProvisioned:
		claim, err := resolveClaimIntent(ctx, r.Client, machine)
		if err != nil {
			return false, err
		}
		return isEmptyDeployPayload(claim), nil
	case keziov1alpha3.MachineStateReleased:
		// A released machine's deploy payload is empty by construction
		// (no claimRef, no intent): re-inspect is the only path back to
		// Available, and it is always accepted here.
		return true, nil
	default:
		return false, nil
	}
}

// reconcileReInspect handles the re-inspect annotation: consume-then-delete,
// with a Kubernetes Event either way. The annotation is always removed here,
// exactly once, regardless of whether it is honored - a Machine outside the
// acceptable state must not keep re-triggering this branch on every
// reconcile. On acceptance the existing MachineHardware is deleted (never
// patched) and the machine moves to Inspecting, where the normal inspection
// walk creates a fresh one.
func (r *MachineReconciler) reconcileReInspect(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	accepted, err := r.reInspectAcceptable(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.consumeReInspectAnnotation(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}

	if !accepted {
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ReInspectRefused",
			"re-inspect annotation refused: machine is in state %q; re-inspect is accepted in %q, or in %q with an empty deploy payload",
			machine.Status.State, keziov1alpha3.MachineStateAvailable, keziov1alpha3.MachineStateProvisioned)
		return ctrl.Result{}, nil
	}

	if err := r.deleteMachineHardware(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return r.setState(ctx, machine, keziov1alpha3.MachineStateInspecting, func() {
		r.Recorder.Event(machine, corev1.EventTypeNormal, "ReInspectAccepted",
			"re-inspect annotation accepted: MachineHardware deleted, machine moving to Inspecting")
	})
}

// consumeReInspectAnnotation removes MachineAnnotationReInspect from
// machine. Called exactly once per reconcile that observes the annotation,
// before acting on it, so a crash or a refuse decision can never leave the
// annotation to re-trigger this branch forever.
func (r *MachineReconciler) consumeReInspectAnnotation(ctx context.Context, machine *keziov1alpha3.Machine) error {
	patch := client.MergeFrom(machine.DeepCopy())
	delete(machine.Annotations, keziov1alpha3.MachineAnnotationReInspect)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: consuming re-inspect annotation: %w", machine.Name, err)
	}
	return nil
}

// deleteMachineHardware deletes the MachineHardware named after machine, if
// any. It never patches: re-inspection always produces a genuinely new
// object (a new UID) through the normal inspection walk's create call, not
// an update of the old one.
func (r *MachineReconciler) deleteMachineHardware(ctx context.Context, machine *keziov1alpha3.Machine) error {
	hw := &keziov1alpha3.MachineHardware{ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace}}
	if err := r.Delete(ctx, hw); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("machine %q: deleting MachineHardware for re-inspection: %w", machine.Name, err)
	}
	return nil
}

// reconcileIdle handles Available and Provisioned: both are idle states
// that only leave through the same provisioning trigger. Before a
// provisioning run actually starts, this gates on spec.imageRef's Image
// being resolvable and Ready per the cross-reference contract (see
// imageReadyForProvisioning) - a Machine with no imageRef never reaches
// that gate at all.
func (r *MachineReconciler) reconcileIdle(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	lastRun, err := r.lastSuccessfulRun(ctx, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: reading last successful DeployRun: %w", machine.Name, err)
	}
	claim, err := resolveClaimIntent(ctx, r.Client, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: resolving MachineClaim: %w", machine.Name, err)
	}
	if claimUnresolved(machine, claim) {
		return r.markDelayedNotReady(ctx, machine, "ClaimUnresolved", claimUnresolvedMessage(machine))
	}
	if isEmptyDeployPayload(claim) {
		return ctrl.Result{}, nil
	}

	ready, reason, message, err := r.imageReadyForProvisioning(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.markDelayedNotReady(ctx, machine, reason, message)
	}

	hooksHash, ok, result, err := r.currentHooksHash(ctx, machine, claim)
	if !ok {
		return result, err
	}
	if !shouldProvision(claim, lastRun, hooksHash) {
		return ctrl.Result{}, nil
	}

	meta.RemoveStatusCondition(&machine.Status.Conditions, keziov1alpha3.MachineConditionProgressing)
	return r.startProvisioningRun(ctx, machine, claim)
}

// imageReadyForProvisioning gates the provisioning trigger on claim's
// spec.imageRef's referenced Image, applying the cross-reference contract
// aggregateSlotContents documents (see conditionObservedGenerationStale):
// a missing Image, one with no or non-True Ready condition, or one whose
// Ready condition is stale all report not-ready, naming why in reason and
// message. A nil claim or a nil ImageRef reports ready unconditionally
// with no lookup at all - the fast lane (no OS image, or a dataImages-only
// deploy) never depended on an Image existing and must keep working
// exactly as before.
func (r *MachineReconciler) imageReadyForProvisioning(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ready bool, reason, message string, err error) {
	if claim == nil {
		return true, "", "", nil
	}
	ref := claim.Spec.ImageRef
	if ref == nil {
		return true, "", "", nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = claim.Namespace
	}
	var image keziov1alpha3.Image
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, &image); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "ImageNotFound", fmt.Sprintf("Image %q not found", ref.Name), nil
		}
		return false, "", "", fmt.Errorf("machineclaim %q: getting Image %q: %w", claim.Name, ref.Name, err)
	}

	cond := meta.FindStatusCondition(image.Status.Conditions, keziov1alpha3.ImageConditionReady)
	switch {
	case cond == nil:
		return false, "ImageNotReady", fmt.Sprintf("Image %q has no Ready condition yet", ref.Name), nil
	case conditionObservedGenerationStale(cond, image.Generation):
		return false, "ImageStatusStale", fmt.Sprintf("Image %q status has not caught up to its current generation", ref.Name), nil
	case cond.Status != metav1.ConditionTrue:
		return false, "ImageNotReady", fmt.Sprintf("Image %q is not Ready (%s)", ref.Name, cond.Reason), nil
	default:
		return true, "", "", nil
	}
}

// classifyPlanBuildError sorts a planbuild.Builder.Build error into the
// three shapes that package's doc comment fixes: notReady means "try
// again later" (a Delayed outcome), failed means the plan itself cannot
// resolve as currently specified (DiskSelectionError or ValidationError -
// a Failed outcome), and neither means a transient infrastructure error
// the caller must return unchanged.
func classifyPlanBuildError(err error) (notReady, failed bool) {
	var nr *planbuild.NotReadyError
	if errors.As(err, &nr) {
		return true, false
	}
	var diskErr *planbuild.DiskSelectionError
	var validationErr *planbuild.ValidationError
	if errors.As(err, &diskErr) || errors.As(err, &validationErr) {
		return false, true
	}
	return false, false
}

// currentHooksHash resolves the hooksHash the provisioning trigger
// compares against lastRun's recorded snapshot: empty (ok) when r.Builder
// is nil (the pre-snapshot fast lane - see Builder's own doc comment), or
// whatever r.Builder.Build resolves right now otherwise. ok is false when
// the caller must return (result, err) as-is instead of continuing the
// walk: a NotReadyError delays without touching error state, a
// DiskSelectionError/ValidationError records a Failed provision step
// (the same shape a Deployer.Provision failure would), and any other
// error is transient and returned unchanged. The DeployRun passed to
// Build is a throwaway (never created): only its resolved Snapshot is
// used here, not the DeployPlan Build also returns.
func (r *MachineReconciler) currentHooksHash(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) (hooksHash string, ok bool, result ctrl.Result, err error) {
	if r.Builder == nil {
		return "", true, ctrl.Result{}, nil
	}

	_, snapshot, buildErr := r.Builder.Build(ctx, machine, claim, &keziov1alpha3.DeployRun{})
	if buildErr == nil {
		return snapshot.HooksHash, true, ctrl.Result{}, nil
	}

	notReady, failed := classifyPlanBuildError(buildErr)
	switch {
	case notReady:
		result, err = r.markDelayedNotReady(ctx, machine, "PlanNotReady", buildErr.Error())
	case failed:
		result, err = r.recordFailure(ctx, machine, deployer.Result{
			Outcome:      deployer.Failed,
			ErrorType:    keziov1alpha3.MachineErrorTypeTransient,
			ErrorMessage: buildErr.Error(),
		})
	default:
		err = fmt.Errorf("machine %q: resolving deploy plan snapshot: %w", machine.Name, buildErr)
	}
	return "", false, result, err
}

// markDelayedNotReady records why the Machine is waiting - an imageRef
// whose Image is not usable yet, or a plan that cannot build yet - as
// MachineConditionProgressing=False, then delegates to markDelayed for the
// actual delayed-axis write. Without this the reason is lost entirely:
// markDelayed leaves errorMessage alone, so a delayed Machine otherwise
// shows only whatever unrelated error was recorded last, if any.
//
// The condition uses machine.Generation, not the referenced object's, per
// the same observedGeneration-per-writer convention every other condition
// in this codebase follows.
func (r *MachineReconciler) markDelayedNotReady(ctx context.Context, machine *keziov1alpha3.Machine, reason, message string) (ctrl.Result, error) {
	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineConditionProgressing,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: machine.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.markDelayed(ctx, machine, delayedRequeueInterval)
}

// startProvisioningRun creates a fresh DeployRun for machine and records it
// as the current run, moving (or keeping) status.state at Provisioning.
// Used both to enter Provisioning from an idle state and to recover from a
// current run that disappeared mid-Provisioning. The status write does not
// itself re-trigger the watch (the Machine update predicate ignores
// status-only changes), so this requeues explicitly to run the deployer.
//
// createDeployRun's plan-build error (see its own doc comment) is mapped
// exactly like currentHooksHash's: a NotReadyError delays without
// touching error state, a DiskSelectionError/ValidationError records a
// Failed provision step, and anything else is a transient error returned
// unchanged.
func (r *MachineReconciler) startProvisioningRun(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	run, err := r.createDeployRun(ctx, machine, claim)
	if err != nil {
		notReady, failed := classifyPlanBuildError(err)
		switch {
		case notReady:
			return r.markDelayedNotReady(ctx, machine, "PlanNotReady", err.Error())
		case failed:
			return r.recordFailure(ctx, machine, deployer.Result{
				Outcome:      deployer.Failed,
				ErrorType:    keziov1alpha3.MachineErrorTypeTransient,
				ErrorMessage: err.Error(),
			})
		default:
			return ctrl.Result{}, fmt.Errorf("machine %q: creating DeployRun: %w", machine.Name, err)
		}
	}

	machine.Status.State = keziov1alpha3.MachineStateProvisioning
	machine.Status.CurrentRunRef = &keziov1alpha3.NameRef{Name: run.Name}
	markOperational(machine)
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to Provisioning: %w", machine.Name, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	// A nil currentRunRef while still Provisioning is not an invariant
	// violation: recordCurrentRunDeleted clears it after the run
	// disappears out from under an in-progress deployment, and this is
	// where the retry-in-place picks it back up with a fresh run.
	if machine.Status.CurrentRunRef == nil {
		claim, err := resolveClaimIntent(ctx, r.Client, machine)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("machine %q: resolving MachineClaim: %w", machine.Name, err)
		}
		if claimUnresolved(machine, claim) {
			return r.markDelayedNotReady(ctx, machine, "ClaimUnresolved", claimUnresolvedMessage(machine))
		}
		return r.startProvisioningRun(ctx, machine, claim)
	}

	run, err := r.getRun(ctx, machine, machine.Status.CurrentRunRef)
	if apierrors.IsNotFound(err) {
		return r.recordCurrentRunDeleted(ctx, machine)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: getting DeployRun %q: %w", machine.Name, machine.Status.CurrentRunRef.Name, err)
	}

	secret, blocked, gateResult, err := r.resolveCredentialsSecret(ctx, machine)
	if blocked {
		return gateResult, err
	}
	if err := r.recordTriedCredentials(ctx, machine, secret); err != nil {
		return ctrl.Result{}, err
	}
	restarting := restartOnFailure(machine)
	result, err := r.Deployer.Provision(ctx, machine, run, restarting)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.Outcome != deployer.Failed {
		if err := r.recordGoodCredentials(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}
	if result.Outcome != deployer.Complete {
		if result.Outcome == deployer.Failed {
			// The only place a reference to a failed run is kept:
			// applyNonCompleteOutcome writes this field with the failure it
			// records, in the same patch.
			machine.Status.LastAttemptedRunRef = &keziov1alpha3.NameRef{Name: run.Name}
		}
		return r.applyNonCompleteOutcome(ctx, machine, result, restarting)
	}

	machine.Status.State = keziov1alpha3.MachineStateProvisioned
	machine.Status.LastSuccessfulRunRef = &keziov1alpha3.NameRef{Name: run.Name}
	machine.Status.LastAttemptedRunRef = &keziov1alpha3.NameRef{Name: run.Name}
	markOperational(machine)
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to Provisioned: %w", machine.Name, err)
	}
	// The status write above does not itself re-trigger the watch, so this
	// requeues explicitly - reconcileIdle re-checks the provisioning trigger
	// once on entry to Provisioned, the same as it does on entry to
	// Available.
	return ctrl.Result{Requeue: true}, nil
}

// restartOnFailure derives the deployer's restartOnFailure argument from
// the previous attempt's recorded error: only a live error (OK is never
// stale-restart) with errorType MachineErrorTypeRestart asks the deployer
// to discard in-progress step state; every other case, including an
// unrecognized errorType, resumes the step in place.
func restartOnFailure(machine *keziov1alpha3.Machine) bool {
	return machine.Status.OperationalStatus == keziov1alpha3.MachineOperationalStatusError &&
		machine.Status.ErrorType == keziov1alpha3.MachineErrorTypeRestart
}

// releaseEligible reports whether state is one a Machine can be mid-way
// through or have completed when its claimRef disappears: Provisioning or
// Provisioned. Available/Enrolling/Inspecting never carry a claimRef to
// begin with, so a cleared claimRef is a no-op for them.
func releaseEligible(state string) bool {
	return state == keziov1alpha3.MachineStateProvisioning || state == keziov1alpha3.MachineStateProvisioned
}

// hasUnknownErrorType reports whether the machine is recorded as errored
// with an errorType outside the known set (Transient, Restart): corrupt
// data or a value written by an incompatible controller version. An empty
// errorType is the no-error case, not unknown, so it does not match.
func hasUnknownErrorType(machine *keziov1alpha3.Machine) bool {
	if machine.Status.OperationalStatus != keziov1alpha3.MachineOperationalStatusError {
		return false
	}
	switch machine.Status.ErrorType {
	case "", keziov1alpha3.MachineErrorTypeTransient, keziov1alpha3.MachineErrorTypeRestart:
		return false
	default:
		return true
	}
}

// reconcileDetached keeps operationalStatus=detached current while the
// detached annotation is present, without ever calling the deployer: state,
// errorType, errorCount, and every deployer-derived field are frozen
// exactly as they were the moment detached was set.
func (r *MachineReconciler) reconcileDetached(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	if machine.Status.OperationalStatus == keziov1alpha3.MachineOperationalStatusDetached {
		return ctrl.Result{}, nil
	}
	machine.Status.OperationalStatus = keziov1alpha3.MachineOperationalStatusDetached
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording detached status: %w", machine.Name, err)
	}
	return ctrl.Result{}, nil
}

// clearDetached resumes normal operation once the detached annotation is
// removed: it marks the Machine operational in its own patch, so the
// next reconcile's state walk starts from a clean status - the same
// one-step-per-reconcile discipline as clearRetryStatus. The annotation removal
// that reached this reconcile already happened; nothing further will
// re-trigger the watch, so this requeues explicitly to resume the walk.
func (r *MachineReconciler) clearDetached(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	markOperational(machine)
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: clearing detached status: %w", machine.Name, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileReboot honors a reboot annotation outside deletion, regardless
// of machine.status.state: a BMC reboot command does not depend on which
// workflow step the machine is in. Complete clears the suffixless
// annotation; every other outcome goes through applyNonCompleteOutcome
// (the same two-axis error/delay bookkeeping as Inspect/Provision) and
// leaves every reboot annotation untouched, so the reboot is retried as-is
// on the next reconcile.
func (r *MachineReconciler) reconcileReboot(ctx context.Context, machine *keziov1alpha3.Machine, hard bool) (ctrl.Result, error) {
	result, err := r.Deployer.Reboot(ctx, machine, hard)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.Outcome != deployer.Complete {
		// Reboot carries no restartOnFailure argument (Deployer.Reboot's
		// own signature has none), so there is never a Restart-classified
		// error for this call to clear early.
		return r.applyNonCompleteOutcome(ctx, machine, result, false)
	}
	if err := r.clearSuffixlessRebootAnnotation(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// applyNonCompleteOutcome handles every deployer.Result.Outcome except
// Complete, shared between the Inspecting and Provisioning steps.
// restarting is the restartOnFailure value the caller passed into the
// deployer step that produced result: see clearRetryStatus's doc comment
// for why Continuing/Busy need it.
func (r *MachineReconciler) applyNonCompleteOutcome(ctx context.Context, machine *keziov1alpha3.Machine, result deployer.Result, restarting bool) (ctrl.Result, error) {
	switch result.Outcome {
	case deployer.Continuing:
		return r.clearRetryStatus(ctx, machine, restarting, ctrl.Result{RequeueAfter: jitter(continuingRequeueInterval)})
	case deployer.Busy:
		requeueAfter := jitter(defaultBusyRequeueInterval)
		if result.RequeueAfter > 0 {
			requeueAfter = result.RequeueAfter
		}
		return r.clearRetryStatus(ctx, machine, restarting, ctrl.Result{RequeueAfter: requeueAfter})
	case deployer.Delayed:
		return r.recordDelayed(ctx, machine)
	case deployer.Failed:
		return r.recordFailure(ctx, machine, result)
	default:
		return ctrl.Result{}, fmt.Errorf("machine %q: deployer step returned unrecognized outcome %v", machine.Name, result.Outcome)
	}
}

// recordDelayed marks machine delayed and requeues after
// delayedRequeueInterval, the fixed delay for a deployer.Delayed outcome.
func (r *MachineReconciler) recordDelayed(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	return r.markDelayed(ctx, machine, delayedRequeueInterval)
}

// markDelayed marks machine delayed: state, errorType, and errorCount are
// left untouched - delayed is not an error, so it never joins the error
// backoff/reset accounting. The caller always requeues after requeueAfter,
// a fixed delay independent of any error backoff.
func (r *MachineReconciler) markDelayed(ctx context.Context, machine *keziov1alpha3.Machine, requeueAfter time.Duration) (ctrl.Result, error) {
	machine.Status.OperationalStatus = keziov1alpha3.MachineOperationalStatusDelayed
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording delayed status: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: jitter(requeueAfter)}, nil
}

// clearRetryStatus clears whichever advisory status no longer applies now
// that a Continuing/Busy outcome means the deployer step made progress
// without failing: a previously recorded delayed status, so "delayed"
// never outlives the condition that caused it (only Failed's own path
// overwrites an error status; delayed is otherwise unconditional here). It
// is a no-op (result returned unchanged) when neither applies.
//
// restarting additionally clears a Restart-classified error's
// operationalStatus the instant the one call it was granted for succeeds:
// MachineErrorType's own doc comment scopes restartOnFailure to "the
// deployer's next Inspect/Provision call", and nothing else would ever
// clear it here - Continuing/Busy do not otherwise touch operationalStatus
// at all, so a Restart error left in place would keep restartOnFailure
// true on every following reconcile. For AgentDeployer that means its
// arm-PXE-and-power-on branches (`!armed || restartOnFailure`) would
// re-arm and power-cycle the machine on every reconcile forever, instead
// of ever reaching the poll branch that waits for it to actually
// register.
//
// errorCount is deliberately left untouched here, unlike the Complete/
// setState paths: a restart is not a success, only a discarded attempt at
// the same unfinished goal (applyFailure's own doc comment: errorCount
// "counts consecutive errors since the last success in the current
// state"), so a run stuck failing the same way every restart still shows
// a climbing errorCount for backoff/observability - this only unblocks
// the deployer from being asked to restart forever.
func (r *MachineReconciler) clearRetryStatus(ctx context.Context, machine *keziov1alpha3.Machine, restarting bool, result ctrl.Result) (ctrl.Result, error) {
	switch {
	case machine.Status.OperationalStatus == keziov1alpha3.MachineOperationalStatusDelayed:
		markOperational(machine)
	case restarting && machine.Status.OperationalStatus == keziov1alpha3.MachineOperationalStatusError:
		markOperational(machine)
	default:
		return result, nil
	}
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: clearing retry status: %w", machine.Name, err)
	}
	return result, nil
}

func (r *MachineReconciler) recordFailure(ctx context.Context, machine *keziov1alpha3.Machine, result deployer.Result) (ctrl.Result, error) {
	applyFailure(machine, result.ErrorType, result.ErrorMessage)
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording deployer failure: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
}

// applyFailure applies the two-axis error semantics shared by every failure
// path: state is left untouched, only operationalStatus/errorType/
// errorMessage/errorCount change.
func applyFailure(machine *keziov1alpha3.Machine, errorType keziov1alpha3.MachineErrorType, errorMessage string) {
	machine.Status.OperationalStatus = keziov1alpha3.MachineOperationalStatusError
	machine.Status.ErrorType = errorType
	machine.Status.ErrorMessage = errorMessage
	machine.Status.ErrorCount++
}

// markOperational is applyFailure's inverse and the single place
// operationalStatus goes back to OK: errorType/errorMessage describe the
// error status they were written with, so they must never outlive it - a
// healthy machine still displaying the last failure it ever had reads as
// current state, and has already sent one investigation chasing a failure
// that was not happening. Every reset path calls this rather than spelling
// the reset out again; those fields drifted out of step precisely because
// the reset was written separately in each place.
//
// errorCount is deliberately not reset here: its reset points are not the
// same set (clearRetryStatus clears the error status but keeps the count -
// see its own doc comment), so callers that do reset it say so themselves.
func markOperational(machine *keziov1alpha3.Machine) {
	machine.Status.OperationalStatus = keziov1alpha3.MachineOperationalStatusOK
	machine.Status.ErrorType = ""
	machine.Status.ErrorMessage = ""
}

// recordCurrentRunDeleted handles the current DeployRun having disappeared
// out from under a Provisioning machine - GC or an operator deleting it
// mid-run, which internal/agentserver's POST /agent/progress handler
// (see NewAbortDecider) also uses to make the live kezio-agent stop and
// report its own attempt failed before this ever runs. This is reported
// through the same recordFailure semantics as any other phase failure
// (state unchanged, operationalStatus=error) rather than a parallel error
// path, and currentRunRef is cleared in the same patch so the next
// Provisioning reconcile's nil-ref branch starts a fresh run.
func (r *MachineReconciler) recordCurrentRunDeleted(ctx context.Context, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	// MachineErrorTypeRestart: the run this errorType would ask to resume no
	// longer exists, so the only meaningful next step is starting over, not
	// resuming in-progress step state.
	applyFailure(machine, keziov1alpha3.MachineErrorTypeRestart, fmt.Sprintf("current DeployRun %q no longer exists", machine.Status.CurrentRunRef.Name))
	machine.Status.CurrentRunRef = nil
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording current-run-deleted failure: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
}

// createDeployRun creates the DeployRun for one provisioning pass, copying
// imageRef and dataImages from claim.spec. When r.Builder is set, it also
// resolves machine's full deploy plan through it and writes the resulting
// Snapshot (ResolvedDisks, HooksHash) into the run's spec before Create -
// DeployRunSpec is immutable once the object exists, so this must happen
// before the create call. A Build failure is returned unchanged (the
// caller, startProvisioningRun, maps it through classifyPlanBuildError);
// no DeployRun is created in that case.
//
// r.Builder nil keeps the pre-snapshot fast lane: ResolvedDisks/HooksHash
// stay empty, exactly as before this field existed. See Builder's own doc
// comment for why the existing envtest/FakeDeployer suites rely on this
// (their fixture Machines/Images are not shaped for a real plan build).
func (r *MachineReconciler) createDeployRun(ctx context.Context, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) (*keziov1alpha3.DeployRun, error) {
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: machine.Name + "-",
			Namespace:    machine.Namespace,
		},
		Spec: keziov1alpha3.DeployRunSpec{
			MachineRef: keziov1alpha3.NameRef{Name: machine.Name},
			ClaimRef:   machine.Spec.ClaimRef.DeepCopy(),
			ImageRef:   claim.Spec.ImageRef.DeepCopy(),
			DataImages: append([]keziov1alpha3.MachineDataImage(nil), claim.Spec.DataImages...),
		},
	}
	if r.Builder != nil {
		_, snapshot, err := r.Builder.Build(ctx, machine, claim, run)
		if err != nil {
			return nil, err
		}
		planbuild.ApplySnapshot(run, snapshot)
	}
	if err := controllerutil.SetControllerReference(machine, run, r.Scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}
	if err := r.Create(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// lastSuccessfulRun resolves machine.status.lastSuccessfulRunRef. A missing
// ref, or a ref whose DeployRun no longer exists, both mean "no successful
// run known" rather than an error: an operator can delete old DeployRuns
// without wedging the provisioning trigger.
func (r *MachineReconciler) lastSuccessfulRun(ctx context.Context, machine *keziov1alpha3.Machine) (*keziov1alpha3.DeployRun, error) {
	if machine.Status.LastSuccessfulRunRef == nil {
		return nil, nil
	}
	run, err := r.getRun(ctx, machine, machine.Status.LastSuccessfulRunRef)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return run, err
}

func (r *MachineReconciler) getRun(ctx context.Context, machine *keziov1alpha3.Machine, ref *keziov1alpha3.NameRef) (*keziov1alpha3.DeployRun, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = machine.Namespace
	}
	var run keziov1alpha3.DeployRun
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// shouldProvision is the provisioning trigger. It fires only when claim's
// spec.imageRef/spec.dataImages describe some deploy intent and that
// intent's subset - including hooksHash - differs from the last
// successful run's recorded snapshot. A missing lastRun - no
// lastSuccessfulRunRef yet, or one whose DeployRun was deleted - is "no
// successful run known": a non-empty payload triggers once, and the
// resulting run's own success writes a fresh lastSuccessfulRunRef, so
// this path cannot repeat into a storm. hooksHash is the caller's
// currently resolved value (see MachineReconciler.currentHooksHash) - an
// empty string when hook resolution is not wired in (no Builder), which
// only ever compares equal to a lastRun that itself carries no
// HooksHash.
func shouldProvision(claim *keziov1alpha3.MachineClaim, lastRun *keziov1alpha3.DeployRun, hooksHash string) bool {
	if isEmptyDeployPayload(claim) {
		return false
	}
	if lastRun == nil {
		return true
	}
	return !intentSubsetEqual(claim, lastRun, hooksHash)
}

// isEmptyDeployPayload reports whether claim.spec carries no deploy
// intent at all: no claim, no OS image, and no data images. An empty
// payload never triggers a run - clearing intent means "nothing to do",
// not "wipe".
func isEmptyDeployPayload(claim *keziov1alpha3.MachineClaim) bool {
	return claim == nil || (claim.Spec.ImageRef == nil && len(claim.Spec.DataImages) == 0)
}

// intentSubsetEqual compares the provisioning trigger's intent subset -
// imageRef, dataImages, hooksHash - against the last successful run's
// recorded spec. This is full equality, not a superset check, so removing
// a dataImages entry triggers correctly. resolvedDisks is deliberately
// excluded: device names are not stable across boots, and a
// re-resolution must never diff into a disk-wiping redeploy.
func intentSubsetEqual(claim *keziov1alpha3.MachineClaim, lastRun *keziov1alpha3.DeployRun, hooksHash string) bool {
	return nameRefEqual(claim.Spec.ImageRef, lastRun.Spec.ImageRef) &&
		reflect.DeepEqual(claim.Spec.DataImages, lastRun.Spec.DataImages) &&
		hooksHash == lastRun.Spec.HooksHash
}

func nameRefEqual(a, b *keziov1alpha3.NameRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Namespace == b.Namespace
}

// resolveCredentialsSecret resolves, labels, owns, and finalizes the BMC
// credentials Secret named by spec.bmc.credentialsSecretRef, before an
// attempt (Inspect/Provision) that needs it.
//
// An empty credentialsSecretRef.Name is a spec-invalid condition: the walk
// cannot proceed and never will until an operator edits the spec, which
// triggers its own reconcile through the generation-change update
// predicate. This returns (nil, true, ctrl.Result{}, nil) - blocked, no
// requeue, no error, and no status change (no errorCount increase): there
// is nothing to retry.
//
// A NotFound Secret is a transient, resolvable condition: this returns
// (nil, true, <requeue after credentialsSecretAbsentRequeueInterval>, nil)
// and marks the machine delayed, the same "unresolved reference" treatment
// as any other delayed outcome - it clears on its own once a subsequent
// Deployer call succeeds or reports Continuing/Busy.
//
// Otherwise this claims the Secret (label, owner reference, finalizer) and
// returns (secret, false, ctrl.Result{}, nil): the caller proceeds.
func (r *MachineReconciler) resolveCredentialsSecret(ctx context.Context, machine *keziov1alpha3.Machine) (*corev1.Secret, bool, ctrl.Result, error) {
	name := machine.Spec.BMC.CredentialsSecretRef.Name
	if name == "" {
		return nil, true, ctrl.Result{}, nil
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, true, ctrl.Result{}, fmt.Errorf("machine %q: getting BMC credentials secret %q: %w", machine.Name, name, err)
		}
		result, err := r.markDelayed(ctx, machine, credentialsSecretAbsentRequeueInterval)
		return nil, true, result, err
	}

	if err := r.claimCredentialsSecret(ctx, machine, &secret); err != nil {
		return nil, true, ctrl.Result{}, err
	}
	return &secret, false, ctrl.Result{}, nil
}

// claimCredentialsSecret stamps secret with the kezio BMC-credentials
// label, a non-controller owner reference to machine, and
// MachineCredentialsSecretFinalizer, patching only what is missing.
//
// The owner reference is deliberately non-controller
// (controllerutil.SetOwnerReference, not SetControllerReference): the
// Secret is user-created and this claim must never fail with "already
// owned by a different controller" if more than one Machine names the same
// Secret. Kubernetes garbage collection honors any owner reference
// (controller or not) once this Machine is actually deleted, cascading the
// Secret's own deletion - releaseCredentialsSecretFinalizer (run from
// onDelete) is what lets that deletion complete.
func (r *MachineReconciler) claimCredentialsSecret(ctx context.Context, machine *keziov1alpha3.Machine, secret *corev1.Secret) error {
	patch := client.MergeFrom(secret.DeepCopy())
	changed := false

	if secret.Labels[keziov1alpha3.MachineCredentialsSecretLabel] != annotationValueTrue {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[keziov1alpha3.MachineCredentialsSecretLabel] = annotationValueTrue
		changed = true
	}

	hasOwner, err := controllerutil.HasOwnerReference(secret.OwnerReferences, machine, r.Scheme)
	if err != nil {
		return fmt.Errorf("machine %q: checking BMC credentials secret %q owner reference: %w", machine.Name, secret.Name, err)
	}
	if !hasOwner {
		if err := controllerutil.SetOwnerReference(machine, secret, r.Scheme); err != nil {
			return fmt.Errorf("machine %q: setting BMC credentials secret %q owner reference: %w", machine.Name, secret.Name, err)
		}
		changed = true
	}

	if controllerutil.AddFinalizer(secret, keziov1alpha3.MachineCredentialsSecretFinalizer) {
		changed = true
	}

	if !changed {
		return nil
	}
	if err := r.Patch(ctx, secret, patch); err != nil {
		return fmt.Errorf("machine %q: claiming BMC credentials secret %q: %w", machine.Name, secret.Name, err)
	}
	return nil
}

// releaseCredentialsSecretFinalizer removes MachineCredentialsSecretFinalizer
// from the BMC credentials Secret machine references, if any. An empty
// credentialsSecretRef.Name or an already-gone Secret are both no-ops. This
// is a forward-only release for the one Machine that owned the finalizer
// removal call, not a check against other Machines that might name the
// same Secret - sharing one BMC credentials Secret across Machines is not
// guarded here.
func (r *MachineReconciler) releaseCredentialsSecretFinalizer(ctx context.Context, machine *keziov1alpha3.Machine) error {
	name := machine.Spec.BMC.CredentialsSecretRef.Name
	if name == "" {
		return nil
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: machine.Namespace, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("machine %q: getting BMC credentials secret %q for finalizer release: %w", machine.Name, name, err)
	}

	if !controllerutil.RemoveFinalizer(&secret, keziov1alpha3.MachineCredentialsSecretFinalizer) {
		return nil
	}
	if err := r.Update(ctx, &secret); err != nil {
		return fmt.Errorf("machine %q: releasing BMC credentials secret %q finalizer: %w", machine.Name, name, err)
	}
	return nil
}

// recordTriedCredentials records secret's name and resourceVersion as
// status.triedCredentials, before an attempt (Inspect/Provision) that would
// use it. secret is the Secret resolveCredentialsSecret already resolved
// and claimed for this attempt. A goodCredentials resourceVersion mismatch
// against the current Secret forces re-registration: goodCredentials is
// cleared here, in the same patch, without touching status.state.
//
// This is the one status write that stays on the pre-SSA merge patch
// (client.MergeFrom) rather than applyMachineStatus: goodStale clears
// GoodCredentials to its zero value, and GoodCredentials/TriedCredentials
// are non-pointer struct fields, so a structural schema without an
// explicit "nullable: true" rejects the Server-Side Apply merge that
// results - the API server reported the merged field as invalid type
// "null" once every subfield was removed. Making these fields pointers
// would let SSA clear them cleanly, but that is a schema change outside
// this function's scope.
func (r *MachineReconciler) recordTriedCredentials(ctx context.Context, machine *keziov1alpha3.Machine, secret *corev1.Secret) error {
	observed := keziov1alpha3.MachineCredentialsStatus{
		SecretRef:       &keziov1alpha3.SecretReference{Name: secret.Name},
		ResourceVersion: secret.ResourceVersion,
	}
	goodStale := machine.Status.GoodCredentials.SecretRef != nil &&
		!credentialsStatusEqual(machine.Status.GoodCredentials, observed)
	if credentialsStatusEqual(machine.Status.TriedCredentials, observed) && !goodStale {
		return nil
	}

	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.TriedCredentials = observed
	if goodStale {
		machine.Status.GoodCredentials = keziov1alpha3.MachineCredentialsStatus{}
	}
	stampLastUpdated(machine)
	// FieldOwner is honored on a merge patch too, even though this is not a
	// Server-Side Apply request: every Machine status write attributes to
	// the same manager identity regardless of which of the two write paths
	// made it.
	if err := r.Status().Patch(ctx, machine, patch, client.FieldOwner(machineControllerFieldOwner)); err != nil {
		return fmt.Errorf("machine %q: recording tried BMC credentials: %w", machine.Name, err)
	}
	return nil
}

// recordGoodCredentials copies status.triedCredentials into
// status.goodCredentials once a Deployer call returns without a transient
// error and without Outcome == Failed - the modeled stand-in for "the BMC
// answered" in a codebase whose fake path never dials a real BMC. A no-op
// when goodCredentials is already up to date.
func (r *MachineReconciler) recordGoodCredentials(ctx context.Context, machine *keziov1alpha3.Machine) error {
	if machine.Status.TriedCredentials.SecretRef == nil {
		return nil
	}
	if credentialsStatusEqual(machine.Status.GoodCredentials, machine.Status.TriedCredentials) {
		return nil
	}

	machine.Status.GoodCredentials = machine.Status.TriedCredentials
	stampLastUpdated(machine)
	if err := r.applyMachineStatus(ctx, machine); err != nil {
		return fmt.Errorf("machine %q: recording good BMC credentials: %w", machine.Name, err)
	}
	return nil
}

// credentialsStatusEqual compares two MachineCredentialsStatus values by
// value, since SecretRef is a pointer and the zero value (no observation
// yet) must compare equal to itself.
func credentialsStatusEqual(a, b keziov1alpha3.MachineCredentialsStatus) bool {
	if a.ResourceVersion != b.ResourceVersion {
		return false
	}
	switch {
	case a.SecretRef == nil || b.SecretRef == nil:
		return a.SecretRef == b.SecretRef
	default:
		return a.SecretRef.Name == b.SecretRef.Name
	}
}

func stampLastUpdated(machine *keziov1alpha3.Machine) {
	now := metav1.Now()
	machine.Status.LastUpdated = &now
}

// deployRunDeletionOnly restricts the Owns(DeployRun) watch to deletion
// events: the reconciler only needs to notice a currentRunRef target
// disappearing. Progress-only DeployRun status updates must not fan out
// into Machine reconciles (the same reconcile-hygiene discipline as the
// Machine update predicate).
var deployRunDeletionOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// finalizersChangedPredicate reports true only when metadata.finalizers
// differs between the old and new object of an Update event, by set
// membership - controller-runtime has no built-in equivalent to
// GenerationChangedPredicate/AnnotationChangedPredicate for finalizers.
var finalizersChangedPredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !finalizerSetsEqual(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	},
}

func finalizerSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, f := range a {
		counts[f]++
	}
	for _, f := range b {
		counts[f]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// machineUpdatePredicate restricts the Machine watch's Update events to a
// generation, finalizers, or annotation change. A status-only self-write
// (state-walk progress, error/delay bookkeeping, credential tracking) must
// not re-trigger the reconciler on its own - every walk step that needs to
// run again returns an explicit requeue instead of depending on this event.
// Create/Delete/Generic events stay unfiltered: predicate.Or keeps each
// branch predicate's default (true) for them.
var machineUpdatePredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	finalizersChangedPredicate,
)

// SetupWithManager sets up the controller with the Manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha3.Machine{}, builder.WithPredicates(machineUpdatePredicate)).
		Owns(&keziov1alpha3.DeployRun{}, builder.WithPredicates(deployRunDeletionOnly)).
		// MatchEveryOwner: claimCredentialsSecret sets a non-controller
		// owner reference on the BMC credentials Secret, so the default
		// controller-only owner match would never see it. No predicate:
		// credential data edits must always reconcile the Machine.
		Owns(&corev1.Secret{}, builder.MatchEveryOwner).
		Named("machine").
		Complete(r)
}
