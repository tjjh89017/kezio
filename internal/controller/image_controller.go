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
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageStaleContentRequeueInterval is how soon the reconciler retries
// after finding a referenced PartitionContent's Ready condition stale
// (see aggregateSlotContents) - short, since the referenced content's own
// controller is expected to catch its condition up to its current
// generation quickly, and the PartitionContent watch (see
// SetupWithManager) normally retriggers a reconcile before this fires
// anyway.
var imageStaleContentRequeueInterval = 5 * time.Second

// ImageReconciler reconciles a Image object.
//
// This reconciler aggregates the readiness of an Image's referenced
// PartitionContent objects into Ready/Valid conditions and status.state.
// It owns no seeders (those belong to PartitionContent). A composed Image
// (no spec.source) is create-only metadata over existing content:
// reconciling it is pure aggregation, triggering no ingest. A
// source-bearing Image additionally owns one ingest Job and its scratch
// PVC (see image_ingest.go): reconcileIngesting creates or observes that
// Job and, once it succeeds, creates the PartitionContent objects its
// declared contentRef slots name (or reuses them, content-addressed, if
// they already exist) - it never creates a PartitionContent's own
// content PVC or publish Job itself, those stay PartitionContent's own.
type ImageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Ingest configures the ingest Job an Image with spec.source needs.
	// The zero value holds every such Image at Pending with a condition
	// explaining why - see ImageIngestConfig's doc comment.
	Ingest ImageIngestConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var image keziov1alpha2.Image
	if err := r.Get(ctx, req.NamespacedName, &image); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return r.onChange(ctx, &image)
}

// onChange aggregates image's referenced PartitionContent readiness into
// its Ready/Valid conditions and status.state.
func (r *ImageReconciler) onChange(ctx context.Context, image *keziov1alpha2.Image) (ctrl.Result, error) {
	agg, err := r.aggregateSlotContents(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Stale: some referenced content's Ready condition has not caught up
	// to that content's own current generation. The cross-reference
	// contract (see aggregateSlotContents) requires retrying rather than
	// acting on that data, so this Image's status is left untouched.
	if agg.stale {
		return ctrl.Result{RequeueAfter: imageStaleContentRequeueInterval}, nil
	}

	if image.Spec.Source != nil {
		image.Status.SourceChecksum = image.Spec.Source.Checksum
	}

	validOK := len(agg.invalidSizes) == 0
	setImageValidCondition(image, validOK, agg.invalidSizes)

	switch {
	case len(agg.failed) > 0:
		return r.recordFailed(ctx, image, agg.failed)
	case len(agg.notReady) == 0 && validOK:
		return r.recordReady(ctx, image)
	case len(agg.notReady) == 0:
		return r.recordInvalid(ctx, image, agg.invalidSizes)
	case image.Spec.Source != nil:
		return r.reconcileIngestPending(ctx, image, agg.notReady)
	default:
		return r.recordPending(ctx, image, agg.notReady)
	}
}

// imageUpdatePredicate restricts the Image watch's Update events to a
// generation or annotation change - ImageSpec is immutable once created
// (see ImageSpec's type-level XValidation rule), so a status-only
// self-write (this reconciler's own aggregation result) must not
// re-trigger the reconciler on its own, mirroring
// partitionContentUpdatePredicate. Create/Delete/Generic events stay
// unfiltered.
var imageUpdatePredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
)

// partitionContentStatusChangedPredicate reacts to a PartitionContent's
// status changing. Generation never changes after creation
// (PartitionContentSpec is immutable), so a generation-based predicate
// like partitionContentUpdatePredicate would never fire for the status
// transitions this watch exists to catch; comparing Status directly is
// the only way to filter a PartitionContent reconcile that changed
// nothing an Image cares about.
var partitionContentStatusChangedPredicate = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	DeleteFunc: func(event.DeleteEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldPC, ok := e.ObjectOld.(*keziov1alpha2.PartitionContent)
		if !ok {
			return true
		}
		newPC, ok := e.ObjectNew.(*keziov1alpha2.PartitionContent)
		if !ok {
			return true
		}
		return !reflect.DeepEqual(oldPC.Status, newPC.Status)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// mapPartitionContentToImages maps a PartitionContent event to a
// reconcile request per Image that references it, via
// imageContentRefIndex's reverse lookup - the watch that lets an Image's
// aggregated status catch up the moment the content it references
// changes, without waiting on imageStaleContentRequeueInterval or any
// other poll. Mirrors PartitionContentReconciler.mapImageToPartitionContents'
// same-namespace assumption: a slot's contentRef is resolved in the
// Image's own namespace unless it sets an explicit Namespace (see
// aggregateSlotContents), but the index and this reverse lookup only key
// on content name within the content's namespace.
func (r *ImageReconciler) mapPartitionContentToImages(ctx context.Context, obj client.Object) []reconcile.Request {
	pc, ok := obj.(*keziov1alpha2.PartitionContent)
	if !ok {
		return nil
	}
	var images keziov1alpha2.ImageList
	if err := r.List(ctx, &images, client.InNamespace(pc.Namespace), client.MatchingFields{imageContentRefIndex: pc.Name}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(images.Items))
	for _, image := range images.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: image.Namespace, Name: image.Name},
		})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensureImageContentRefIndex(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.Image{}, builder.WithPredicates(imageUpdatePredicate)).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&keziov1alpha2.PartitionContent{}, handler.EnqueueRequestsFromMapFunc(r.mapPartitionContentToImages), builder.WithPredicates(partitionContentStatusChangedPredicate)).
		Named("image").
		Complete(r)
}
