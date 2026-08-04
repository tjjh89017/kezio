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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Ready condition reasons used while an Image progresses through its
// ingest state machine.
const (
	reasonAwaitingUpload = "AwaitingUpload"
	reasonIngesting      = "Ingesting"
	reasonIngestComplete = "Ready"
)

// imageInUseRequeueInterval is the fallback poll interval while deletion
// of an in-use Image is blocked. A Machine change that drops the last
// reference also requeues the Image directly (see mapMachineToImages);
// this interval only covers the gap until the next such event.
const imageInUseRequeueInterval = 30 * time.Second

// ImageReconciler reconciles a Image object
type ImageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Ingest configures real ingest Jobs. Its zero value keeps the
	// stub fast path described on onChange, so existing deployments
	// and the e2e suite are unaffected until it is explicitly wired up
	// (see IngestConfig's doc comment).
	Ingest IngestConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile fetches the Image, dispatches to onDelete if it is being
// deleted, and otherwise ensures the finalizer is present before
// dispatching to onChange.
//
// An Image is immutable once created (its spec is create-only), so
// deletion is the only way to retire one. A finalizer protects an Image
// that a Machine still references — through spec.imageRef,
// spec.dataImages, or a recorded status.provisioning entry — so that
// deleting it never leaves a Machine pointing at content that no longer
// exists. The finalizer is removed once no Machine references the
// Image.
func (r *ImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	image := &keziov1alpha1.Image{}
	if err := r.Get(ctx, req.NamespacedName, image); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !image.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, req, image)
	}

	if !controllerutil.ContainsFinalizer(image, keziov1alpha1.FinalizerName) {
		patch := client.MergeFrom(image.DeepCopy())
		controllerutil.AddFinalizer(image, keziov1alpha1.FinalizerName)
		if err := r.Patch(ctx, image, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return r.onChange(ctx, req, image)
}

// onChange drives an Image through its ingest state machine: Pending ->
// Ingesting -> Ready. An Image with no source URL (one that awaits a
// direct upload instead) stays Pending, with the Ready condition naming
// the reason, until a URL appears.
//
// Ingesting has two modes, chosen by whether r.Ingest is configured
// (r.Ingest.Image != ""):
//
//   - Stub mode (the zero value, and the only mode until an operator
//     wires up r.Ingest): no ingest Job runs. An Image with a source URL
//     passes through Ingesting on the next reconcile and reaches Ready
//     with no disk or partition status populated. This keeps the state
//     machine shape and the Ready condition contract stable for callers
//     (the Machine reconciler, kezioctl, e2e tests) that only need an
//     Image to reach Ready, without requiring the ingest pipeline's
//     container image or store/staging volumes to exist. The e2e suite
//     runs in this mode today.
//   - Ingest mode: reconcileIngesting creates (or adopts) a real ingest
//     Job and polls it to completion, populating status.disk and
//     status.partitions from its result before advancing to Ready (or
//     Failed).
func (r *ImageReconciler) onChange(ctx context.Context, _ ctrl.Request, image *keziov1alpha1.Image) (ctrl.Result, error) {
	switch image.Status.State {
	case "", keziov1alpha1.ImageStatePending:
		if image.Spec.Source.URL == "" {
			return r.advance(ctx, image, keziov1alpha1.ImageStatePending, reasonAwaitingUpload,
				"Image has no source URL; waiting for a direct upload")
		}
		return r.advance(ctx, image, keziov1alpha1.ImageStateIngesting, reasonIngesting,
			"Source image found; ingest starting")
	case keziov1alpha1.ImageStateIngesting:
		if !r.Ingest.enabled() {
			return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete,
				"Image ingest complete")
		}
		return r.reconcileIngesting(ctx, image)
	default:
		// Ready and Failed are steady states at this stage: nothing to do
		// until re-ingest on a spec change becomes a feature.
		return ctrl.Result{}, nil
	}
}

// onDelete blocks deletion of an Image that a Machine still references, and
// otherwise removes the finalizer so the API server can finish deleting the
// object.
func (r *ImageReconciler) onDelete(ctx context.Context, _ ctrl.Request, image *keziov1alpha1.Image) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(image, keziov1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}

	inUse, err := r.isImageInUse(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inUse {
		return ctrl.Result{RequeueAfter: imageInUseRequeueInterval}, nil
	}

	patch := client.MergeFrom(image.DeepCopy())
	controllerutil.RemoveFinalizer(image, keziov1alpha1.FinalizerName)
	if err := r.Patch(ctx, image, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// isImageInUse reports whether any Machine references image, through
// spec.imageRef, spec.dataImages, or a recorded status.provisioning
// entry.
func (r *ImageReconciler) isImageInUse(ctx context.Context, image *keziov1alpha1.Image) (bool, error) {
	machines := &keziov1alpha1.MachineList{}
	if err := r.List(ctx, machines); err != nil {
		return false, err
	}

	for i := range machines.Items {
		machine := &machines.Items[i]
		for _, ref := range collectImageRefs(machine) {
			if ref.Name == image.Name && keziov1alpha1.ResolveNamespace(ref, machine.Namespace) == image.Namespace {
				return true, nil
			}
		}
	}
	return false, nil
}

// collectImageRefs returns every Image reference a Machine carries: its
// desired OS and data images (spec), plus what it last successfully
// deployed (status.provisioning). Both are checked so a deployed Image
// stays protected even if a later spec edit drops the reference before
// the Machine is redeployed or deleted.
func collectImageRefs(machine *keziov1alpha1.Machine) []keziov1alpha1.NameRef {
	var refs []keziov1alpha1.NameRef

	if machine.Spec.ImageRef != nil {
		refs = append(refs, *machine.Spec.ImageRef)
	}
	for _, dataImage := range machine.Spec.DataImages {
		refs = append(refs, dataImage.ImageRef)
	}

	if machine.Status.Provisioning != nil {
		if machine.Status.Provisioning.Image != nil {
			refs = append(refs, machine.Status.Provisioning.Image.ImageRef)
		}
		for _, dataImage := range machine.Status.Provisioning.DataImages {
			refs = append(refs, dataImage.ImageRef)
		}
	}

	return refs
}

// mapMachineToImages requeues every Image a Machine references whenever
// that Machine changes, so an Image blocked on deletion is re-checked
// promptly once the reference that blocked it is removed (rather than
// waiting for imageInUseRequeueInterval to elapse).
func mapMachineToImages(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		return nil
	}

	seen := map[types.NamespacedName]struct{}{}
	var requests []reconcile.Request
	for _, ref := range collectImageRefs(machine) {
		key := types.NamespacedName{
			Namespace: keziov1alpha1.ResolveNamespace(ref, machine.Namespace),
			Name:      ref.Name,
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

// advance moves the image to newState, sets the Ready condition, and
// persists the status. Reaching Ready sets Ready=True; every other state
// means the image is still progressing (or waiting on an upload), so
// Ready stays False with a reason naming why.
func (r *ImageReconciler) advance(ctx context.Context, image *keziov1alpha1.Image, newState, reason, message string) (ctrl.Result, error) {
	image.Status.State = newState

	condStatus := metav1.ConditionFalse
	if newState == keziov1alpha1.ImageStateReady {
		condStatus = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.ConditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: image.Generation,
	})

	if err := r.Status().Update(ctx, image); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha1.Image{}).
		Owns(&batchv1.Job{}).
		Watches(&keziov1alpha1.Machine{}, handler.EnqueueRequestsFromMapFunc(mapMachineToImages)).
		Named("image").
		Complete(r)
}
