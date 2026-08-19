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
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/deployer"
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

// MachineReconciler reconciles a Machine object
type MachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Deployer drives hardware inspection and image provisioning. Required.
	Deployer deployer.Deployer
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machinehardwares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var machine keziov1alpha2.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, &machine)
	}

	if !controllerutil.ContainsFinalizer(&machine, keziov1alpha2.MachineFinalizer) {
		controllerutil.AddFinalizer(&machine, keziov1alpha2.MachineFinalizer)
		if err := r.Update(ctx, &machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.onChange(ctx, &machine)
}

func (r *MachineReconciler) onDelete(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	// Release the credentials Secret's sub-finalizer before the Machine's
	// own finalizer: once the Machine is actually removed, nothing maps a
	// dangling Secret owner reference back to a live reconcile that could
	// still clear it.
	if err := r.releaseCredentialsSecretFinalizer(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(machine, keziov1alpha2.MachineFinalizer)
	return ctrl.Result{}, r.Update(ctx, machine)
}

// onChange drives one step of the state walk per call: Enrolling ->
// Inspecting -> Available -> Provisioning -> Provisioned, plus the
// Available/Provisioned re-trigger. It never advances more than one step,
// relying on the watch on Machine (state and status changes) or an explicit
// RequeueAfter to drive the next step.
func (r *MachineReconciler) onChange(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	if hasUnknownErrorType(machine) {
		return r.setState(ctx, machine, keziov1alpha2.MachineStateEnrolling)
	}

	switch machine.Status.State {
	case "":
		return r.setState(ctx, machine, keziov1alpha2.MachineStateEnrolling)
	case keziov1alpha2.MachineStateEnrolling:
		return r.setState(ctx, machine, keziov1alpha2.MachineStateInspecting)
	case keziov1alpha2.MachineStateInspecting:
		return r.reconcileInspecting(ctx, machine)
	case keziov1alpha2.MachineStateAvailable, keziov1alpha2.MachineStateProvisioned:
		return r.reconcileIdle(ctx, machine)
	case keziov1alpha2.MachineStateProvisioning:
		return r.reconcileProvisioning(ctx, machine)
	default:
		return ctrl.Result{}, fmt.Errorf("machine %q: unknown status.state %q", machine.Name, machine.Status.State)
	}
}

// setState patches machine.status.state and clears any error, leaving every
// other status field untouched.
func (r *MachineReconciler) setState(ctx context.Context, machine *keziov1alpha2.Machine, state string) (ctrl.Result, error) {
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.State = state
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusOK
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to %q: %w", machine.Name, state, err)
	}
	return ctrl.Result{}, nil
}

func (r *MachineReconciler) reconcileInspecting(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	secret, blocked, gateResult, err := r.resolveCredentialsSecret(ctx, machine)
	if blocked {
		return gateResult, err
	}
	if err := r.recordTriedCredentials(ctx, machine, secret); err != nil {
		return ctrl.Result{}, err
	}
	result, err := r.Deployer.Inspect(ctx, machine, restartOnFailure(machine))
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.Outcome != deployer.Failed {
		if err := r.recordGoodCredentials(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}
	if result.Outcome == deployer.Complete {
		return r.setState(ctx, machine, keziov1alpha2.MachineStateAvailable)
	}
	return r.applyNonCompleteOutcome(ctx, machine, result)
}

// reconcileIdle handles Available and Provisioned: both are idle states
// that only leave through the same provisioning trigger.
func (r *MachineReconciler) reconcileIdle(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	lastRun, err := r.lastSuccessfulRun(ctx, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: reading last successful DeployRun: %w", machine.Name, err)
	}
	if !shouldProvision(machine, lastRun) {
		return ctrl.Result{}, nil
	}

	return r.startProvisioningRun(ctx, machine)
}

// startProvisioningRun creates a fresh DeployRun for machine and records it
// as the current run, moving (or keeping) status.state at Provisioning.
// Used both to enter Provisioning from an idle state and to recover from a
// current run that disappeared mid-Provisioning.
func (r *MachineReconciler) startProvisioningRun(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	run, err := r.createDeployRun(ctx, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: creating DeployRun: %w", machine.Name, err)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.State = keziov1alpha2.MachineStateProvisioning
	machine.Status.CurrentRunRef = &keziov1alpha2.NameRef{Name: run.Name}
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusOK
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to Provisioning: %w", machine.Name, err)
	}
	return ctrl.Result{}, nil
}

func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	// A nil currentRunRef while still Provisioning is not an invariant
	// violation: recordCurrentRunDeleted clears it after the run
	// disappears out from under an in-progress deployment, and this is
	// where the retry-in-place picks it back up with a fresh run.
	if machine.Status.CurrentRunRef == nil {
		return r.startProvisioningRun(ctx, machine)
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
	result, err := r.Deployer.Provision(ctx, machine, run, restartOnFailure(machine))
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.Outcome != deployer.Failed {
		if err := r.recordGoodCredentials(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}
	if result.Outcome != deployer.Complete {
		return r.applyNonCompleteOutcome(ctx, machine, result)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.State = keziov1alpha2.MachineStateProvisioned
	machine.Status.LastSuccessfulRunRef = &keziov1alpha2.NameRef{Name: run.Name}
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusOK
	machine.Status.ErrorCount = 0
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: setting status.state to Provisioned: %w", machine.Name, err)
	}
	return ctrl.Result{}, nil
}

// restartOnFailure derives the deployer's restartOnFailure argument from
// the previous attempt's recorded error: only a live error (OK is never
// stale-restart) with errorType MachineErrorTypeRestart asks the deployer
// to discard in-progress step state; every other case, including an
// unrecognized errorType, resumes the step in place.
func restartOnFailure(machine *keziov1alpha2.Machine) bool {
	return machine.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusError &&
		machine.Status.ErrorType == keziov1alpha2.MachineErrorTypeRestart
}

// hasUnknownErrorType reports whether the machine is recorded as errored
// with an errorType outside the known set (Transient, Restart): corrupt
// data or a value written by an incompatible controller version. An empty
// errorType is the no-error case, not unknown, so it does not match.
func hasUnknownErrorType(machine *keziov1alpha2.Machine) bool {
	if machine.Status.OperationalStatus != keziov1alpha2.MachineOperationalStatusError {
		return false
	}
	switch machine.Status.ErrorType {
	case "", keziov1alpha2.MachineErrorTypeTransient, keziov1alpha2.MachineErrorTypeRestart:
		return false
	default:
		return true
	}
}

// applyNonCompleteOutcome handles every deployer.Result.Outcome except
// Complete, shared between the Inspecting and Provisioning steps.
func (r *MachineReconciler) applyNonCompleteOutcome(ctx context.Context, machine *keziov1alpha2.Machine, result deployer.Result) (ctrl.Result, error) {
	switch result.Outcome {
	case deployer.Continuing:
		return r.clearDelayed(ctx, machine, ctrl.Result{RequeueAfter: continuingRequeueInterval})
	case deployer.Busy:
		requeueAfter := defaultBusyRequeueInterval
		if result.RequeueAfter > 0 {
			requeueAfter = result.RequeueAfter
		}
		return r.clearDelayed(ctx, machine, ctrl.Result{RequeueAfter: requeueAfter})
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
func (r *MachineReconciler) recordDelayed(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	return r.markDelayed(ctx, machine, delayedRequeueInterval)
}

// markDelayed marks machine delayed: state, errorType, and errorCount are
// left untouched - delayed is not an error, so it never joins the error
// backoff/reset accounting. The caller always requeues after requeueAfter,
// a fixed delay independent of any error backoff.
func (r *MachineReconciler) markDelayed(ctx context.Context, machine *keziov1alpha2.Machine, requeueAfter time.Duration) (ctrl.Result, error) {
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusDelayed
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording delayed status: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// clearDelayed clears a previously recorded delayed status on any outcome
// other than Delayed itself, so "delayed" never outlives the condition that
// caused it. It is a no-op (result returned unchanged) when the machine is
// not currently delayed, including when it is in error: only Failed's own
// path overwrites an error status.
func (r *MachineReconciler) clearDelayed(ctx context.Context, machine *keziov1alpha2.Machine, result ctrl.Result) (ctrl.Result, error) {
	if machine.Status.OperationalStatus != keziov1alpha2.MachineOperationalStatusDelayed {
		return result, nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusOK
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: clearing delayed status: %w", machine.Name, err)
	}
	return result, nil
}

func (r *MachineReconciler) recordFailure(ctx context.Context, machine *keziov1alpha2.Machine, result deployer.Result) (ctrl.Result, error) {
	patch := client.MergeFrom(machine.DeepCopy())
	applyFailure(machine, result.ErrorType, result.ErrorMessage)
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording deployer failure: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
}

// applyFailure applies the two-axis error semantics shared by every failure
// path: state is left untouched, only operationalStatus/errorType/
// errorMessage/errorCount change.
func applyFailure(machine *keziov1alpha2.Machine, errorType keziov1alpha2.MachineErrorType, errorMessage string) {
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusError
	machine.Status.ErrorType = errorType
	machine.Status.ErrorMessage = errorMessage
	machine.Status.ErrorCount++
}

// recordCurrentRunDeleted handles the current DeployRun having disappeared
// out from under a Provisioning machine - GC or an operator deleting it
// mid-run. The abort transport that would let the controller interrupt an
// in-progress agent session ships later; until then this is reported
// through the same recordFailure semantics as any other phase failure
// (state unchanged, operationalStatus=error) rather than a parallel error
// path, and currentRunRef is cleared in the same patch so the next
// Provisioning reconcile's nil-ref branch starts a fresh run.
func (r *MachineReconciler) recordCurrentRunDeleted(ctx context.Context, machine *keziov1alpha2.Machine) (ctrl.Result, error) {
	patch := client.MergeFrom(machine.DeepCopy())
	// MachineErrorTypeRestart: the run this errorType would ask to resume no
	// longer exists, so the only meaningful next step is starting over, not
	// resuming in-progress step state.
	applyFailure(machine, keziov1alpha2.MachineErrorTypeRestart, fmt.Sprintf("current DeployRun %q no longer exists", machine.Status.CurrentRunRef.Name))
	machine.Status.CurrentRunRef = nil
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording current-run-deleted failure: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
}

// createDeployRun creates the DeployRun for one provisioning pass, copying
// imageRef and dataImages from machine.spec. ResolvedDisks and HooksHash
// are left empty: disk and hook resolution are not implemented yet.
func (r *MachineReconciler) createDeployRun(ctx context.Context, machine *keziov1alpha2.Machine) (*keziov1alpha2.DeployRun, error) {
	run := &keziov1alpha2.DeployRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: machine.Name + "-",
			Namespace:    machine.Namespace,
		},
		Spec: keziov1alpha2.DeployRunSpec{
			MachineRef: keziov1alpha2.NameRef{Name: machine.Name},
			ImageRef:   machine.Spec.ImageRef.DeepCopy(),
			DataImages: append([]keziov1alpha2.MachineDataImage(nil), machine.Spec.DataImages...),
		},
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
func (r *MachineReconciler) lastSuccessfulRun(ctx context.Context, machine *keziov1alpha2.Machine) (*keziov1alpha2.DeployRun, error) {
	if machine.Status.LastSuccessfulRunRef == nil {
		return nil, nil
	}
	run, err := r.getRun(ctx, machine, machine.Status.LastSuccessfulRunRef)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return run, err
}

func (r *MachineReconciler) getRun(ctx context.Context, machine *keziov1alpha2.Machine, ref *keziov1alpha2.NameRef) (*keziov1alpha2.DeployRun, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = machine.Namespace
	}
	var run keziov1alpha2.DeployRun
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// shouldProvision is the provisioning trigger. It fires only when
// spec.imageRef/spec.dataImages describe some deploy intent and that
// intent's subset differs from the last successful run's recorded
// snapshot. A missing lastRun - no lastSuccessfulRunRef yet, or one whose
// DeployRun was deleted - is "no successful run known": a non-empty
// payload triggers once, and the resulting run's own success writes a
// fresh lastSuccessfulRunRef, so this path cannot repeat into a storm.
func shouldProvision(machine *keziov1alpha2.Machine, lastRun *keziov1alpha2.DeployRun) bool {
	if isEmptyDeployPayload(machine) {
		return false
	}
	if lastRun == nil {
		return true
	}
	return !intentSubsetEqual(machine, lastRun)
}

// isEmptyDeployPayload reports whether machine.spec carries no deploy
// intent at all: no OS image and no data images. An empty payload never
// triggers a run - clearing intent means "nothing to do", not "wipe".
func isEmptyDeployPayload(machine *keziov1alpha2.Machine) bool {
	return machine.Spec.ImageRef == nil && len(machine.Spec.DataImages) == 0
}

// intentSubsetEqual compares the provisioning trigger's intent subset -
// imageRef, dataImages, hooksHash - against the last successful run's
// recorded spec. This is full equality, not a superset check, so removing
// a dataImages entry triggers correctly. resolvedDisks is deliberately
// excluded: device names are not stable across boots, and a
// re-resolution must never diff into a disk-wiping redeploy. PostHook
// resolution does not exist yet, so the Machine side of hooksHash is
// always empty; createDeployRun likewise never sets
// DeployRun.spec.hooksHash, so both sides agree until hook resolution
// lands and both must be wired together.
func intentSubsetEqual(machine *keziov1alpha2.Machine, lastRun *keziov1alpha2.DeployRun) bool {
	const machineHooksHash = ""
	return nameRefEqual(machine.Spec.ImageRef, lastRun.Spec.ImageRef) &&
		reflect.DeepEqual(machine.Spec.DataImages, lastRun.Spec.DataImages) &&
		machineHooksHash == lastRun.Spec.HooksHash
}

func nameRefEqual(a, b *keziov1alpha2.NameRef) bool {
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
func (r *MachineReconciler) resolveCredentialsSecret(ctx context.Context, machine *keziov1alpha2.Machine) (*corev1.Secret, bool, ctrl.Result, error) {
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
func (r *MachineReconciler) claimCredentialsSecret(ctx context.Context, machine *keziov1alpha2.Machine, secret *corev1.Secret) error {
	patch := client.MergeFrom(secret.DeepCopy())
	changed := false

	if secret.Labels[keziov1alpha2.MachineCredentialsSecretLabel] != "true" {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[keziov1alpha2.MachineCredentialsSecretLabel] = "true"
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

	if controllerutil.AddFinalizer(secret, keziov1alpha2.MachineCredentialsSecretFinalizer) {
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
func (r *MachineReconciler) releaseCredentialsSecretFinalizer(ctx context.Context, machine *keziov1alpha2.Machine) error {
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

	if !controllerutil.RemoveFinalizer(&secret, keziov1alpha2.MachineCredentialsSecretFinalizer) {
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
func (r *MachineReconciler) recordTriedCredentials(ctx context.Context, machine *keziov1alpha2.Machine, secret *corev1.Secret) error {
	observed := keziov1alpha2.MachineCredentialsStatus{
		SecretRef:       &keziov1alpha2.SecretReference{Name: secret.Name},
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
		machine.Status.GoodCredentials = keziov1alpha2.MachineCredentialsStatus{}
	}
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: recording tried BMC credentials: %w", machine.Name, err)
	}
	return nil
}

// recordGoodCredentials copies status.triedCredentials into
// status.goodCredentials once a Deployer call returns without a transient
// error and without Outcome == Failed - the modeled stand-in for "the BMC
// answered" in a codebase whose fake path never dials a real BMC. A no-op
// when goodCredentials is already up to date.
func (r *MachineReconciler) recordGoodCredentials(ctx context.Context, machine *keziov1alpha2.Machine) error {
	if machine.Status.TriedCredentials.SecretRef == nil {
		return nil
	}
	if credentialsStatusEqual(machine.Status.GoodCredentials, machine.Status.TriedCredentials) {
		return nil
	}

	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.GoodCredentials = machine.Status.TriedCredentials
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: recording good BMC credentials: %w", machine.Name, err)
	}
	return nil
}

// credentialsStatusEqual compares two MachineCredentialsStatus values by
// value, since SecretRef is a pointer and the zero value (no observation
// yet) must compare equal to itself.
func credentialsStatusEqual(a, b keziov1alpha2.MachineCredentialsStatus) bool {
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

func stampLastUpdated(machine *keziov1alpha2.Machine) {
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

// SetupWithManager sets up the controller with the Manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.Machine{}).
		Owns(&keziov1alpha2.DeployRun{}, builder.WithPredicates(deployRunDeletionOnly)).
		// MatchEveryOwner: claimCredentialsSecret sets a non-controller
		// owner reference on the BMC credentials Secret, so the default
		// controller-only owner match would never see it.
		Owns(&corev1.Secret{}, builder.MatchEveryOwner).
		Named("machine").
		Complete(r)
}
