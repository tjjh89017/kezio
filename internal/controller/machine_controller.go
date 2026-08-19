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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	result, err := r.Deployer.Inspect(ctx, machine, restartOnFailure(machine))
	if err != nil {
		return ctrl.Result{}, err
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
	if machine.Status.CurrentRunRef == nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: status.state is Provisioning but status.currentRunRef is unset", machine.Name)
	}

	run, err := r.getRun(ctx, machine, machine.Status.CurrentRunRef)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: getting DeployRun %q: %w", machine.Name, machine.Status.CurrentRunRef.Name, err)
	}

	result, err := r.Deployer.Provision(ctx, machine, run, restartOnFailure(machine))
	if err != nil {
		return ctrl.Result{}, err
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
		return ctrl.Result{RequeueAfter: continuingRequeueInterval}, nil
	case deployer.Busy:
		if result.RequeueAfter > 0 {
			return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
		}
		return ctrl.Result{RequeueAfter: defaultBusyRequeueInterval}, nil
	case deployer.Failed:
		return r.recordFailure(ctx, machine, result)
	default:
		return ctrl.Result{}, fmt.Errorf("machine %q: deployer step returned unrecognized outcome %v", machine.Name, result.Outcome)
	}
}

func (r *MachineReconciler) recordFailure(ctx context.Context, machine *keziov1alpha2.Machine, result deployer.Result) (ctrl.Result, error) {
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.OperationalStatus = keziov1alpha2.MachineOperationalStatusError
	machine.Status.ErrorType = result.ErrorType
	machine.Status.ErrorMessage = result.ErrorMessage
	machine.Status.ErrorCount++
	stampLastUpdated(machine)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("machine %q: recording deployer failure: %w", machine.Name, err)
	}
	return ctrl.Result{RequeueAfter: failedRequeueInterval}, nil
}

// createDeployRun creates the DeployRun for one provisioning pass, copying
// this stage's minimal snapshot (imageRef, dataImages) from machine.spec.
// ResolvedDisks and HooksHash are left empty: disk and hook resolution are
// not part of this stage's deployer contract.
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

// shouldProvision is this stage's provisioning trigger: spec.imageRef alone,
// compared against the last successful run's resolved snapshot. An absent
// spec.imageRef never triggers.
func shouldProvision(machine *keziov1alpha2.Machine, lastRun *keziov1alpha2.DeployRun) bool {
	if machine.Spec.ImageRef == nil {
		return false
	}
	if lastRun == nil {
		return true
	}
	return !nameRefEqual(machine.Spec.ImageRef, lastRun.Spec.ImageRef)
}

func nameRefEqual(a, b *keziov1alpha2.NameRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Namespace == b.Namespace
}

func stampLastUpdated(machine *keziov1alpha2.Machine) {
	now := metav1.Now()
	machine.Status.LastUpdated = &now
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.Machine{}).
		Named("machine").
		Complete(r)
}
