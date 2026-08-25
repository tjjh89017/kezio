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
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// claimImageRefIndex indexes MachineClaim by every Image it may cause to
// be deployed - spec.imageRef plus spec.dataImages[].imageRef - each entry
// resolved to a "namespace/name" key (an empty NameRef.Namespace defaults
// to the MachineClaim's own namespace). The deploy intent moved from
// Machine to MachineClaim, so demand is now a property of the claim, not
// the machine it may or may not be bound to.
const claimImageRefIndex = "spec.imageRefs"

var (
	claimImageRefIndexOnce sync.Once
	claimImageRefIndexErr  error
)

// ensureClaimImageRefIndex registers claimImageRefIndex on the manager's
// field indexer exactly once, regardless of how many reconcilers'
// SetupWithManager call it or in what order.
func ensureClaimImageRefIndex(mgr ctrl.Manager) error {
	claimImageRefIndexOnce.Do(func() {
		claimImageRefIndexErr = mgr.GetFieldIndexer().IndexField(context.Background(), &keziov1alpha3.MachineClaim{}, claimImageRefIndex, indexClaimImageRefs)
	})
	return claimImageRefIndexErr
}

// indexClaimImageRefs extracts claimImageRefIndex's keys for obj, for the
// field indexer.
func indexClaimImageRefs(obj client.Object) []string {
	claim, ok := obj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil
	}
	refs := claimImageRefs(claim)
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, objectKeyIndexString(ref))
	}
	return keys
}

// objectKeyIndexString renders key as the "namespace/name" string
// claimImageRefIndex is keyed by.
func objectKeyIndexString(key client.ObjectKey) string {
	return key.Namespace + "/" + key.Name
}

// claimImageRefs returns the deduplicated set of Images claim's
// spec.imageRef/spec.dataImages may deploy, namespace-resolved against
// claim's own namespace - mirroring deployRunImageNames' resolution of
// DeployRunSpec's equivalent fields.
func claimImageRefs(claim *keziov1alpha3.MachineClaim) []client.ObjectKey {
	seen := make(map[client.ObjectKey]bool, 1+len(claim.Spec.DataImages))
	keys := make([]client.ObjectKey, 0, 1+len(claim.Spec.DataImages))
	add := func(ref keziov1alpha3.NameRef) {
		namespace := ref.Namespace
		if namespace == "" {
			namespace = claim.Namespace
		}
		key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
		if seen[key] {
			return
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if claim.Spec.ImageRef != nil {
		add(*claim.Spec.ImageRef)
	}
	for _, di := range claim.Spec.DataImages {
		add(di.ImageRef)
	}
	return keys
}

// resolveSeedDemand reports whether pc currently has seed demand: at least
// one MachineClaim, not being deleted, whose spec.imageRef or
// spec.dataImages[].imageRef names an Image (always in pc's own namespace
// - the Image webhook denies a slot contentRef naming any other) whose
// slots reference pc, or at least one DeployRun in a non-terminal phase
// whose resolved snapshot names such an Image
// (activeDeployRunsReferencing, shared with the deletion-blocking
// finalizer walk in onDelete).
//
// A claim that has not bound to a machine yet still counts: it has not
// started deploying, but pre-seeding the content ahead of provisioning -
// so the transfer is already warm once provisioning starts - is the
// entire point of a seeder Deployment existing at all.
func (r *PartitionContentReconciler) resolveSeedDemand(ctx context.Context, pc *keziov1alpha3.PartitionContent) (bool, error) {
	images, err := r.imagesReferencing(ctx, pc)
	if err != nil {
		return false, fmt.Errorf("partitioncontent %q: listing referencing images: %w", pc.Name, err)
	}

	for i := range images {
		live, err := r.hasLiveClaimReferencing(ctx, &images[i])
		if err != nil {
			return false, fmt.Errorf("partitioncontent %q: listing claims referencing image %q: %w", pc.Name, images[i].Name, err)
		}
		if live {
			return true, nil
		}
	}

	runs, err := r.activeDeployRunsReferencing(ctx, pc)
	if err != nil {
		return false, fmt.Errorf("partitioncontent %q: listing referencing deploy runs: %w", pc.Name, err)
	}
	return len(runs) > 0, nil
}

// hasLiveClaimReferencing reports whether any MachineClaim not being
// deleted names image via spec.imageRef or spec.dataImages[].imageRef,
// via claimImageRefIndex.
func (r *PartitionContentReconciler) hasLiveClaimReferencing(ctx context.Context, image *keziov1alpha3.Image) (bool, error) {
	var list keziov1alpha3.MachineClaimList
	key := objectKeyIndexString(client.ObjectKey{Namespace: image.Namespace, Name: image.Name})
	if err := r.List(ctx, &list, client.MatchingFields{claimImageRefIndex: key}); err != nil {
		return false, err
	}
	return anyLiveClaim(list.Items), nil
}

// anyLiveClaim reports whether any claim in claims is not being deleted -
// the pure predicate half of hasLiveClaimReferencing, kept separate so it
// is unit-testable without a live index or client.
func anyLiveClaim(claims []keziov1alpha3.MachineClaim) bool {
	for i := range claims {
		if claims[i].DeletionTimestamp.IsZero() {
			return true
		}
	}
	return false
}

// mapMachineClaimToPartitionContents maps a MachineClaim event to a
// reconcile request per PartitionContent that claim is now (or, for a
// Delete event, was) a demand source for: every content referenced by the
// slots of every Image its spec.imageRef/dataImages name. Reads each
// Image live (a Get, not claimImageRefIndex, which only maps the other
// direction) - cheap since a claim names at most a handful of Images.
func (r *PartitionContentReconciler) mapMachineClaimToPartitionContents(ctx context.Context, obj client.Object) []reconcile.Request {
	claim, ok := obj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil
	}
	return r.imageRefsToPartitionContentRequests(ctx, claimImageRefs(claim))
}

// mapDeployRunToPartitionContents maps a DeployRun event to a reconcile
// request per PartitionContent it is (or, for a Delete event or a
// terminal-phase Update, no longer) a demand source for - the same
// resolved-Image lookup deployRunImageNames already implements for the
// deletion-blocking finalizer walk.
func (r *PartitionContentReconciler) mapDeployRunToPartitionContents(ctx context.Context, obj client.Object) []reconcile.Request {
	run, ok := obj.(*keziov1alpha3.DeployRun)
	if !ok {
		return nil
	}
	return r.imageRefsToPartitionContentRequests(ctx, deployRunImageNames(run))
}

// imageRefsToPartitionContentRequests resolves each Image key in refs
// (skipping one that does not exist) to the PartitionContents its slots
// reference, deduplicated.
func (r *PartitionContentReconciler) imageRefsToPartitionContentRequests(ctx context.Context, refs []client.ObjectKey) []reconcile.Request {
	seen := make(map[client.ObjectKey]bool)
	var requests []reconcile.Request
	for _, ref := range refs {
		var image keziov1alpha3.Image
		if err := r.Get(ctx, ref, &image); err != nil {
			continue
		}
		for _, name := range imageContentRefNames(&image) {
			key := client.ObjectKey{Namespace: image.Namespace, Name: name}
			if seen[key] {
				continue
			}
			seen[key] = true
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}
