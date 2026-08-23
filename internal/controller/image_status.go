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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageControllerFieldOwner is the Server-Side Apply field manager
// identity for every Image status write this reconciler makes - see
// partitionContentControllerFieldOwner's doc comment for why this is a
// stable string.
const imageControllerFieldOwner = "kezio-image-controller"

// imageStatusApplyBody is the Server-Side Apply request body
// applyImageStatus sends: identity plus status, deliberately with no Spec
// field - see partitionContentStatusApplyBody's doc comment for why.
type imageStatusApplyBody struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        imageStatusApplyBodyMetadata `json:"metadata,omitempty"`
	Status          keziov1alpha2.ImageStatus    `json:"status"`
}

// imageStatusApplyBodyMetadata mirrors partitionContentStatusApplyBodyMetadata.
type imageStatusApplyBodyMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// applyImageStatus writes image.Status through Server-Side Apply under
// imageControllerFieldOwner. This reconciler is the only writer of
// Image.status, so sending the full status on every write is safe here,
// mirroring applyPartitionContentStatus (this reconciler emits no
// Events, so it has no onSuccess callbacks to run after the write
// succeeds).
func (r *ImageReconciler) applyImageStatus(ctx context.Context, image *keziov1alpha2.Image) error {
	body := imageStatusApplyBody{
		TypeMeta: metav1.TypeMeta{APIVersion: keziov1alpha2.GroupVersion.String(), Kind: "Image"},
		Metadata: imageStatusApplyBodyMetadata{Name: image.Name, Namespace: image.Namespace},
		Status:   image.Status,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("image %q: encoding status apply body: %w", image.Name, err)
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	return r.Status().Patch(ctx, image, patch, client.FieldOwner(imageControllerFieldOwner), client.ForceOwnership)
}

// setImageReadyCondition sets the Ready condition on image.Status.Conditions,
// stamping ObservedGeneration from image.Generation - the write-side half
// of the cross-reference contract aggregateSlotContents documents: any
// other reconciler reading this condition must compare
// ObservedGeneration against this Image's current generation before
// acting on it.
func setImageReadyCondition(image *keziov1alpha2.Image, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.ImageConditionReady,
		Status:             status,
		ObservedGeneration: image.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setImageValidCondition sets the Valid condition on image.Status.Conditions,
// stamping ObservedGeneration from image.Generation. Ready must never be
// set True while Valid is False (see recordReady's caller).
func setImageValidCondition(image *keziov1alpha2.Image, valid bool, problems []string) {
	status := metav1.ConditionTrue
	reason := "SlotSizesOK"
	message := "every contentRef slot's declared sizeBytes fits its content's lastExtentEnd"
	if !valid {
		status = metav1.ConditionFalse
		reason = "SlotSizeTooSmall"
		message = boundedMessage(problems)
	}
	meta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.ImageConditionValid,
		Status:             status,
		ObservedGeneration: image.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// recordReady records Ready: every referenced content is Ready (fresh)
// and every contentRef slot's declared size fits its content.
func (r *ImageReconciler) recordReady(ctx context.Context, image *keziov1alpha2.Image) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStateReady
	setImageReadyCondition(image, metav1.ConditionTrue, "ContentReady", "every referenced PartitionContent is Ready")
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Ready: %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordPending records Pending for a composed Image (no spec.source)
// whose referenced content is missing or not yet Ready.
func (r *ImageReconciler) recordPending(ctx context.Context, image *keziov1alpha2.Image, notReady []string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStatePending
	setImageReadyCondition(image, metav1.ConditionFalse, "ContentNotReady", boundedMessage(notReady))
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Pending: %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordInvalid records Pending for an Image whose referenced content is
// all present and Ready but fails the slot-size/lastExtentEnd check:
// Ready must not become True while Valid is False.
func (r *ImageReconciler) recordInvalid(ctx context.Context, image *keziov1alpha2.Image, invalidSizes []string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStatePending
	setImageReadyCondition(image, metav1.ConditionFalse, "Invalid", boundedMessage(invalidSizes))
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Pending (invalid): %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordFailed records Failed: at least one referenced content is itself
// terminally Failed (fresh status). There is no automatic retry - an
// operator addressing the failed PartitionContent is what re-enters the
// walk, mirroring PartitionContentReconciler.recordFailed.
func (r *ImageReconciler) recordFailed(ctx context.Context, image *keziov1alpha2.Image, failed []string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStateFailed
	setImageReadyCondition(image, metav1.ConditionFalse, "ContentFailed", boundedMessage(failed))
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Failed: %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}

// imageSeederDegradedCause is one reason updateImageSeederCondition can
// report: a Reason plus the ready-to-render message for the site(s) it
// applies to.
type imageSeederDegradedCause struct {
	reason  string
	message string
}

// updateImageSeederCondition aggregates reconcileImageSeeder's per-Site
// findings - unsetSeederSubnetSites (from imageSeedDemandBySite), and
// problems (foreign-owner, unready) collected while reconciling each
// Site's Deployment - into ImageConditionSeederDegraded, then writes
// image.Status only when the condition actually changed. Each cause gets
// its own Reason when it is the only one present, so a human goes
// straight to the specific remediation; multiple causes share Reason
// "SeederDegraded" instead, since the condition has room for only one.
// The condition is removed entirely (never set False) when nothing is
// wrong - "degraded" does not apply to a healthy seeder plane.
func (r *ImageReconciler) updateImageSeederCondition(ctx context.Context, image *keziov1alpha2.Image, problems []seederSiteProblem, unsetSeederSubnetSites []string) error {
	var foreignOwner, unready []string
	for _, p := range problems {
		switch p.kind {
		case seederProblemForeignOwner:
			foreignOwner = append(foreignOwner, p.site)
		case seederProblemUnready:
			unready = append(unready, p.site)
		}
	}

	var causes []imageSeederDegradedCause
	if len(unsetSeederSubnetSites) > 0 {
		sites := append([]string(nil), unsetSeederSubnetSites...)
		sort.Strings(sites)
		causes = append(causes, imageSeederDegradedCause{
			reason: "SeederSubnetRefUnset",
			message: fmt.Sprintf("Site.spec.seederSubnetRef is unset for site(s) %s; no seeder Deployment is created there, so machines there cannot build a deploy plan for this Image until it is set",
				strings.Join(sites, ", ")),
		})
	}
	if len(foreignOwner) > 0 {
		sort.Strings(foreignOwner)
		causes = append(causes, imageSeederDegradedCause{
			reason: "SeederDeploymentForeignOwner",
			message: fmt.Sprintf("seeder deployment name is already controlled by a different object for site(s) %s; left untouched and not counted as served",
				strings.Join(foreignOwner, ", ")),
		})
	}
	if len(unready) > 0 {
		sort.Strings(unready)
		causes = append(causes, imageSeederDegradedCause{
			reason: "SeederPodUnready",
			message: fmt.Sprintf("seeder deployment has reported no available replica for site(s) %s; check its pod status (crash loop, bad image, or an unsatisfiable nodeSelector)",
				strings.Join(unready, ", ")),
		})
	}

	var changed bool
	switch len(causes) {
	case 0:
		changed = meta.RemoveStatusCondition(&image.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)
	case 1:
		changed = meta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha2.ImageConditionSeederDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             causes[0].reason,
			Message:            causes[0].message,
			ObservedGeneration: image.Generation,
		})
	default:
		messages := make([]string, len(causes))
		for i, c := range causes {
			messages[i] = c.message
		}
		changed = meta.SetStatusCondition(&image.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha2.ImageConditionSeederDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             "SeederDegraded",
			Message:            strings.Join(messages, "; "),
			ObservedGeneration: image.Generation,
		})
	}

	if !changed {
		return nil
	}
	if err := r.applyImageStatus(ctx, image); err != nil {
		return fmt.Errorf("image %q: recording seeder degraded condition: %w", image.Name, err)
	}
	return nil
}

// reconcileIngestPending holds an Image that has spec.source and whose
// referenced content is not all present/Ready, at Pending with
// IngestUnconfigured when no ingest Job image is configured on the
// manager. Once r.Ingest is ready(), it hands off to reconcileIngesting
// (image_ingest.go) instead, which creates or observes the ingest Job.
func (r *ImageReconciler) reconcileIngestPending(ctx context.Context, image *keziov1alpha2.Image, notReady []string) (ctrl.Result, error) {
	if !r.Ingest.ready() {
		image.Status.State = keziov1alpha2.ImageStatePending
		setImageReadyCondition(image, metav1.ConditionFalse, "IngestUnconfigured",
			"spec.source is set but no ingest Job image is configured on the manager; content stays Pending until it is")
		if err := r.applyImageStatus(ctx, image); err != nil {
			return ctrl.Result{}, fmt.Errorf("image %q: recording IngestUnconfigured: %w", image.Name, err)
		}
		return ctrl.Result{}, nil
	}
	return r.reconcileIngesting(ctx, image, notReady)
}
