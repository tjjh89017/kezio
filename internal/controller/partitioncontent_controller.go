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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
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
// (create, observe, reflect into status), and - once Ready - reflects the
// real per-Site seeder placement Images referencing this content maintain
// into status.seeders[]/SeederDegraded (see reconcileSeeder). It never
// creates or owns a seeder Deployment itself: that lives per (Image,
// Site), owned by whichever Image references this content
// (ImageReconciler). It also carries PartitionContentFinalizer, blocking
// actual removal while an Image slot or an active DeployRun still
// references this content (see onDelete). It never mounts the content
// PVC's filesystem itself: the publish Job is the sole witness to
// whether publishing succeeded (see outcomeOf). Reaching Ready needs no
// tracker configured anywhere - content readiness is independent of the
// network-plane setting a Site's tracker is.
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
	// Seeder is the same ImageSeederConfig the Image reconciler holds -
	// wired from the same manager-wide setting in cmd/main.go. Read only
	// for its ready() check: a demanded Site whose Image never gets a
	// seeder Deployment because no seeder image is configured is a
	// different fact from one whose Deployment merely is not available
	// yet, and this reconciler needs the same config the Image reconciler
	// checks to tell the two apart in status.seeders[]'s degraded reason.
	Seeder ImageSeederConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=deployruns,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PartitionContentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pc keziov1alpha3.PartitionContent
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

	if !controllerutil.ContainsFinalizer(&pc, keziov1alpha3.PartitionContentFinalizer) {
		controllerutil.AddFinalizer(&pc, keziov1alpha3.PartitionContentFinalizer)
		if err := r.Update(ctx, &pc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.onChange(ctx, &pc)
}

// onChange drives one step of the publish walk: ensure the content PVC,
// then Pending -> Publishing -> Ready|Failed. An already-Ready object
// never re-enters the publish walk (see the branch below) - this is what
// the seeder lifecycle relies on: reaching Ready never re-triggers a
// second publish Job for the same content. Once Ready, reconcileSeeder
// takes over instead.
func (r *PartitionContentReconciler) onChange(ctx context.Context, pc *keziov1alpha3.PartitionContent) (ctrl.Result, error) {
	pvc, err := r.ensureContentPVC(ctx, pc)
	if err != nil {
		return ctrl.Result{}, err
	}
	setPartitionContentValidCondition(pc)

	if pc.Status.State == keziov1alpha3.PartitionContentStateReady {
		return r.reconcileSeeder(ctx, pc)
	}

	job, err := r.publishJobFor(ctx, pc)
	if err != nil {
		return ctrl.Result{}, err
	}

	if job == nil {
		if !r.Publish.ready() {
			return r.recordPending(ctx, pc, pvc)
		}
		if err := r.createPublishJob(ctx, pc, pvc.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordPublishing(ctx, pc, pvc)
	}

	switch outcomeOf(job) {
	case jobSucceeded:
		return r.completePublish(ctx, pc, pvc, job)
	case jobFailed:
		return r.recordFailed(ctx, pc, job)
	default:
		return r.recordPublishing(ctx, pc, pvc)
	}
}

// completePublish handles a successfully completed publish Job: it reads
// the info hash the Job computed from the bytes it actually wrote into
// the content PVC, and records it alongside Ready.
//
// The hash has to travel this way round. It only exists once partclone
// has run, so it cannot be declared in spec; and this reconciler can
// never mount the content PVC to compute it itself (see the type's doc
// comment), so the Job that did the writing is the only witness to it.
// A Job that succeeded but reported no hash is a failure: without it,
// nothing downstream can address this content's swarm.
func (r *PartitionContentReconciler) completePublish(
	ctx context.Context, pc *keziov1alpha3.PartitionContent, pvc *corev1.PersistentVolumeClaim, job *batchv1.Job,
) (ctrl.Result, error) {
	result, err := readJobResult(ctx, r.Client, pc.Namespace, job)
	if err != nil {
		return r.recordFailedMessage(ctx, pc, fmt.Sprintf("reading publish result: %s", err))
	}
	if !result.Success {
		return r.recordFailedMessage(ctx, pc, "publish job reported failure: "+result.Error)
	}
	if result.Publish == nil || result.Publish.InfoHash == "" {
		return r.recordFailedMessage(ctx, pc, "publish job reported success with no info hash")
	}
	if _, err := store.ParseInfoHash(result.Publish.InfoHash); err != nil {
		return r.recordFailedMessage(ctx, pc, fmt.Sprintf("publish job reported an invalid info hash: %s", err))
	}

	return r.recordReady(ctx, pc, pvc, result.Publish.InfoHash)
}

// recordPending records Pending with a condition explaining that the
// manager has no publish image configured. This is a visible, non-error
// hold, not a failure: it clears on its own, with no operator action on
// this object, once the manager is configured and restarted (Publish is
// read once from the environment at startup - see
// PartitionContentPublishConfig).
func (r *PartitionContentReconciler) recordPending(ctx context.Context, pc *keziov1alpha3.PartitionContent, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha3.PartitionContentStatePending
	pc.Status.PVCRef = &keziov1alpha3.NameRef{Name: pvc.Name}
	setPartitionContentReadyCondition(pc, metav1.ConditionFalse,
		"PublishConfigMissing", "no publish Job image is configured on the manager; content stays Pending until it is")
	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Pending: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordPublishing records Publishing: the publish Job exists and has
// not yet reported success or failure.
func (r *PartitionContentReconciler) recordPublishing(ctx context.Context, pc *keziov1alpha3.PartitionContent, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha3.PartitionContentStatePublishing
	pc.Status.PVCRef = &keziov1alpha3.NameRef{Name: pvc.Name}
	setPartitionContentReadyCondition(pc, metav1.ConditionFalse,
		"Publishing", "publish job is running")
	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording Publishing: %w", pc.Name, err)
	}
	return ctrl.Result{RequeueAfter: publishPollInterval}, nil
}

// recordReady records Ready: the publish Job succeeded, so the content
// PVC holds a validated torrent.info a seeder at any Site can build a
// .torrent from, and infoHash names the swarm that content forms.
func (r *PartitionContentReconciler) recordReady(ctx context.Context, pc *keziov1alpha3.PartitionContent, pvc *corev1.PersistentVolumeClaim, infoHash string) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha3.PartitionContentStateReady
	pc.Status.PVCRef = &keziov1alpha3.NameRef{Name: pvc.Name}
	pc.Status.InfoHash = infoHash
	setPartitionContentReadyCondition(pc, metav1.ConditionTrue,
		"PublishJobSucceeded", "publish job succeeded; torrent.info is present in the content PVC")
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
func (r *PartitionContentReconciler) recordFailed(ctx context.Context, pc *keziov1alpha3.PartitionContent, job *batchv1.Job) (ctrl.Result, error) {
	return r.recordFailedMessage(ctx, pc, fmt.Sprintf("publish job %q failed", job.Name))
}

// recordFailedMessage records Failed with an explicit message: the
// publish Job failed, or it succeeded but its result could not be
// trusted.
func (r *PartitionContentReconciler) recordFailedMessage(ctx context.Context, pc *keziov1alpha3.PartitionContent, message string) (ctrl.Result, error) {
	pc.Status.State = keziov1alpha3.PartitionContentStateFailed
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

// imageCreateOrDeleteOnly restricts the Image watch to Create and Delete
// events: ImageSpec is immutable once created (see ImageSpec's type-level
// XValidation rule), so only an Image's creation or removal can ever
// change what it references. Removal is what can unblock a
// PartitionContent's pending delete (see onDelete); creation is what can
// complete a MachineClaim's already-set imageRef into seed demand for
// content the new Image's slots reference (see resolveSeedDemand) - a
// claim created before its intended Image exists must still pick up
// demand once that Image shows up. Mirrors machine_controller's
// deployRunDeletionOnly.
var imageCreateOrDeleteOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// claimDeletionTimestampSetPredicate reports true only on an Update event
// where obj newly acquired a deletion timestamp: setting one does not
// change metadata.generation, so predicate.GenerationChangedPredicate
// alone never notices a MachineClaim demand source disappearing this way.
var claimDeletionTimestampSetPredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}

// claimDemandPredicate restricts the MachineClaim watch's Update events to
// a spec change (imageRef/dataImages, via generation) or a newly-set
// deletion timestamp - the two ways a MachineClaim's contribution to seed
// demand can change. Create/Delete/Generic events stay unfiltered:
// predicate.Or keeps each branch predicate's default (true) for them.
var claimDemandPredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	claimDeletionTimestampSetPredicate,
)

// deployRunDemandPredicate restricts the DeployRun watch's Update events
// to a phase change - in particular, entering or leaving the terminal
// phases isDeployRunActive tests. Create/Delete/Generic events stay
// unfiltered (predicate.Funcs' default is true): a fresh DeployRun starts
// active (see isDeployRunActive) and so is a demand source from creation,
// and its removal must drop that demand too.
var deployRunDemandPredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldRun, ok := e.ObjectOld.(*keziov1alpha3.DeployRun)
		if !ok {
			return true
		}
		newRun, ok := e.ObjectNew.(*keziov1alpha3.DeployRun)
		if !ok {
			return true
		}
		return oldRun.Status.Phase != newRun.Status.Phase
	},
}

// SetupWithManager sets up the controller with the Manager.
func (r *PartitionContentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensureImageContentRefIndex(mgr); err != nil {
		return err
	}
	if err := ensureClaimImageRefIndex(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha3.PartitionContent{}, builder.WithPredicates(partitionContentUpdatePredicate)).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Watches(&keziov1alpha3.Image{}, handler.EnqueueRequestsFromMapFunc(r.mapImageToPartitionContents), builder.WithPredicates(imageCreateOrDeleteOnly)).
		Watches(&keziov1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineToPartitionContents), builder.WithPredicates(machineUpdatePredicate)).
		Watches(&keziov1alpha3.MachineClaim{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineClaimToPartitionContents), builder.WithPredicates(claimDemandPredicate)).
		Watches(&keziov1alpha3.DeployRun{}, handler.EnqueueRequestsFromMapFunc(r.mapDeployRunToPartitionContents), builder.WithPredicates(deployRunDemandPredicate)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapSeederDeploymentToPartitionContents)).
		Named("partitioncontent").
		Complete(r)
}
