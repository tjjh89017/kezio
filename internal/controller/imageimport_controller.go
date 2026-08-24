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
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

// imageImportAnnotation marks an Image as created by the ImageImport it
// names. The Image is deliberately not owner-referenced to that import -
// an import is a one-shot request and deleting it must not take the Image
// (or the swarms behind it) with it - so this annotation is what lets a
// repeated reconcile tell "the Image I already created" apart from "a
// different object is squatting on that name".
const imageImportAnnotation = "kezio.kojuro.date/image-import"

// ImageImportReconciler reconciles an ImageImport object.
//
// It owns one ingest Job and its scratch PVC, and turns that Job's result
// into the objects the import was asked for: one PartitionContent per
// non-swap partition, named store.ContentName(spec.contentPrefix, N), and
// one Image binding them under spec.imageName. Both are created only
// once, at the end, with everything the ingest run discovered already
// known - which is what lets ImageSpec stay immutable and complete from
// creation.
//
// Neither the created PartitionContent objects nor the created Image are
// owner-referenced to the import: content outlives the import that
// captured it (PartitionContentFinalizer governs its lifetime instead),
// and an Image is a durable declaration, not a byproduct. The scratch PVC
// is owner-referenced, so deleting an ImageImport before every content it
// captured has published discards the bytes those publish Jobs still need
// to read.
type ImageImportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Ingest configures the ingest Job. The zero value holds every import
	// at Pending with a condition explaining why - see ImageIngestConfig's
	// doc comment.
	Ingest ImageIngestConfig
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=imageimports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=imageimports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=imageimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=images,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=partitioncontents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ImageImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var imp keziov1alpha2.ImageImport
	if err := r.Get(ctx, req.NamespacedName, &imp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return r.onChange(ctx, &imp)
}

// onChange drives one step of the import walk: Pending -> Ingesting ->
// Ready|Failed. A finished import never re-enters it - the Image and the
// content it created are immutable, so there is nothing left to converge.
func (r *ImageImportReconciler) onChange(ctx context.Context, imp *keziov1alpha2.ImageImport) (ctrl.Result, error) {
	switch imp.Status.State {
	case keziov1alpha2.ImageImportStateReady, keziov1alpha2.ImageImportStateFailed:
		return ctrl.Result{}, nil
	}

	if !r.Ingest.ready() {
		return r.recordImportPending(ctx, imp, "IngestUnconfigured",
			"no ingest Job image is configured on the manager; the import stays Pending until it is")
	}
	return r.reconcileIngesting(ctx, imp)
}

// completeIngest handles a successfully completed ingest Job: it reads
// back the per-partition Result, creates one PartitionContent per
// non-swap partition under the import's own content prefix, then creates
// the Image whose layout binds them.
//
// Content is created before the Image, and the Image is created in one
// shot with the whole layout it will ever have. Nothing here patches an
// existing object: an Image or content name already taken by something
// this import did not create fails the import (see ensureImportedContent
// and ensureImportedImage), because both kinds promise immutability and
// writing over a name would break that promise for whoever already
// references it.
func (r *ImageImportReconciler) completeIngest(ctx context.Context, imp *keziov1alpha2.ImageImport, job *batchv1.Job) (ctrl.Result, error) {
	result, err := readJobResult(ctx, r.Client, imp.Namespace, job)
	if err != nil {
		return r.recordImportFailed(ctx, imp, fmt.Sprintf("reading ingest result: %s", err))
	}
	if !result.Success {
		return r.recordImportFailed(ctx, imp, "ingest job reported failure: "+result.Error)
	}
	if result.Disk == nil {
		return r.recordImportFailed(ctx, imp, "ingest job reported success with no disk result")
	}

	layout, err := importLayout(imp, result.Disk)
	if err != nil {
		return r.recordImportFailed(ctx, imp, err.Error())
	}

	contentRefs := make([]keziov1alpha2.NameRef, 0, len(result.Disk.Partitions))
	for _, part := range result.Disk.Partitions {
		if part.Role == keziov1alpha2.PartitionRoleSwap {
			continue
		}
		name := store.ContentName(imp.Spec.ContentPrefix, part.Number)
		if err := r.ensureImportedContent(ctx, imp, name, part); err != nil {
			var conflict *importNameConflictError
			if errors.As(err, &conflict) {
				return r.recordImportFailed(ctx, imp, conflict.Error())
			}
			return ctrl.Result{}, err
		}
		contentRefs = append(contentRefs, keziov1alpha2.NameRef{Name: name})
	}

	if err := r.ensureImportedImage(ctx, imp, layout); err != nil {
		var conflict *importNameConflictError
		if errors.As(err, &conflict) {
			return r.recordImportFailed(ctx, imp, conflict.Error())
		}
		return ctrl.Result{}, err
	}

	return r.recordImportReady(ctx, imp, contentRefs)
}

// importNameConflictError reports a name this import had to create but
// that something it did not create already holds. It is a terminal
// failure of the import, not a transient error: content and Image are
// both immutable, so there is no version of "try again" that could
// succeed without an operator picking different names.
type importNameConflictError struct {
	kind string
	name string
}

func (e *importNameConflictError) Error() string {
	return fmt.Sprintf("%s %q already exists and was not created by this import: %s is immutable, so the import will not write over it; re-run the import with a different name",
		e.kind, e.name, e.kind)
}

// ensureImportedContent creates the PartitionContent named name from
// part, unless this same import already created it (a reconcile that
// resumed after creating part of the set). An existing object from any
// other origin is a conflict.
func (r *ImageImportReconciler) ensureImportedContent(ctx context.Context, imp *keziov1alpha2.ImageImport, name string, part ingest.ResultPartition) error {
	existing := &keziov1alpha2.PartitionContent{}
	key := client.ObjectKey{Namespace: imp.Namespace, Name: name}
	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		if existing.Spec.Source.ImportName == imp.Name {
			return nil
		}
		return &importNameConflictError{kind: "PartitionContent", name: name}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("imageimport %q: getting PartitionContent %q: %w", imp.Name, name, err)
	}

	pc := &keziov1alpha2.PartitionContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: imp.Namespace},
		Spec: keziov1alpha2.PartitionContentSpec{
			FSType:        part.FSType,
			UsedBytes:     part.UsedBytes,
			SizeBytes:     part.SizeBytes,
			LastExtentEnd: part.LastExtentEnd,
			PieceLength:   part.PieceLength,
			Source: keziov1alpha2.PartitionContentSource{
				ImportName:      imp.Name,
				PartitionNumber: part.Number,
			},
		},
	}
	if err := r.Create(ctx, pc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("imageimport %q: creating PartitionContent %q: %w", imp.Name, name, err)
	}
	return nil
}

// ensureImportedImage creates the Image named imp.Spec.ImageName with
// layout, unless this same import already created it (see
// imageImportAnnotation).
func (r *ImageImportReconciler) ensureImportedImage(ctx context.Context, imp *keziov1alpha2.ImageImport, layout keziov1alpha2.ImageDiskLayout) error {
	name := imp.Spec.ImageName
	existing := &keziov1alpha2.Image{}
	key := client.ObjectKey{Namespace: imp.Namespace, Name: name}
	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		if existing.Annotations[imageImportAnnotation] == imp.Name {
			return nil
		}
		return &importNameConflictError{kind: "Image", name: name}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("imageimport %q: getting Image %q: %w", imp.Name, name, err)
	}

	image := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   imp.Namespace,
			Annotations: map[string]string{imageImportAnnotation: imp.Name},
		},
		Spec: keziov1alpha2.ImageSpec{
			OSFamily:     imp.Spec.OSFamily,
			Bootable:     imp.Spec.Bootable,
			Layout:       layout,
			Params:       imp.Spec.Params,
			PostHookRefs: imp.Spec.PostHookRefs,
		},
	}
	if err := r.Create(ctx, image); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("imageimport %q: creating Image %q: %w", imp.Name, name, err)
	}
	return nil
}

// imageImportUpdatePredicate restricts the ImageImport watch's Update
// events to a generation or annotation change - ImageImportSpec is
// immutable, so a status-only self-write must not re-trigger the
// reconciler, mirroring imageUpdatePredicate.
var imageImportUpdatePredicate = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
)

// SetupWithManager sets up the controller with the Manager.
func (r *ImageImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.ImageImport{}, builder.WithPredicates(imageImportUpdatePredicate)).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("imageimport").
		Complete(r)
}
