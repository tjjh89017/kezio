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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/store"
)

// publishPollInterval is the safety-net requeue interval while a publish
// Job is running: the Job watch (Owns, unfiltered) normally retriggers a
// reconcile the moment the Job's status changes, so this is a fallback
// against a missed watch event rather than the primary progress signal.
var publishPollInterval = 30 * time.Second

// PartitionContentReconciler reconciles a PartitionContent object.
//
// This reconciler owns the content PVC, the publish Job's lifecycle
// (create, observe, reflect into status), and - once Ready - the seeder
// Deployment's lifecycle (create-on-demand, grace-period shutdown, status
// reflection; see reconcileSeeder). It also carries
// PartitionContentFinalizer, blocking actual removal while an Image slot
// or an active DeployRun still references this content (see onDelete). It
// never mounts the content PVC's
// filesystem itself: the publish Job is the sole witness to whether
// publishing succeeded (see outcomeOf), and status.torrentPath is set
// from the store package's naming convention, not from anything this
// reconciler read off disk.
type PartitionContentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes Events on PartitionContent, notably a
	// publish Job's success/failure. Required.
	Recorder record.EventRecorder
	// Publish configures the publish Job's image and tracker. The zero
	// value holds every PartitionContent at Pending - see
	// PartitionContentPublishConfig's doc comment.
	Publish PartitionContentPublishConfig
	// Seeder configures the seeder Deployment's image and grace period.
	// The zero value holds every seed-demanded content at
	// SeederDegraded=True instead of creating a half-configured
	// Deployment - see PartitionContentSeederConfig's doc comment.
	Seeder PartitionContentSeederConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PartitionContentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pc keziov1alpha2.PartitionContent
	if err := r.Get(ctx, req.NamespacedName, &pc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The content PVC and publish Job are owner-referenced to this object,
	// so ordinary Kubernetes garbage collection reclaims them once this
	// object is actually deleted; PartitionContentFinalizer only guards
	// the object itself from being removed while still referenced.
	if !pc.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, &pc)
	}

	if !controllerutil.ContainsFinalizer(&pc, keziov1alpha2.PartitionContentFinalizer) {
		controllerutil.AddFinalizer(&pc, keziov1alpha2.PartitionContentFinalizer)
		if err := r.Update(ctx, &pc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	hash, err := store.ParseInfoHash(strings.TrimPrefix(pc.Name, "pc-"))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: name is not a valid content hash: %w", pc.Name, err)
	}

	return r.onChange(ctx, &pc, hash)
}

// onChange drives one step of the publish walk: ensure the content PVC,
// then Pending -> Publishing -> Ready|Failed. An already-Ready object
// never re-enters the publish walk (see the branch below) - this is the
// dedupe guarantee the seeder lifecycle relies on: reaching Ready never
// re-triggers a second publish Job for the same content hash. Once
// Ready, reconcileSeeder takes over instead.
func (r *PartitionContentReconciler) onChange(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) (ctrl.Result, error) {
	pvc, err := r.ensureContentPVC(ctx, pc, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	if pc.Status.State == keziov1alpha2.PartitionContentStateReady {
		return r.reconcileSeeder(ctx, pc, hash)
	}

	job, err := r.publishJobFor(ctx, pc, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	if job == nil {
		if !r.Publish.ready() {
			return r.recordPending(ctx, pc, pvc)
		}
		if err := r.createPublishJob(ctx, pc, hash, pvc.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordPublishing(ctx, pc, pvc)
	}

	switch outcomeOf(job) {
	case publishJobSucceeded:
		return r.recordReady(ctx, pc, pvc)
	case publishJobFailed:
		return r.recordFailed(ctx, pc, job)
	default:
		return r.recordPublishing(ctx, pc, pvc)
	}
}

// recordPending records Pending with a condition explaining that the
// manager has no publish image/tracker configured. This is a visible,
// non-error hold, not a failure: it clears on its own, with no operator
// action on this object, once the manager is configured and restarted
// (Publish is read once from the environment at startup - see
// PartitionContentPublishConfig).
func (r *PartitionContentReconciler) recordPending(ctx context.Context, pc *keziov1alpha2.PartitionContent, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha2.PartitionContentStatePending
	pc.Status.PVCRef = &keziov1alpha2.NameRef{Name: pvc.Name}
	setPartitionContentReadyCondition(pc, metav1.ConditionFalse,
		"PublishConfigMissing", "no publish Job image or tracker URL is configured on the manager; content stays Pending until it is")
	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Pending: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordPublishing records Publishing: the publish Job exists and has
// not yet reported success or failure.
func (r *PartitionContentReconciler) recordPublishing(ctx context.Context, pc *keziov1alpha2.PartitionContent, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha2.PartitionContentStatePublishing
	pc.Status.PVCRef = &keziov1alpha2.NameRef{Name: pvc.Name}
	setPartitionContentReadyCondition(pc, metav1.ConditionFalse,
		"Publishing", "publish job is running")
	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Publishing: %w", pc.Name, err)
	}
	return ctrl.Result{RequeueAfter: publishPollInterval}, nil
}

// recordReady records Ready: the publish Job succeeded, so status.torrentPath
// is set to the store package's fixed .torrent file name - a path
// relative to the content PVC's root (see PartitionContentStatus.TorrentPath's
// doc comment: "within the PVC"), the same frame of reference a seeder
// resolves against once it mounts that PVC.
func (r *PartitionContentReconciler) recordReady(ctx context.Context, pc *keziov1alpha2.PartitionContent, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha2.PartitionContentStateReady
	pc.Status.PVCRef = &keziov1alpha2.NameRef{Name: pvc.Name}
	pc.Status.TorrentPath = store.ContentTorrentFileName
	setPartitionContentReadyCondition(pc, metav1.ConditionTrue,
		"PublishJobSucceeded", "publish job succeeded; the .torrent is present in the content PVC")
	onSuccess := func() {
		r.Recorder.Event(pc, corev1.EventTypeNormal, "PartitionContentReady", "publish job succeeded")
	}
	if err := r.applyPartitionContentStatus(ctx, pc, onSuccess); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Ready: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordFailed records Failed: the publish Job failed terminally. There
// is no automatic retry in this item - an operator (or a later item)
// deleting the failed Job and/or this object is what re-enters the walk.
func (r *PartitionContentReconciler) recordFailed(ctx context.Context, pc *keziov1alpha2.PartitionContent, job *batchv1.Job) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha2.PartitionContentStateFailed
	message := fmt.Sprintf("publish job %q failed", job.Name)
	setPartitionContentReadyCondition(pc, metav1.ConditionFalse,
		"PublishJobFailed", message)
	onSuccess := func() {
		r.Recorder.Event(pc, corev1.EventTypeWarning, "PartitionContentPublishFailed", message)
	}
	if err := r.applyPartitionContentStatus(ctx, pc, onSuccess); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Failed: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// partitionContentUpdatePredicate restricts the PartitionContent watch's
// Update events to a generation, finalizer, or annotation change - a
// status-only self-write (the publish walk's own progress) must not
// re-trigger the reconciler on its own, mirroring
// machineUpdatePredicate. Create/Delete/Generic events stay unfiltered.
var partitionContentUpdatePredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	finalizersChangedPredicate,
)

// imageDeletionOnly restricts the Image watch to Delete events: ImageSpec
// is immutable once created (see ImageSpec's type-level XValidation rule),
// so only an Image's creation or removal can change what it references -
// and only removal can ever unblock a PartitionContent's pending delete
// (see onDelete), mirroring machine_controller's deployRunDeletionOnly.
var imageDeletionOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// SetupWithManager sets up the controller with the Manager.
func (r *PartitionContentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensureImageContentRefIndex(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.PartitionContent{}, builder.WithPredicates(partitionContentUpdatePredicate)).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.Deployment{}).
		Watches(&keziov1alpha2.Image{}, handler.EnqueueRequestsFromMapFunc(r.mapImageToPartitionContents), builder.WithPredicates(imageDeletionOnly)).
		Named("partitioncontent").
		Complete(r)
}
