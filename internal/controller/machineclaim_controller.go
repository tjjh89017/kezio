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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// machineClaimNoCandidateRequeueInterval bounds how long a Pending claim
// with no bindable Machine right now waits before trying again - the
// claim also re-binds sooner, through the Machine watch, the moment a
// Machine in its namespace becomes Available.
const machineClaimNoCandidateRequeueInterval = 30 * time.Second

// MachineClaimReconciler reconciles a MachineClaim object.
type MachineClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes Events on MachineClaim and Machine -
	// binding, lost-binding, and release outcomes. Required.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machineclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machineclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machineclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machinehardwares,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *MachineClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim keziov1alpha3.MachineClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return r.clearOrphanedClaimRefs(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, &claim)
	}

	if !controllerutil.ContainsFinalizer(&claim, keziov1alpha3.MachineClaimFinalizer) {
		controllerutil.AddFinalizer(&claim, keziov1alpha3.MachineClaimFinalizer)
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.onChange(ctx, &claim)
}

// onDelete clears spec.claimRef on the bound Machine (if any still points
// at this claim) before removing the finalizer - see findBoundMachine for
// how the bound Machine is found. Clearing claimRef, not deleting or
// wiping anything, is deliberate: the claim layer erases nothing on the
// machine's disk, it only removes the machine from this claim's hands.
func (r *MachineClaimReconciler) onDelete(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, keziov1alpha3.MachineClaimFinalizer) {
		return ctrl.Result{}, nil
	}

	machine, err := r.findBoundMachine(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine != nil {
		if err := r.clearClaimRef(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(claim, keziov1alpha3.MachineClaimFinalizer)
	if err := r.Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// findBoundMachine returns the Machine whose spec.claimRef points at
// claim (matching name, namespace, and uid), or nil if none exists.
// status.machineName is checked first - the common case - falling back
// to a namespace-wide scan only when it is empty: a claim whose status
// was never written (a crash between binding the Machine and recording
// Bound) must still find its Machine.
func (r *MachineClaimReconciler) findBoundMachine(ctx context.Context, claim *keziov1alpha3.MachineClaim) (*keziov1alpha3.Machine, error) {
	if claim.Status.MachineName != "" {
		var machine keziov1alpha3.Machine
		key := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.MachineName}
		if err := r.Get(ctx, key, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("machineclaim %q: getting bound machine %q: %w", claim.Name, claim.Status.MachineName, err)
		}
		if claimRefMatches(machine.Spec.ClaimRef, claim) {
			return &machine, nil
		}
		return nil, nil
	}

	var machines keziov1alpha3.MachineList
	if err := r.List(ctx, &machines, client.InNamespace(claim.Namespace)); err != nil {
		return nil, fmt.Errorf("machineclaim %q: listing machines: %w", claim.Name, err)
	}
	for i := range machines.Items {
		if claimRefMatches(machines.Items[i].Spec.ClaimRef, claim) {
			return &machines.Items[i], nil
		}
	}
	return nil, nil
}

// claimRefMatches reports whether ref names claim exactly: name,
// namespace, and uid all match. The uid check is what tells a claim
// apart from an earlier claim of the same name.
func claimRefMatches(ref *keziov1alpha3.MachineClaimReference, claim *keziov1alpha3.MachineClaim) bool {
	return ref != nil && ref.Name == claim.Name && ref.Namespace == claim.Namespace && ref.UID == claim.UID
}

// clearClaimRef removes spec.claimRef from machine. It is the only field
// this reconciler ever writes on a Machine's spec.
func (r *MachineClaimReconciler) clearClaimRef(ctx context.Context, machine *keziov1alpha3.Machine) error {
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Spec.ClaimRef = nil
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("machine %q: clearing claimRef: %w", machine.Name, err)
	}
	return nil
}

// clearOrphanedClaimRefs handles a reconcile request naming a MachineClaim
// that does not exist: every Machine in namespace whose spec.claimRef
// names exactly this (now-gone) namespace/name is a lost binding - the
// live claim it pointed at is gone entirely, so no uid could ever match
// again. This is the safety net for a claim removed without this
// reconciler clearing claimRef first (the finalizer path above is the
// normal case); see the claim layer's single-writer rule: this is the
// only case where the controller clears a claimRef it did not write
// itself.
func (r *MachineClaimReconciler) clearOrphanedClaimRefs(ctx context.Context, namespace, name string) (ctrl.Result, error) {
	var machines keziov1alpha3.MachineList
	if err := r.List(ctx, &machines, client.InNamespace(namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing machines to find a claimRef orphaned by the deletion of %s/%s: %w", namespace, name, err)
	}
	for i := range machines.Items {
		machine := &machines.Items[i]
		ref := machine.Spec.ClaimRef
		if ref == nil || ref.Name != name || ref.Namespace != namespace {
			continue
		}
		if err := r.clearClaimRef(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ClaimLost",
			"clearing claimRef: MachineClaim %s/%s no longer exists", namespace, name)
	}
	return ctrl.Result{}, nil
}

// onChange dispatches a live (non-deleting) claim to the Bound or Pending
// path by its recorded phase. Failed is terminal: nothing here ever moves
// a claim out of it (see reconcilePendingByName's own doc comment).
func (r *MachineClaimReconciler) onChange(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	if claim.Status.Phase == keziov1alpha3.MachineClaimPhaseBound {
		return r.reconcileBound(ctx, claim)
	}
	return r.reconcilePending(ctx, claim)
}

// reconcileBound re-validates a Bound claim's binding on every reconcile:
// the bound Machine must still exist and still carry this claim's
// claimRef. Either failing is a lost binding - the claim controller
// clears its own status back to Pending and lets reconcilePending re-bind
// on the next pass. Otherwise this mirrors the bound Machine's deploy
// progress onto the claim's own status.
func (r *MachineClaimReconciler) reconcileBound(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	var machine keziov1alpha3.Machine
	key := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.MachineName}
	err := r.Get(ctx, key, &machine)
	switch {
	case apierrors.IsNotFound(err):
		return r.recordLostBinding(ctx, claim, fmt.Sprintf("bound machine %q no longer exists", claim.Status.MachineName))
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: getting bound machine %q: %w", claim.Name, claim.Status.MachineName, err)
	case !claimRefMatches(machine.Spec.ClaimRef, claim):
		return r.recordLostBinding(ctx, claim, fmt.Sprintf("machine %q no longer carries this claim's claimRef", claim.Status.MachineName))
	}

	return r.mirrorMachineStatus(ctx, claim, &machine)
}

// recordLostBinding clears claim's binding back to Pending and emits a
// Warning Event naming what was cleared and why.
func (r *MachineClaimReconciler) recordLostBinding(ctx context.Context, claim *keziov1alpha3.MachineClaim, reason string) (ctrl.Result, error) {
	lostMachine := claim.Status.MachineName
	claim.Status.Phase = keziov1alpha3.MachineClaimPhasePending
	claim.Status.MachineName = ""
	claim.Status.BoundAt = nil
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineClaimConditionBound,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: claim.Generation,
		Reason:             "LostBinding",
		Message:            reason,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: recording lost binding: %w", claim.Name, err)
	}
	r.Recorder.Eventf(claim, corev1.EventTypeWarning, "LostBinding", "cleared binding to machine %q: %s", lostMachine, reason)
	return ctrl.Result{Requeue: true}, nil
}

// mirrorMachineStatus copies machine's deploy progress onto claim.status:
// currentRunRef, lastSuccessfulRunRef, and the Ready condition (True once
// machine reaches Provisioned). A no-op write is skipped so a Bound
// claim's status does not churn on every unrelated Machine reconcile.
func (r *MachineClaimReconciler) mirrorMachineStatus(ctx context.Context, claim *keziov1alpha3.MachineClaim, machine *keziov1alpha3.Machine) (ctrl.Result, error) {
	changed := false
	if !nameRefEqual(claim.Status.CurrentRunRef, machine.Status.CurrentRunRef) {
		claim.Status.CurrentRunRef = machine.Status.CurrentRunRef.DeepCopy()
		changed = true
	}
	if !nameRefEqual(claim.Status.LastSuccessfulRunRef, machine.Status.LastSuccessfulRunRef) {
		claim.Status.LastSuccessfulRunRef = machine.Status.LastSuccessfulRunRef.DeepCopy()
		changed = true
	}

	readyStatus, readyReason, readyMessage := metav1.ConditionFalse, "MachineNotProvisioned", fmt.Sprintf("machine %q has not reached Provisioned", machine.Name)
	if machine.Status.State == keziov1alpha3.MachineStateProvisioned {
		readyStatus, readyReason, readyMessage = metav1.ConditionTrue, "MachineProvisioned", fmt.Sprintf("machine %q reached Provisioned", machine.Name)
	}
	if existing := apimeta.FindStatusCondition(claim.Status.Conditions, keziov1alpha3.MachineClaimConditionReady); existing == nil ||
		existing.Status != readyStatus || existing.Reason != readyReason || existing.ObservedGeneration != claim.Generation {
		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha3.MachineClaimConditionReady,
			Status:             readyStatus,
			ObservedGeneration: claim.Generation,
			Reason:             readyReason,
			Message:            readyMessage,
		})
		changed = true
	}
	if !apimeta.IsStatusConditionTrue(claim.Status.Conditions, keziov1alpha3.MachineClaimConditionBound) {
		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha3.MachineClaimConditionBound,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: claim.Generation,
			Reason:             "Bound",
			Message:            fmt.Sprintf("bound to machine %q", machine.Name),
		})
		changed = true
	}

	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: mirroring machine status: %w", claim.Name, err)
	}
	return ctrl.Result{}, nil
}

// reconcilePending drives a claim with no recorded binding toward one.
// adoptExistingBinding covers the idempotent case first - a Machine
// somewhere already carries this exact claim's claimRef, most likely
// because the claim's own status was lost - before ever considering a
// fresh bind.
func (r *MachineClaimReconciler) reconcilePending(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	adopted, err := r.adoptExistingBinding(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if adopted {
		return ctrl.Result{Requeue: true}, nil
	}

	switch {
	case claim.Spec.MachineName != "":
		return r.reconcilePendingByName(ctx, claim)
	case claim.Spec.Selector != nil:
		return r.reconcilePendingBySelector(ctx, claim)
	default:
		// Neither is given: MachinePool (not in this stage) is the only
		// thing that would ever supply a candidate to a claim shaped
		// this way, so this claim waits forever, exactly like a PVC
		// naming no StorageClass and no volume.
		return r.markNoCandidate(ctx, claim, "spec gives neither machineName nor selector: nothing can bind this claim")
	}
}

// adoptExistingBinding reports whether some Machine already carries
// claim's own claimRef, and if so records claim Bound to it.
func (r *MachineClaimReconciler) adoptExistingBinding(ctx context.Context, claim *keziov1alpha3.MachineClaim) (bool, error) {
	machine, err := r.findBoundMachine(ctx, claim)
	if err != nil {
		return false, err
	}
	if machine == nil {
		return false, nil
	}

	now := metav1.Now()
	claim.Status.Phase = keziov1alpha3.MachineClaimPhaseBound
	claim.Status.MachineName = machine.Name
	if claim.Status.BoundAt == nil {
		claim.Status.BoundAt = &now
	}
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineClaimConditionBound,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: claim.Generation,
		Reason:             "Bound",
		Message:            fmt.Sprintf("bound to machine %q", machine.Name),
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return false, fmt.Errorf("machineclaim %q: recording bound status: %w", claim.Name, err)
	}
	return true, nil
}

// reconcilePendingByName handles a claim that names its one candidate
// directly. A named Machine that does not exist at all moves the claim
// to Failed - a webhook cannot catch this at admission time, and the doc
// this reconciler follows is explicit that this case must not retry
// forever. Every other way the named Machine is unusable right now (not
// yet Available, or already claimed) leaves the claim Pending: it may
// still become bindable later.
func (r *MachineClaimReconciler) reconcilePendingByName(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	var machine keziov1alpha3.Machine
	key := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.MachineName}
	err := r.Get(ctx, key, &machine)
	if apierrors.IsNotFound(err) {
		return r.markFailed(ctx, claim, "MachineNotFound", fmt.Sprintf("spec.machineName names Machine %q, which does not exist", claim.Spec.MachineName))
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: getting machine %q: %w", claim.Name, claim.Spec.MachineName, err)
	}
	if !machineIsFreeAndAvailable(&machine) {
		return r.markNoCandidate(ctx, claim, fmt.Sprintf("machine %q is not Available, or is already bound to another claim", claim.Spec.MachineName))
	}

	bound, err := r.tryBind(ctx, claim, &machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if bound {
		return ctrl.Result{Requeue: true}, nil
	}
	// A conflict means another claim won the race for this one named
	// machine; spec.machineName gives no other candidate to fall back to.
	return r.markNoCandidate(ctx, claim, fmt.Sprintf("machine %q was bound by another MachineClaim first", claim.Spec.MachineName))
}

// reconcilePendingBySelector tries every matching candidate in
// deterministic order until one binds or the list is exhausted.
func (r *MachineClaimReconciler) reconcilePendingBySelector(ctx context.Context, claim *keziov1alpha3.MachineClaim) (ctrl.Result, error) {
	candidates, err := r.matchingCandidates(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, machine := range candidates {
		bound, err := r.tryBind(ctx, claim, machine)
		if err != nil {
			return ctrl.Result{}, err
		}
		if bound {
			return ctrl.Result{Requeue: true}, nil
		}
		// Conflict: another claim won this candidate. Try the next one.
	}
	return r.markNoCandidate(ctx, claim, "no Machine currently matches spec.selector")
}

// machineIsFreeAndAvailable reports whether machine can be bound right
// now: unclaimed, and inspected (Available is the PV-Available phase -
// see MachineStateAvailable's own doc comment).
func machineIsFreeAndAvailable(machine *keziov1alpha3.Machine) bool {
	return machine.Spec.ClaimRef == nil && machine.Status.State == keziov1alpha3.MachineStateAvailable
}

// matchingCandidates lists every free, Available Machine in claim's
// namespace whose labels and (when given) hardware satisfy
// claim.spec.selector, sorted by name for a deterministic bind order.
func (r *MachineClaimReconciler) matchingCandidates(ctx context.Context, claim *keziov1alpha3.MachineClaim) ([]*keziov1alpha3.Machine, error) {
	selector, err := metav1.LabelSelectorAsSelector(&claim.Spec.Selector.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("machineclaim %q: parsing spec.selector: %w", claim.Name, err)
	}

	var machines keziov1alpha3.MachineList
	if err := r.List(ctx, &machines, client.InNamespace(claim.Namespace)); err != nil {
		return nil, fmt.Errorf("machineclaim %q: listing machines: %w", claim.Name, err)
	}

	candidates := make([]*keziov1alpha3.Machine, 0, len(machines.Items))
	for i := range machines.Items {
		machine := &machines.Items[i]
		if !machineIsFreeAndAvailable(machine) {
			continue
		}
		if !selector.Matches(labels.Set(machine.Labels)) {
			continue
		}
		if claim.Spec.Selector.Hardware != nil {
			matched, err := r.machineMatchesHardware(ctx, machine, claim.Spec.Selector.Hardware)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
		}
		candidates = append(candidates, machine)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	return candidates, nil
}

// machineMatchesHardware reports whether machine's MachineHardware
// satisfies sel. A Machine with no MachineHardware object at all does
// not match a hardware selector - it has nothing to compare against.
func (r *MachineClaimReconciler) machineMatchesHardware(ctx context.Context, machine *keziov1alpha3.Machine, sel *keziov1alpha3.ClaimHardwareSelector) (bool, error) {
	var hw keziov1alpha3.MachineHardware
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}
	if err := r.Get(ctx, key, &hw); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("machine %q: getting MachineHardware: %w", machine.Name, err)
	}
	return hardwareMatchesSelector(&hw.Spec, sel), nil
}

// tryBind writes claimRef onto machine with an optimistic-concurrency
// Update: a resourceVersion conflict means another claim's reconcile won
// the race for this candidate first (returns false, nil - not an error),
// and the caller moves on to the next candidate, if any. A successful
// write is immediately followed by recording the claim Bound, in the
// same reconcile.
func (r *MachineClaimReconciler) tryBind(ctx context.Context, claim *keziov1alpha3.MachineClaim, machine *keziov1alpha3.Machine) (bool, error) {
	machine.Spec.ClaimRef = &keziov1alpha3.MachineClaimReference{
		Name:      claim.Name,
		Namespace: claim.Namespace,
		UID:       claim.UID,
	}
	switch err := r.Update(ctx, machine); {
	case err == nil:
	case apierrors.IsConflict(err):
		return false, nil
	default:
		return false, fmt.Errorf("machine %q: binding claimRef: %w", machine.Name, err)
	}

	now := metav1.Now()
	claim.Status.Phase = keziov1alpha3.MachineClaimPhaseBound
	claim.Status.MachineName = machine.Name
	claim.Status.BoundAt = &now
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineClaimConditionBound,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: claim.Generation,
		Reason:             "Bound",
		Message:            fmt.Sprintf("bound to machine %q", machine.Name),
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return false, fmt.Errorf("machineclaim %q: recording bound status: %w", claim.Name, err)
	}
	r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Bound", "bound to machine %q", machine.Name)
	return true, nil
}

// markNoCandidate records Pending with a Bound=False condition explaining
// why, and requeues with backoff: a claim that finds no candidate waits,
// it does not fail, the same way an unfulfilled PersistentVolumeClaim
// does.
func (r *MachineClaimReconciler) markNoCandidate(ctx context.Context, claim *keziov1alpha3.MachineClaim, message string) (ctrl.Result, error) {
	claim.Status.Phase = keziov1alpha3.MachineClaimPhasePending
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineClaimConditionBound,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: claim.Generation,
		Reason:             "NoMatchingMachine",
		Message:            message,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: recording no matching machine: %w", claim.Name, err)
	}
	return ctrl.Result{RequeueAfter: machineClaimNoCandidateRequeueInterval}, nil
}

// markFailed records phase Failed with a Bound=False condition explaining
// why, and does not requeue: Failed means the claim cannot bind and must
// not retry.
func (r *MachineClaimReconciler) markFailed(ctx context.Context, claim *keziov1alpha3.MachineClaim, reason, message string) (ctrl.Result, error) {
	claim.Status.Phase = keziov1alpha3.MachineClaimPhaseFailed
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineClaimConditionBound,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: claim.Generation,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("machineclaim %q: recording failed phase: %w", claim.Name, err)
	}
	r.Recorder.Event(claim, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{}, nil
}

// machineClaimUpdatePredicate restricts the MachineClaim watch's Update
// events to a generation, finalizers, or annotation change, mirroring
// machineUpdatePredicate: a status-only self-write (phase, conditions,
// the run-ref mirror) must not re-trigger this reconciler on its own.
var machineClaimUpdatePredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	finalizersChangedPredicate,
)

// mapMachineToClaims requeues the claim a changed Machine's claimRef
// names, if any, plus - when the Machine has no claimRef and just became
// Available - every Pending claim in its namespace: any of those may now
// find it as a candidate. This is a superset (every Pending claim is
// requeued, not only ones its selector would actually match): a spurious
// reconcile just re-confirms Pending, while missing one would leave a
// claim waiting on a Machine watch event that never comes.
func (r *MachineClaimReconciler) mapMachineToClaims(ctx context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		return nil
	}

	if ref := machine.Spec.ClaimRef; ref != nil {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}}}
	}

	if machine.Status.State != keziov1alpha3.MachineStateAvailable {
		return nil
	}
	var claims keziov1alpha3.MachineClaimList
	if err := r.List(ctx, &claims, client.InNamespace(machine.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.Phase != keziov1alpha3.MachineClaimPhasePending {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha3.MachineClaim{}, builder.WithPredicates(machineClaimUpdatePredicate)).
		Watches(&keziov1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineToClaims)).
		Named("machineclaim").
		Complete(r)
}
