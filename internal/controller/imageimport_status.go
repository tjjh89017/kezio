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
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// imageImportControllerFieldOwner is the Server-Side Apply field manager
// identity for every ImageImport status write this reconciler makes - see
// partitionContentControllerFieldOwner's doc comment for why this is a
// stable string.
const imageImportControllerFieldOwner = "kezio-imageimport-controller"

// imageImportStatusApplyBody is the Server-Side Apply request body
// applyImageImportStatus sends: identity plus status, deliberately with
// no Spec field - see partitionContentStatusApplyBody's doc comment.
type imageImportStatusApplyBody struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        imageImportStatusApplyBodyMetadata `json:"metadata,omitempty"`
	Status          keziov1alpha3.ImageImportStatus    `json:"status"`
}

// imageImportStatusApplyBodyMetadata mirrors imageStatusApplyBodyMetadata.
type imageImportStatusApplyBodyMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// applyImageImportStatus writes imp.Status through Server-Side Apply
// under imageImportControllerFieldOwner. This reconciler is the only
// writer of ImageImport.status, so sending the full status on every write
// is safe here, mirroring applyImageStatus.
func (r *ImageImportReconciler) applyImageImportStatus(ctx context.Context, imp *keziov1alpha3.ImageImport) error {
	body := imageImportStatusApplyBody{
		TypeMeta: metav1.TypeMeta{APIVersion: keziov1alpha3.GroupVersion.String(), Kind: "ImageImport"},
		Metadata: imageImportStatusApplyBodyMetadata{Name: imp.Name, Namespace: imp.Namespace},
		Status:   imp.Status,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("imageimport %q: encoding status apply body: %w", imp.Name, err)
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	return r.Status().Patch(ctx, imp, patch, client.FieldOwner(imageImportControllerFieldOwner), client.ForceOwnership)
}

// setImageImportReadyCondition sets the Ready condition on
// imp.Status.Conditions, stamping ObservedGeneration from imp.Generation.
func setImageImportReadyCondition(imp *keziov1alpha3.ImageImport, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&imp.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.ImageImportConditionReady,
		Status:             status,
		ObservedGeneration: imp.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// recordImportPending records Pending: the manager is missing
// configuration the import needs. This is a visible, non-error hold, not
// a failure - it clears on its own once the manager is configured,
// mirroring PartitionContentReconciler.recordPending.
func (r *ImageImportReconciler) recordImportPending(ctx context.Context, imp *keziov1alpha3.ImageImport, reason, message string) (ctrl.Result, error) {
	imp.Status.State = keziov1alpha3.ImageImportStatePending
	setImageImportReadyCondition(imp, metav1.ConditionFalse, reason, message)
	if err := r.applyImageImportStatus(ctx, imp); err != nil {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: recording Pending: %w", imp.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordIngesting records Ingesting: the ingest Job exists and has not
// yet reported success or failure (including the moment it was just
// created).
func (r *ImageImportReconciler) recordIngesting(ctx context.Context, imp *keziov1alpha3.ImageImport, reason, message string) (ctrl.Result, error) {
	imp.Status.State = keziov1alpha3.ImageImportStateIngesting
	setImageImportReadyCondition(imp, metav1.ConditionFalse, reason, message)
	if err := r.applyImageImportStatus(ctx, imp); err != nil {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: recording Ingesting: %w", imp.Name, err)
	}
	return ctrl.Result{RequeueAfter: imageIngestPollInterval}, nil
}

// recordImportSucceeded records Succeeded: every PartitionContent this
// import captured, and the Image binding them, now exist. Their own
// controllers take it from here - this import has nothing left to do but
// the completionTime/TTL bookkeeping enforceTTL owns.
func (r *ImageImportReconciler) recordImportSucceeded(ctx context.Context, imp *keziov1alpha3.ImageImport, contentRefs []keziov1alpha3.NameRef) (ctrl.Result, error) {
	imp.Status.State = keziov1alpha3.ImageImportStateSucceeded
	imp.Status.ImageRef = &keziov1alpha3.NameRef{Name: imp.Spec.ImageName}
	imp.Status.ContentRefs = contentRefs
	stampCompletionTime(imp)
	setImageImportReadyCondition(imp, metav1.ConditionTrue, "ImportComplete",
		fmt.Sprintf("captured %d partition content object(s) and created Image %q", len(contentRefs), imp.Spec.ImageName))
	if err := r.applyImageImportStatus(ctx, imp); err != nil {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: recording Succeeded: %w", imp.Name, err)
	}
	return r.enforceImportTTL(ctx, imp)
}

// recordImportFailed records Failed: the ingest Job failed terminally,
// succeeded but its result could not be trusted, or a name this import
// had to create was already taken. See reconcileIngesting's doc comment
// for retry semantics.
func (r *ImageImportReconciler) recordImportFailed(ctx context.Context, imp *keziov1alpha3.ImageImport, message string) (ctrl.Result, error) {
	imp.Status.State = keziov1alpha3.ImageImportStateFailed
	stampCompletionTime(imp)
	setImageImportReadyCondition(imp, metav1.ConditionFalse, "ImportFailed", message)
	if err := r.applyImageImportStatus(ctx, imp); err != nil {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: recording Failed: %w", imp.Name, err)
	}
	return r.enforceImportTTL(ctx, imp)
}

// stampCompletionTime sets imp.Status.CompletionTime to now, if it is not
// already set - an import only ever enters Succeeded or Failed once, but
// this guard also lets the same call sit safely in the legacy Ready
// rewrite and the terminal-state backfill path, both of which may find it
// already set.
func stampCompletionTime(imp *keziov1alpha3.ImageImport) {
	if imp.Status.CompletionTime == nil {
		now := metav1.Now()
		imp.Status.CompletionTime = &now
	}
}

// migrateLegacyReadyState rewrites a pre-rename Ready import to Succeeded,
// once, without touching ingest - see legacyReadyState's doc comment. The
// content refs and Image ref it already recorded are left as they are;
// only status.state (and status.completionTime, if this import predates
// that field too) change.
func (r *ImageImportReconciler) migrateLegacyReadyState(ctx context.Context, imp *keziov1alpha3.ImageImport) (ctrl.Result, error) {
	imp.Status.State = keziov1alpha3.ImageImportStateSucceeded
	stampCompletionTime(imp)
	if err := r.applyImageImportStatus(ctx, imp); err != nil {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: rewriting legacy Ready state to Succeeded: %w", imp.Name, err)
	}
	return r.enforceImportTTL(ctx, imp)
}

// reconcileFinished keeps a Succeeded or Failed import's
// status.completionTime backfilled (for one recorded before that field
// existed) and applies spec.ttlSecondsAfterFinished. It never re-runs
// ingest - see onChange's doc comment.
func (r *ImageImportReconciler) reconcileFinished(ctx context.Context, imp *keziov1alpha3.ImageImport) (ctrl.Result, error) {
	if imp.Status.CompletionTime == nil {
		stampCompletionTime(imp)
		if err := r.applyImageImportStatus(ctx, imp); err != nil {
			return ctrl.Result{}, fmt.Errorf("imageimport %q: backfilling completionTime: %w", imp.Name, err)
		}
	}
	return r.enforceImportTTL(ctx, imp)
}

// enforceImportTTL deletes imp once spec.ttlSecondsAfterFinished has
// elapsed since status.completionTime, or requeues for the remaining
// duration. An unset TTL keeps the import forever. Deleting the
// ImageImport never deletes the Image or PartitionContent objects it
// created - neither carries an owner reference to it, only an annotation
// (see imageImportAnnotation) or its own Source.ImportName, precisely so
// that this delete cannot take them with it.
func (r *ImageImportReconciler) enforceImportTTL(ctx context.Context, imp *keziov1alpha3.ImageImport) (ctrl.Result, error) {
	if imp.Spec.TTLSecondsAfterFinished == nil {
		return ctrl.Result{}, nil
	}
	ttl := time.Duration(*imp.Spec.TTLSecondsAfterFinished) * time.Second
	remaining := ttl - time.Since(imp.Status.CompletionTime.Time)
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	if err := r.Delete(ctx, imp); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("imageimport %q: deleting after ttlSecondsAfterFinished: %w", imp.Name, err)
	}
	return ctrl.Result{}, nil
}
