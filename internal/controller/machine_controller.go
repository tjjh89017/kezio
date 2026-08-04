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

package controller

import (
	"context"
	"errors"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/deployer"
	"github.com/tjjh89017/kezio/internal/diskmatch"
)

// Ready condition reasons used while a Machine progresses through its
// states. A reason ending in "Failed" also names the phase to retry once
// the machine leaves the Error state: reconcileError reads it back from the
// Ready condition to resume the step that failed, instead of restarting
// from Enrolling.
const (
	reasonEnrolling    = "Enrolling"
	reasonInspecting   = "Inspecting"
	reasonAvailable    = "Available"
	reasonProvisioning = "Provisioning"
	reasonProvisioned  = "Provisioned"

	reasonRegisterFailed    = "RegisterFailed"
	reasonInspectFailed     = "InspectFailed"
	reasonProvisionFailed   = "ProvisionFailed"
	reasonDeprovisionFailed = "DeprovisionFailed"
)

// MachineReconciler reconciles a Machine object
type MachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DeployerFactory builds the Deployer used to drive a machine through
	// its phases. Production wiring and tests inject different factories
	// (a real, hardware-backed one and deployer.FakeFactory respectively)
	// behind the same deployer.Factory function type.
	DeployerFactory deployer.Factory
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines/finalizers,verbs=update

// Reconcile follows the fetch / onDelete / ensure-finalizer / onChange shape
// shared by every kezio controller: fetch the Machine, dispatch to onDelete
// if it is being deleted, otherwise ensure the finalizer is present before
// dispatching to onChange.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	machine := &keziov1alpha1.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, req, machine)
	}

	if !controllerutil.ContainsFinalizer(machine, keziov1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(machine, keziov1alpha1.FinalizerName)
		if err := r.Update(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return r.onChange(ctx, req, machine)
}

// onChange drives one Machine through its state machine: Enrolling ->
// Inspecting -> Available -> Provisioning -> Provisioned, plus an Error
// state entered from any phase failure.
func (r *MachineReconciler) onChange(ctx context.Context, _ ctrl.Request, machine *keziov1alpha1.Machine) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	dep, err := r.DeployerFactory(machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building deployer: %w", err)
	}

	if machine.Status.State == "" {
		return r.advance(ctx, machine, keziov1alpha1.MachineStateEnrolling, reasonEnrolling,
			"Machine enrolled; registering with the deployer")
	}

	switch machine.Status.State {
	case keziov1alpha1.MachineStateEnrolling:
		return r.reconcileEnrolling(ctx, machine, dep)
	case keziov1alpha1.MachineStateInspecting:
		return r.reconcileInspecting(ctx, machine, dep)
	case keziov1alpha1.MachineStateAvailable:
		return r.reconcileAvailable(ctx, machine, dep)
	case keziov1alpha1.MachineStateProvisioning:
		return r.reconcileProvisioning(ctx, machine, dep)
	case keziov1alpha1.MachineStateProvisioned:
		return r.reconcileProvisioned(ctx, machine, dep)
	case keziov1alpha1.MachineStateError:
		return r.reconcileError(ctx, machine, dep)
	default:
		log.Info("unknown machine state, resetting to Enrolling", "state", machine.Status.State)
		return r.advance(ctx, machine, keziov1alpha1.MachineStateEnrolling, reasonEnrolling,
			"Unknown state observed; re-enrolling")
	}
}

// reconcileEnrolling registers the machine with the deployer, driving
// Enrolling -> Inspecting.
func (r *MachineReconciler) reconcileEnrolling(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	result, err := dep.Register(ctx, &deployer.RegisterData{
		BootMACAddress: machine.Spec.BootMACAddress,
		BMC:            machine.Spec.BMC,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.ErrorMessage != "" {
		return r.recordPhaseError(ctx, machine, reasonRegisterFailed, result.ErrorMessage)
	}
	if result.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}

	return r.advance(ctx, machine, keziov1alpha1.MachineStateInspecting, reasonInspecting,
		"Machine registered; collecting hardware inventory")
}

// reconcileInspecting collects hardware inventory from the deployer,
// driving Inspecting -> Available.
func (r *MachineReconciler) reconcileInspecting(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	data := &deployer.InspectData{}
	result, err := dep.Inspect(ctx, data)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.ErrorMessage != "" {
		return r.recordPhaseError(ctx, machine, reasonInspectFailed, result.ErrorMessage)
	}
	if result.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}

	machine.Status.Hardware = data.Hardware
	return r.advance(ctx, machine, keziov1alpha1.MachineStateAvailable, reasonAvailable,
		"Hardware inventory collected; machine is available for deployment")
}

// reconcileAvailable checks whether spec differs from the last successful
// deployment and, if so, drives Available -> Provisioning. Otherwise it
// keeps the machine's power state matched to spec.online.
func (r *MachineReconciler) reconcileAvailable(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	if r.needsProvisioning(machine) {
		return r.advance(ctx, machine, keziov1alpha1.MachineStateProvisioning, reasonProvisioning,
			"Deployment requested; provisioning")
	}

	return r.reconcilePower(ctx, machine, dep)
}

// reconcileProvisioning resolves the target disk for the OS image and every
// data image against the reported hardware inventory, then drives the
// deployment plan, polling Provision until it completes, then records what
// was deployed and drives Provisioning -> Provisioned.
//
// Reboot handoff: AfterDeploy only applies when the deployment carries no
// OS image (see EffectiveAfterDeploy's doc comment) - a deployment that
// does carry one always reboots into it, and that reboot is the agent's
// own doing (a self-issued "systemctl reboot" once it finishes writing
// content), never anything this method drives. For the no-OS-image case,
// AfterDeployReboot likewise stays agent-driven (it reboots the machine
// back to its already-running OS, which the agent can do itself); only
// AfterDeployPowerOff is handled here, because powering off is not
// something the agent can safely leave itself to do after reporting
// success. dep.PowerOff is the same Deployer method reconcilePower calls
// for steady-state power matching - when the Machine has a BMC
// configured, that call is BMC-driven (a graceful shutdown, the BMC
// equivalent of "systemctl poweroff"); without one it is a no-op
// documented on agentDeployer.PowerOff, deliberately leaving that case to
// an operator or a future agent-driven poweroff rather than this method
// guessing at one. Either way, this call never races the agent's own
// reboot: it only ever fires for the no-OS-image path the agent's reboot
// does not touch.
func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	if err := r.resolveTargetDisks(ctx, machine); err != nil {
		return r.recordPhaseError(ctx, machine, reasonProvisionFailed, err.Error())
	}

	data := &deployer.ProvisionData{
		ImageRef:   machine.Spec.ImageRef,
		TargetDisk: machine.Spec.TargetDisk,
		DataImages: machine.Spec.DataImages,
		Ezio:       machine.Spec.Ezio,
	}
	result, err := dep.Provision(ctx, data)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.ErrorMessage != "" {
		return r.recordPhaseError(ctx, machine, reasonProvisionFailed, result.ErrorMessage)
	}
	if result.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}

	machine.Status.Provisioning = r.buildProvisioningStatus(machine, data)

	if machine.Spec.ImageRef == nil && machine.Spec.EffectiveAfterDeploy() == keziov1alpha1.AfterDeployPowerOff {
		poResult, poErr := dep.PowerOff(ctx)
		if poErr != nil {
			return ctrl.Result{}, poErr
		}
		if poResult.ErrorMessage != "" {
			return r.recordPhaseError(ctx, machine, reasonProvisionFailed, poResult.ErrorMessage)
		}
		off := false
		machine.Status.PoweredOn = &off
	}

	return r.advance(ctx, machine, keziov1alpha1.MachineStateProvisioned, reasonProvisioned,
		"Deployment complete")
}

// reconcileProvisioned checks for a new deployment request (a later spec
// change), driving back to Provisioning; otherwise it keeps the machine's
// power state matched to spec.online.
func (r *MachineReconciler) reconcileProvisioned(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	if r.needsProvisioning(machine) {
		return r.advance(ctx, machine, keziov1alpha1.MachineStateProvisioning, reasonProvisioning,
			"Deployment requested; provisioning")
	}

	return r.reconcilePower(ctx, machine, dep)
}

// reconcileError retries the phase that failed, read back from the Ready
// condition's reason. The retry uses the same handler as the state that
// phase belongs to, so a successful retry advances the machine exactly as
// a first-try success would have.
func (r *MachineReconciler) reconcileError(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.ConditionReady)

	reason := ""
	if cond != nil {
		reason = cond.Reason
	}

	switch reason {
	case reasonInspectFailed:
		return r.reconcileInspecting(ctx, machine, dep)
	case reasonProvisionFailed:
		return r.reconcileProvisioning(ctx, machine, dep)
	case reasonRegisterFailed:
		return r.reconcileEnrolling(ctx, machine, dep)
	default:
		// No recorded failure reason to resume from (or an unrecognized
		// one): the safest retry is to re-enroll from the start.
		return r.reconcileEnrolling(ctx, machine, dep)
	}
}

// onDelete runs Deprovision before letting deletion proceed, retrying on
// failure with the same backoff phase errors use. Once Deprovision reports
// completion, it removes the finalizer so the API server can delete the
// object.
func (r *MachineReconciler) onDelete(ctx context.Context, _ ctrl.Request, machine *keziov1alpha1.Machine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, keziov1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}

	dep, err := r.DeployerFactory(machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building deployer: %w", err)
	}

	result, err := dep.Deprovision(ctx, &deployer.DeprovisionData{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.ErrorMessage != "" {
		return r.recordPhaseError(ctx, machine, reasonDeprovisionFailed, result.ErrorMessage)
	}
	if result.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(machine, keziov1alpha1.FinalizerName)
	if err := r.Update(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcilePower matches the machine's actual power state to spec.online.
// Unlike a phase failure, a power mismatch does not move the machine to
// the Error state: it is background maintenance of a steady state
// (Available or Provisioned), not a step in the deployment flow, so it
// retries on its own short interval instead of consuming errorCount.
func (r *MachineReconciler) reconcilePower(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	desired := machine.Spec.EffectiveOnline()
	if machine.Status.PoweredOn != nil && *machine.Status.PoweredOn == desired {
		return ctrl.Result{}, nil
	}

	var result deployer.Result
	var err error
	if desired {
		result, err = dep.PowerOn(ctx)
	} else {
		result, err = dep.PowerOff(ctx)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.ErrorMessage != "" {
		logf.FromContext(ctx).Error(errors.New(result.ErrorMessage), "power management failed",
			"machine", machine.Name, "desiredOnline", desired)
		return ctrl.Result{RequeueAfter: backoffBaseDelay}, nil
	}

	machine.Status.PoweredOn = &desired
	if err := r.Status().Update(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// needsProvisioning reports whether the machine's spec has drifted from
// the last successful deployment recorded in status.provisioning: a
// changed image reference for the OS image or any DataImages entry. This
// is a pure name comparison; an Image is immutable (its spec is
// create-only), so no generation or content comparison is necessary.
func (r *MachineReconciler) needsProvisioning(machine *keziov1alpha1.Machine) bool {
	if machine.Spec.ImageRef != nil {
		current := machine.Status.Provisioning
		if current == nil || current.Image == nil ||
			current.Image.ImageRef != *machine.Spec.ImageRef {
			return true
		}
	}

	for _, dataImage := range machine.Spec.DataImages {
		deployed := false
		if machine.Status.Provisioning != nil {
			for _, rec := range machine.Status.Provisioning.DataImages {
				if rec.ImageRef == dataImage.ImageRef {
					deployed = true
					break
				}
			}
		}
		if !deployed {
			return true
		}
	}

	return false
}

// resolveTargetDisks matches the machine's reported hardware inventory
// against the OS image's targetDisk hints and every dataImages[] entry's
// own hints, requires every resolved disk to be distinct, and records the
// result into status.provisioning before any deploy action is attempted
// (before dep.Provision is called). A match or distinct-disk error stops
// the deployment here; the caller moves the machine to the Error state
// with the returned message.
//
// It is idempotent: once status.provisioning already names the same image
// references the spec currently asks for, it does nothing, so repeated
// polls of an in-progress Provision do not re-resolve or rewrite status on
// every reconcile.
func (r *MachineReconciler) resolveTargetDisks(ctx context.Context, machine *keziov1alpha1.Machine) error {
	if r.provisioningStatusResolved(machine) {
		return nil
	}

	var disks []keziov1alpha1.MachineHardwareDisk
	if machine.Status.Hardware != nil {
		disks = machine.Status.Hardware.Disks
	}

	var selections []diskmatch.Selection

	var osDisk *keziov1alpha1.MachineHardwareDisk
	if machine.Spec.ImageRef != nil {
		resolved, err := diskmatch.Match(disks, machine.Spec.TargetDisk)
		if err != nil {
			return fmt.Errorf("resolving target disk for the OS image: %w", err)
		}
		osDisk = resolved
		selections = append(selections, diskmatch.Selection{Label: "OS image", Disk: osDisk})
	}

	dataDisks := make([]*keziov1alpha1.MachineHardwareDisk, len(machine.Spec.DataImages))
	for i, dataImage := range machine.Spec.DataImages {
		resolved, err := diskmatch.Match(disks, dataImage.TargetDisk)
		if err != nil {
			return fmt.Errorf("resolving target disk for dataImages[%d] (%s): %w", i, dataImage.ImageRef.Name, err)
		}
		dataDisks[i] = resolved
		selections = append(selections, diskmatch.Selection{Label: fmt.Sprintf("dataImages[%d]", i), Disk: resolved})
	}

	if err := diskmatch.CheckDistinct(selections); err != nil {
		return err
	}

	status := &keziov1alpha1.MachineProvisioningStatus{}
	if osDisk != nil {
		status.Image = &keziov1alpha1.MachineProvisionedImage{
			ImageRef:   *machine.Spec.ImageRef,
			TargetDisk: osDisk.DeviceName,
		}
	}
	for i, dataImage := range machine.Spec.DataImages {
		status.DataImages = append(status.DataImages, keziov1alpha1.MachineProvisionedImage{
			ImageRef:   dataImage.ImageRef,
			TargetDisk: dataDisks[i].DeviceName,
		})
	}

	machine.Status.Provisioning = status
	return r.Status().Update(ctx, machine)
}

// provisioningStatusResolved reports whether machine.Status.Provisioning
// already names the same OS image reference and the same set of
// dataImages references the spec currently asks for. It does not check
// the recorded target disks themselves: the hints and the inventory that
// produced them do not change while a machine sits in Provisioning, so a
// matching set of image references is enough to know resolution already
// ran for this deployment attempt.
func (r *MachineReconciler) provisioningStatusResolved(machine *keziov1alpha1.Machine) bool {
	status := machine.Status.Provisioning
	if status == nil {
		return false
	}

	if machine.Spec.ImageRef != nil {
		if status.Image == nil || status.Image.ImageRef != *machine.Spec.ImageRef {
			return false
		}
	} else if status.Image != nil {
		return false
	}

	if len(status.DataImages) != len(machine.Spec.DataImages) {
		return false
	}
	for i, dataImage := range machine.Spec.DataImages {
		if status.DataImages[i].ImageRef != dataImage.ImageRef {
			return false
		}
	}

	return true
}

// buildProvisioningStatus records what Provision actually deployed: each
// image reference and the resolved target disk Provision reported.
func (r *MachineReconciler) buildProvisioningStatus(machine *keziov1alpha1.Machine, data *deployer.ProvisionData) *keziov1alpha1.MachineProvisioningStatus {
	status := &keziov1alpha1.MachineProvisioningStatus{}

	if machine.Spec.ImageRef != nil {
		status.Image = &keziov1alpha1.MachineProvisionedImage{
			ImageRef:   *machine.Spec.ImageRef,
			TargetDisk: data.ResolvedTargetDisk,
		}
	}

	for i, dataImage := range machine.Spec.DataImages {
		disk := ""
		if i < len(data.ResolvedDataImageDisks) {
			disk = data.ResolvedDataImageDisks[i]
		}
		status.DataImages = append(status.DataImages, keziov1alpha1.MachineProvisionedImage{
			ImageRef:   dataImage.ImageRef,
			TargetDisk: disk,
		})
	}

	return status
}

// recordPhaseError moves the machine to the Error state, increments
// errorCount, and requeues after a backoff computed from the new
// errorCount. reason names the phase that failed, so reconcileError can
// resume the right step once the machine is retried.
func (r *MachineReconciler) recordPhaseError(ctx context.Context, machine *keziov1alpha1.Machine, reason, message string) (ctrl.Result, error) {
	machine.Status.State = keziov1alpha1.MachineStateError
	machine.Status.ErrorCount++
	machine.Status.ErrorMessage = message
	setReadyCondition(machine, metav1.ConditionFalse, reason, message)

	backoff := calculateBackoff(machine.Status.ErrorCount)
	if err := r.Status().Update(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: backoff}, nil
}

// advance moves the machine to newState, resets the error counters (a
// successful phase clears any prior error), sets the Ready condition, and
// persists the status. Reaching Provisioned sets Ready=True; every other
// state means the machine is still progressing, so Ready stays False with
// a reason naming the current step.
func (r *MachineReconciler) advance(ctx context.Context, machine *keziov1alpha1.Machine, newState, reason, message string) (ctrl.Result, error) {
	machine.Status.State = newState
	machine.Status.ErrorCount = 0
	machine.Status.ErrorMessage = ""

	condStatus := metav1.ConditionFalse
	if newState == keziov1alpha1.MachineStateProvisioned {
		condStatus = metav1.ConditionTrue
	}
	setReadyCondition(machine, condStatus, reason, message)

	if err := r.Status().Update(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setReadyCondition sets the shared Ready condition on the machine.
func setReadyCondition(machine *keziov1alpha1.Machine, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha1.Machine{}).
		Named("machine").
		Complete(r)
}
