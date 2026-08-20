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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// deletionBlockRequeueInterval is the fallback poll interval while a
// PartitionContent's deletion is blocked by an active DeployRun: a run's
// phase transition is a DeployRun-only write that nothing here watches, so
// this requeue is what eventually notices the run going terminal and
// re-checks. An Image reference unblocks sooner, through
// mapImageToPartitionContents.
var deletionBlockRequeueInterval = 30 * time.Second

// maxBlockingRefsNamed caps how many blocking object names appear in the
// condition/event message, so a content with many referrers still
// produces a bounded, readable message.
const maxBlockingRefsNamed = 5

// onDelete drives PartitionContent's deletion path: PartitionContentFinalizer
// is removed only once no Image slot and no active DeployRun references
// this content any longer. The content PVC and publish Job stay
// owner-referenced GC as noted on Reconcile - this only guards the
// PartitionContent object itself from being removed out from under a
// still-live reference. The seeder Deployment lifecycle (reconcileSeeder)
// is untouched by a deletion timestamp: a seeder still demanded keeps
// running through the grace period exactly as it would without a pending
// delete, since onDelete returns before ever calling onChange/reconcileSeeder.
func (r *PartitionContentReconciler) onDelete(ctx context.Context, pc *keziov1alpha2.PartitionContent) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pc, keziov1alpha2.PartitionContentFinalizer) {
		return ctrl.Result{}, nil
	}

	blockers, err := r.blockingReferences(ctx, pc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(blockers) > 0 {
		return r.recordDeletionBlocked(ctx, pc, blockers)
	}

	controllerutil.RemoveFinalizer(pc, keziov1alpha2.PartitionContentFinalizer)
	if err := r.Update(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: removing finalizer: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// blockingReferences returns "kind/name" for every Image and active
// DeployRun still referencing pc, sorted for a stable message.
func (r *PartitionContentReconciler) blockingReferences(ctx context.Context, pc *keziov1alpha2.PartitionContent) ([]string, error) {
	images, err := r.imagesReferencing(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("partitioncontent %q: listing referencing images: %w", pc.Name, err)
	}
	runs, err := r.activeDeployRunsReferencing(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("partitioncontent %q: listing referencing deploy runs: %w", pc.Name, err)
	}

	blockers := make([]string, 0, len(images)+len(runs))
	for _, img := range images {
		blockers = append(blockers, "image/"+img.Name)
	}
	for _, run := range runs {
		blockers = append(blockers, "deployrun/"+run.Name)
	}
	sort.Strings(blockers)
	return blockers, nil
}

// imagesReferencing lists every Image in pc's namespace whose layout has a
// slot naming pc, via imageContentRefIndex - a List with a field selector
// against that index instead of a full scan of every Image. Scoping to pc's
// namespace is correct, not merely convenient: the Image webhook denies any
// slot contentRef naming a namespace other than the Image's own, so no
// Image outside pc's namespace can ever reference it.
func (r *PartitionContentReconciler) imagesReferencing(ctx context.Context, pc *keziov1alpha2.PartitionContent) ([]keziov1alpha2.Image, error) {
	var list keziov1alpha2.ImageList
	if err := r.List(ctx, &list, client.InNamespace(pc.Namespace), client.MatchingFields{imageContentRefIndex: pc.Name}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// isDeployRunActive reports whether run has not yet reached a terminal
// phase. This is the complement of DeployRunPhaseSucceeded/
// DeployRunPhaseFailed: every other phase value, including the empty
// phase a freshly created run starts in, counts as active - a run only
// stops being able to matter to a content it deploys once it is
// definitively done, one way or the other.
func isDeployRunActive(run *keziov1alpha2.DeployRun) bool {
	switch run.Status.Phase {
	case keziov1alpha2.DeployRunPhaseSucceeded, keziov1alpha2.DeployRunPhaseFailed:
		return false
	default:
		return true
	}
}

// deployRunImageNames returns the namespaced names of every Image run's
// resolved snapshot names: spec.imageRef (absent for a dataImages-only
// run) plus spec.dataImages[].imageRef. DeployRunSpec never names a
// PartitionContent directly - only the Image it resolved into - so this
// is the set activeDeployRunsReferencing resolves transitively against
// each Image's own slots.
func deployRunImageNames(run *keziov1alpha2.DeployRun) []client.ObjectKey {
	keys := make([]client.ObjectKey, 0, 1+len(run.Spec.DataImages))
	addRef := func(ref keziov1alpha2.NameRef) {
		ns := ref.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		keys = append(keys, client.ObjectKey{Namespace: ns, Name: ref.Name})
	}
	if run.Spec.ImageRef != nil {
		addRef(*run.Spec.ImageRef)
	}
	for _, di := range run.Spec.DataImages {
		addRef(di.ImageRef)
	}
	return keys
}

// activeDeployRunsReferencing lists active DeployRuns (see isDeployRunActive)
// in pc's namespace whose resolved snapshot names an Image that still
// references pc via a slot. An Image already referencing pc is caught by
// imagesReferencing above regardless of any DeployRun; this transitive
// check exists only for the edge case where the Image itself has already
// been deleted (spec.imageRef points at a name Get can no longer resolve).
// Once an Image is gone, whether it ever referenced pc cannot be
// recovered - Get returning NotFound is treated as "does not (any longer)
// reference pc", not as a block, since there is nothing left to name in
// the blocking message and the Image's own deletion is the bigger
// unresolved problem, not this content's.
//
// A resolved Image only ever counts as referencing pc (imageReferencesContent
// below) if it truly does: the webhook denies a slot contentRef naming any
// namespace but the Image's own, so a matching Image is always in pc's
// namespace regardless of which namespace the Image lookup key names. This
// List is still scoped to DeployRuns in pc's own namespace, an independent
// assumption about where a run deploying pc's content lives that this
// contentRef invariant does not establish or depend on.
func (r *PartitionContentReconciler) activeDeployRunsReferencing(ctx context.Context, pc *keziov1alpha2.PartitionContent) ([]keziov1alpha2.DeployRun, error) {
	var list keziov1alpha2.DeployRunList
	if err := r.List(ctx, &list, client.InNamespace(pc.Namespace)); err != nil {
		return nil, err
	}

	imageReferencesPC := make(map[client.ObjectKey]bool)
	var matches []keziov1alpha2.DeployRun
	for i := range list.Items {
		run := &list.Items[i]
		if !isDeployRunActive(run) {
			continue
		}

		for _, key := range deployRunImageNames(run) {
			refs, cached := imageReferencesPC[key]
			if !cached {
				var img keziov1alpha2.Image
				err := r.Get(ctx, key, &img)
				switch {
				case apierrors.IsNotFound(err):
					refs = false
				case err != nil:
					return nil, err
				default:
					refs = imageReferencesContent(&img, pc.Name)
				}
				imageReferencesPC[key] = refs
			}
			if refs {
				matches = append(matches, *run)
				break
			}
		}
	}
	return matches, nil
}

// recordDeletionBlocked sets DeletionBlocked=True naming blockers (bounded
// to maxBlockingRefsNamed), emits a matching Event, and requeues so the
// block is re-checked - the fallback for a blocking active DeployRun,
// which nothing here watches directly (see deletionBlockRequeueInterval).
// A blocking Image is re-checked sooner via mapImageToPartitionContents.
func (r *PartitionContentReconciler) recordDeletionBlocked(ctx context.Context, pc *keziov1alpha2.PartitionContent, blockers []string) (ctrl.Result, error) {
	message := fmt.Sprintf("deletion blocked: still referenced by %s", formatBlockers(blockers))
	setPartitionContentDeletionBlockedCondition(pc, "ReferencesRemain", message)
	onSuccess := func() {
		r.Recorder.Event(pc, corev1.EventTypeWarning, "PartitionContentDeletionBlocked", message)
	}
	if err := r.applyPartitionContentStatus(ctx, pc, onSuccess); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording deletion blocked: %w", pc.Name, err)
	}
	return ctrl.Result{RequeueAfter: deletionBlockRequeueInterval}, nil
}

// formatBlockers renders blockers (already sorted) as a comma-separated
// list, naming at most maxBlockingRefsNamed and summarizing the rest as a
// count, so a content with many referrers still produces a bounded,
// readable message.
func formatBlockers(blockers []string) string {
	if len(blockers) <= maxBlockingRefsNamed {
		return strings.Join(blockers, ", ")
	}
	shown := blockers[:maxBlockingRefsNamed]
	return fmt.Sprintf("%s, and %d more", strings.Join(shown, ", "), len(blockers)-maxBlockingRefsNamed)
}
