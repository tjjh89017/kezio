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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// partitionContentControllerFieldOwner is the Server-Side Apply field
// manager identity for every PartitionContent status write this
// reconciler makes - see machineControllerFieldOwner's doc comment for
// why this is a stable string.
const partitionContentControllerFieldOwner = "kezio-partitioncontent-controller"

// partitionContentStatusApplyBody is the Server-Side Apply request body
// applyPartitionContentStatus sends: identity plus status, deliberately
// with no Spec field - see machineStatusApplyBody's doc comment for why
// (PartitionContentSpec has the same non-omitempty-eligible required
// string fields MachineSpec does).
type partitionContentStatusApplyBody struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        partitionContentStatusApplyBodyMetadata `json:"metadata,omitempty"`
	Status          keziov1alpha2.PartitionContentStatus    `json:"status"`
}

// partitionContentStatusApplyBodyMetadata mirrors machineStatusApplyBodyMetadata.
type partitionContentStatusApplyBodyMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// applyPartitionContentStatus writes pc.Status through Server-Side Apply
// under partitionContentControllerFieldOwner, and - only once the write
// succeeds - runs each of onSuccess in order. See applyMachineStatus's
// doc comment for the same onSuccess/full-status-body discipline this
// mirrors; this reconciler is likewise the only writer of
// PartitionContent.status, so sending the full status on every write is
// safe here for the same reason.
func (r *PartitionContentReconciler) applyPartitionContentStatus(ctx context.Context, pc *keziov1alpha2.PartitionContent, onSuccess ...func()) error {
	body := partitionContentStatusApplyBody{
		TypeMeta: metav1.TypeMeta{APIVersion: keziov1alpha2.GroupVersion.String(), Kind: "PartitionContent"},
		Metadata: partitionContentStatusApplyBodyMetadata{Name: pc.Name, Namespace: pc.Namespace},
		Status:   pc.Status,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("partitioncontent %q: encoding status apply body: %w", pc.Name, err)
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	if err := r.Status().Patch(ctx, pc, patch, client.FieldOwner(partitionContentControllerFieldOwner), client.ForceOwnership); err != nil {
		return err
	}
	for _, cb := range onSuccess {
		cb()
	}
	return nil
}

// setPartitionContentReadyCondition sets the Ready condition on
// pc.Status.Conditions, stamping ObservedGeneration from pc.Generation.
// Valid is set by a later item - so this does not take a condition type
// parameter; setPartitionContentSeederDegradedCondition is the other
// condition setter this reconciler carries.
func setPartitionContentReadyCondition(pc *keziov1alpha2.PartitionContent, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PartitionContentConditionReady,
		Status:             status,
		ObservedGeneration: pc.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setPartitionContentValidCondition sets the Valid condition on
// pc.Status.Conditions, stamping ObservedGeneration from pc.Generation.
// PartitionContentSpec is immutable and CEL-validated at admission (see
// the spec's type-level XValidation rule), so there is no separate
// validation step here: Valid is trivially True on every reconcile. It is
// still written on every reconcile (mirroring setImageValidCondition)
// rather than left unset, so a reader applying the cross-reference
// contract (see aggregateSlotContents) always finds a Valid condition
// with a current observedGeneration to check, instead of having to treat
// "condition absent" as a separate case from "condition stale".
func setPartitionContentValidCondition(pc *keziov1alpha2.PartitionContent) {
	meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PartitionContentConditionValid,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: pc.Generation,
		Reason:             "SpecValid",
		Message:            "spec is CEL-validated at admission",
	})
}

// setPartitionContentSeederDegradedCondition sets the SeederDegraded
// condition on pc.Status.Conditions, stamping ObservedGeneration from
// pc.Generation. See recordSeederStatus for when this is set versus
// removed entirely.
func setPartitionContentSeederDegradedCondition(pc *keziov1alpha2.PartitionContent, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PartitionContentConditionSeederDegraded,
		Status:             status,
		ObservedGeneration: pc.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setPartitionContentDeletionBlockedCondition sets the DeletionBlocked
// condition on pc.Status.Conditions, stamping ObservedGeneration from
// pc.Generation. Set only while a delete is actually blocked - onDelete
// removes it entirely once the finalizer clears (see the type's doc
// comment for why this is never set False).
func setPartitionContentDeletionBlockedCondition(pc *keziov1alpha2.PartitionContent, reason, message string) {
	meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PartitionContentConditionDeletionBlocked,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: pc.Generation,
		Reason:             reason,
		Message:            message,
	})
}
