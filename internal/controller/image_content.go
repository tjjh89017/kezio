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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageMissingContentMessageLimit bounds how many slot problems a
// condition message names, so a large layout cannot blow up the
// condition's message size.
const imageMissingContentMessageLimit = 5

// slotContentAggregation is the result of resolving every contentRef slot
// in an Image's layout against the PartitionContent objects it names.
type slotContentAggregation struct {
	// stale is true when any referenced content's Ready condition
	// observedGeneration does not match that content's current
	// generation - see aggregateSlotContents' doc comment for the
	// cross-reference contract this implements. When stale, the other
	// fields are not meaningful: the caller must requeue without acting.
	stale bool
	// failed lists slot/content descriptions whose PartitionContent is
	// terminally Failed (fresh status).
	failed []string
	// notReady lists slot/content descriptions that are missing or not
	// yet Ready (fresh status).
	notReady []string
	// invalidSizes lists slot/content descriptions whose declared
	// sizeBytes is smaller than the content's lastExtentEnd.
	invalidSizes []string
}

// aggregateSlotContents resolves every slot with a contentRef against its
// PartitionContent, implementing the reader-side half of the
// cross-reference contract used across kezio's controllers: a referenced
// kind (here, PartitionContent) exposes Ready/Valid conditions carrying
// observedGeneration, and a reader that sees
// condition.observedGeneration != the referenced object's current
// generation treats that condition as stale and must retry rather than
// act on it, because the referenced object's own controller has not yet
// reconciled its latest generation and the condition describes an
// obsolete observation.
//
// A blank slot (no contentRef) and a swap/uuid slot need no content and
// are skipped entirely.
func (r *ImageReconciler) aggregateSlotContents(ctx context.Context, image *keziov1alpha2.Image) (slotContentAggregation, error) {
	var agg slotContentAggregation
	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef == nil {
			continue
		}

		// The Image webhook denies a slot contentRef naming any namespace
		// but the Image's own, so namespace always resolves to
		// image.Namespace here; this still reads slot.ContentRef.Namespace
		// rather than assuming it, so an Image admitted before that rule
		// existed (spec is immutable, so it never gets a chance to be
		// re-validated) keeps resolving exactly as it always has.
		namespace := slot.ContentRef.Namespace
		if namespace == "" {
			namespace = image.Namespace
		}

		var content keziov1alpha2.PartitionContent
		key := client.ObjectKey{Namespace: namespace, Name: slot.ContentRef.Name}
		if err := r.Get(ctx, key, &content); err != nil {
			if apierrors.IsNotFound(err) {
				agg.notReady = append(agg.notReady, fmt.Sprintf("slot %d: PartitionContent %q not found", slot.Number, slot.ContentRef.Name))
				continue
			}
			return slotContentAggregation{}, fmt.Errorf("image %q: getting PartitionContent %q for slot %d: %w", image.Name, slot.ContentRef.Name, slot.Number, err)
		}

		// Valid is cheap to re-derive here since the content is fetched
		// anyway (see ImageConditionValid's doc comment): the webhook
		// already denies this at admission for an already-existing
		// content, but an Image admitted before its content existed
		// needs this re-checked at read time.
		if slot.SizeBytes != 0 && slot.SizeBytes < content.Spec.LastExtentEnd {
			agg.invalidSizes = append(agg.invalidSizes, fmt.Sprintf(
				"slot %d: sizeBytes (%d) is smaller than PartitionContent %q lastExtentEnd (%d)",
				slot.Number, slot.SizeBytes, slot.ContentRef.Name, content.Spec.LastExtentEnd))
		}

		cond := meta.FindStatusCondition(content.Status.Conditions, keziov1alpha2.PartitionContentConditionReady)
		if cond == nil {
			agg.notReady = append(agg.notReady, fmt.Sprintf("slot %d: PartitionContent %q has no Ready condition yet", slot.Number, slot.ContentRef.Name))
			continue
		}
		if conditionObservedGenerationStale(cond, content.Generation) {
			agg.stale = true
			continue
		}
		if content.Status.State == keziov1alpha2.PartitionContentStateFailed {
			agg.failed = append(agg.failed, fmt.Sprintf("slot %d: PartitionContent %q failed", slot.Number, slot.ContentRef.Name))
			continue
		}
		if cond.Status != metav1.ConditionTrue {
			agg.notReady = append(agg.notReady, fmt.Sprintf("slot %d: PartitionContent %q not Ready (%s)", slot.Number, slot.ContentRef.Name, cond.Reason))
		}
	}
	return agg, nil
}

// boundedMessage joins items with "; ", truncating to
// imageMissingContentMessageLimit entries and naming how many were left
// out - see imageMissingContentMessageLimit's doc comment.
func boundedMessage(items []string) string {
	if len(items) <= imageMissingContentMessageLimit {
		return strings.Join(items, "; ")
	}
	shown := items[:imageMissingContentMessageLimit]
	return fmt.Sprintf("%s (+%d more)", strings.Join(shown, "; "), len(items)-imageMissingContentMessageLimit)
}
