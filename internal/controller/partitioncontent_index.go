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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageContentRefIndex is the field index name registered on Image for
// slot contentRef names: onDelete's reverse lookup lists against this
// index instead of scanning every Image in the namespace.
const imageContentRefIndex = "spec.layout.slots.contentRef.name"

// indexImageContentRefs extracts every PartitionContent name referenced by
// obj's slots, for imageContentRefIndex.
func indexImageContentRefs(obj client.Object) []string {
	image, ok := obj.(*keziov1alpha2.Image)
	if !ok {
		return nil
	}
	return imageContentRefNames(image)
}

// imageContentRefNames returns the (deduplicated) PartitionContent names
// image's slots reference.
func imageContentRefNames(image *keziov1alpha2.Image) []string {
	seen := make(map[string]bool, len(image.Spec.Layout.Slots))
	names := make([]string, 0, len(image.Spec.Layout.Slots))
	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef == nil || slot.ContentRef.Name == "" || seen[slot.ContentRef.Name] {
			continue
		}
		seen[slot.ContentRef.Name] = true
		names = append(names, slot.ContentRef.Name)
	}
	return names
}

// imageReferencesContent reports whether any of image's slots reference
// the PartitionContent named contentName.
func imageReferencesContent(image *keziov1alpha2.Image, contentName string) bool {
	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef != nil && slot.ContentRef.Name == contentName {
			return true
		}
	}
	return false
}

// mapImageToPartitionContents maps an Image event to a reconcile request
// per PartitionContent its slots reference - the watch that lets a
// PartitionContent's blocked deletion unblock the moment the Image that
// was blocking it is actually removed, without waiting on
// deletionBlockRequeueInterval. On a Delete event this runs against the
// Image's last known state, which is exactly the reference that needs
// re-checking.
func (r *PartitionContentReconciler) mapImageToPartitionContents(_ context.Context, obj client.Object) []reconcile.Request {
	image, ok := obj.(*keziov1alpha2.Image)
	if !ok {
		return nil
	}
	names := imageContentRefNames(image)
	if len(names) == 0 {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(names))
	for _, name := range names {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: image.Namespace, Name: name},
		})
	}
	return requests
}
