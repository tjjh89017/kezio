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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// inspectingStuckThreshold is how long a machine may sit in Inspecting
// before reconcileInspecting reports it as failed (see pollInspecting).
// It is comfortably longer than the poll cadence, so a machine still
// legitimately net-booting is never mistaken for stuck.
//
// No automatic recovery (for example a BMC power-cycle) is attempted
// once this trips: nothing observable at this layer distinguishes "the
// live environment never booted" from "kezio-agent is still working", so
// an unconditional power action could interrupt real progress and repeat
// forever. Reporting the failure and stopping is the safer default; a
// human decides whether a power-cycle is the right recovery.
const inspectingStuckThreshold = 10 * time.Minute

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
		BMC:            &machine.Spec.BMC,
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

	// Register (internal/deployer/agent.go) writes Machine status itself,
	// through a separate Get/Update, so this reconcile's copy is stale by
	// the time Register returns: advancing from it would conflict on
	// Status().Update, and the resulting requeue would re-enter
	// reconcileEnrolling and call Register again, re-arming the BMC's
	// one-time PXE boot against a machine already mid-boot from the
	// first call. Re-fetching first avoids racing Register's own write,
	// so a successful Register is only ever acted on once per Machine
	// generation.
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	machine.Status.InspectingSince = &now
	return r.advance(ctx, machine, keziov1alpha1.MachineStateInspecting, reasonInspecting,
		"Machine registered; collecting hardware inventory")
}

// reconcileInspecting collects hardware inventory from the deployer,
// driving Inspecting -> Available. While still waiting, it also watches
// for a stuck machine (see pollInspecting) and reports it as failed once
// inspectingStuckThreshold has elapsed.
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
		return r.pollInspecting(ctx, machine, result.RequeueAfter)
	}

	machine.Status.Hardware = data.Hardware
	return r.advance(ctx, machine, keziov1alpha1.MachineStateAvailable, reasonAvailable,
		"Hardware inventory collected; machine is available for deployment")
}

// pollInspecting requeues an in-progress inspection after requeueAfter,
// unless the machine has spent longer than inspectingStuckThreshold
// waiting for kezio-agent to register (status.inspectingSince, set in
// reconcileEnrolling), in which case it reports the failure through
// recordPhaseError. See inspectingStuckThreshold's doc comment for why
// no automatic recovery is attempted.
//
// Reporting the failure also restarts inspectingSince to now: the Error
// transition triggers an immediate re-reconcile straight back into
// reconcileInspecting (via reconcileError), and without resetting the
// clock that retry - and every one after it - would see the same
// already-elapsed timestamp and re-trip this branch on every single
// reconcile instead of once per inspectingStuckThreshold window.
func (r *MachineReconciler) pollInspecting(ctx context.Context, machine *keziov1alpha1.Machine, requeueAfter time.Duration) (ctrl.Result, error) {
	since := machine.Status.InspectingSince
	if since == nil || time.Since(since.Time) < inspectingStuckThreshold {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	now := metav1.Now()
	machine.Status.InspectingSince = &now
	return r.recordPhaseError(ctx, machine, reasonInspectFailed, fmt.Sprintf(
		"kezio-agent did not register within %s of entering Inspecting; power-cycle the machine manually or check that it can reach the boot/agent services over the network",
		inspectingStuckThreshold))
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
// does carry one always reboots into it via the agent's own
// "systemctl reboot", never through this method. For the no-OS-image
// case, AfterDeployReboot/AfterDeployPowerOff are handled here through
// dep.PowerCycle/dep.PowerOff, the same Deployer methods reconcilePower
// uses: BMC-driven (forced reset / graceful shutdown) when the Machine
// has one configured, a documented no-op on agentDeployer otherwise - in
// which case AfterDeployReboot falls back to the agent's own reboot and
// AfterDeployPowerOff is left to an operator or a future agent-driven
// poweroff rather than guessed at here. Either way this never races the
// agent's own reboot, since it only fires for the no-OS-image path that
// reboot never touches.
func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *keziov1alpha1.Machine, dep deployer.Deployer) (ctrl.Result, error) {
	if message, err := r.checkReferencedImagesFailed(ctx, machine); err != nil {
		return ctrl.Result{}, err
	} else if message != "" {
		return r.recordPhaseError(ctx, machine, reasonProvisionFailed, message)
	}

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

	if machine.Spec.ImageRef == nil {
		switch machine.Spec.EffectiveAfterDeploy() {
		case keziov1alpha1.AfterDeployPowerOff:
			poResult, poErr := dep.PowerOff(ctx)
			if poErr != nil {
				return ctrl.Result{}, poErr
			}
			if poResult.ErrorMessage != "" {
				return r.recordPhaseError(ctx, machine, reasonProvisionFailed, poResult.ErrorMessage)
			}
			applyObservedPower(machine, poResult, false)
		case keziov1alpha1.AfterDeployReboot:
			rcResult, rcErr := dep.PowerCycle(ctx)
			if rcErr != nil {
				return ctrl.Result{}, rcErr
			}
			if rcResult.ErrorMessage != "" {
				return r.recordPhaseError(ctx, machine, reasonProvisionFailed, rcResult.ErrorMessage)
			}
			// Only record an actual BMC observation here: the no-BMC
			// fallback leaves this reboot to the agent's own "systemctl
			// reboot" (dep.PowerCycle no-ops in that case), so there is
			// nothing observed to overwrite status.poweredOn with.
			if rcResult.PoweredOn != nil {
				machine.Status.PoweredOn = rcResult.PoweredOn
			}
		}
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
	log := logf.FromContext(ctx)
	desired := machine.Spec.EffectiveOnline()
	if machine.Status.PoweredOn != nil && *machine.Status.PoweredOn == desired {
		return ctrl.Result{}, nil
	}

	log.Info("power state mismatch, issuing BMC power command",
		"machine", machine.Name, "desiredOnline", desired)

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

	applyObservedPower(machine, result, desired)
	if err := r.Status().Update(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}

	// A BMC power command can succeed before the machine actually reaches
	// the desired state: PowerOff requests a graceful, orderly shutdown
	// (see internal/bmc/redfish's PowerOff doc comment) that can take
	// much longer than this read-back, and PowerOn can equally race a
	// BMC that has not yet reported the machine on. Recording that
	// mismatched observation (applyObservedPower above) without a retry
	// would let it sit as status.poweredOn until the next unrelated
	// change, where it would wrongly look like a *confirmed* state to
	// compare a later spec.online flip against - the trap that let a
	// stale "still on" reading after a graceful PowerOff cause a
	// subsequent PowerOn to be skipped as a no-op. Requeuing here instead
	// keeps polling GetPowerState (via this same idempotent
	// PowerOn/PowerOff call) until the observation agrees with desired.
	if result.PoweredOn != nil && *result.PoweredOn != desired {
		log.Info("BMC power command accepted but machine has not reached the desired state yet, retrying",
			"machine", machine.Name, "desiredOnline", desired, "observedPoweredOn", *result.PoweredOn)
		return ctrl.Result{RequeueAfter: backoffBaseDelay}, nil
	}

	log.Info("power state now matches spec.online",
		"machine", machine.Name, "desiredOnline", desired)
	return ctrl.Result{}, nil
}

// applyObservedPower records the machine's power state after a
// PowerOn/PowerOff call succeeds: result.PoweredOn when the deployer
// actually read it back from the BMC (see deployer.Result.PoweredOn's
// doc comment), or desired - the commanded state - when it did not. The
// latter is the no-BMC fallback, where nothing observes the machine's
// real state and the commanded state is the best available answer.
func applyObservedPower(machine *keziov1alpha1.Machine, result deployer.Result, desired bool) {
	if result.PoweredOn != nil {
		machine.Status.PoweredOn = result.PoweredOn
		return
	}
	machine.Status.PoweredOn = &desired
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

// checkReferencedImagesFailed reports whether the OS image or any
// dataImages entry the machine references has reached ImageStateFailed -
// the terminal state a checksum mismatch or a failed ingest job drives an
// Image to (see ImageReconciler's handleIngestJobSucceeded and
// handleIngestJobFailed). The agent-facing plan builder treats a failed
// Image the same as one that is simply not Ready yet (see
// agentserver.buildImagePlan's doc comment), so it never hands the agent
// anything to report failure on; without this check a machine referencing
// a failed Image would poll Provision forever instead of ever reaching
// Error. A missing Image is left alone here - the target-disk resolution
// and deploy-plan paths already surface that case on their own.
func (r *MachineReconciler) checkReferencedImagesFailed(ctx context.Context, machine *keziov1alpha1.Machine) (string, error) {
	refs := make([]keziov1alpha1.NameRef, 0, 1+len(machine.Spec.DataImages))
	if machine.Spec.ImageRef != nil {
		refs = append(refs, *machine.Spec.ImageRef)
	}
	for _, dataImage := range machine.Spec.DataImages {
		refs = append(refs, dataImage.ImageRef)
	}

	for _, ref := range refs {
		ns := keziov1alpha1.ResolveNamespace(ref, machine.Namespace)
		image := &keziov1alpha1.Image{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, image); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("get image %s/%s: %w", ns, ref.Name, err)
		}
		if image.Status.State == keziov1alpha1.ImageStateFailed {
			return fmt.Sprintf("referenced image %s/%s failed ingest", ns, ref.Name), nil
		}
	}

	return "", nil
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
