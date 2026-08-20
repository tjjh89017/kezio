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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/store"
)

// partitionContentPVCHeadroomBytes is added on top of spec.sizeBytes when
// sizing a content PVC: spec.sizeBytes is the partition's size at capture,
// but the PVC holds only the used-block extents (bounded by usedBytes,
// always <= sizeBytes) plus the generated .torrent file, so sizing to
// sizeBytes alone already has slack for typical partitions. This headroom
// exists for the pathological case where usedBytes is close to sizeBytes:
// it covers the .torrent file (piece hashes: 20 bytes per PieceSize-sized
// piece, negligible even for very large partitions) and filesystem/inode
// overhead on the PVC's own volume. Sizing from sizeBytes rather than the
// tighter usedBytes is deliberate: usedBytes is a snapshot from capture
// time and is not re-validated here, so sizing against the declared
// partition size is the conservative choice.
// partitionContentPVCHeadroomBytes is also this content PVC's effective
// floor: even a zero (or, defensively, negative) sizeBytes still
// requests this many bytes, well above what any CSI driver requires as
// a minimum viable request, so no separate floor constant is needed.
const partitionContentPVCHeadroomBytes = 32 * 1024 * 1024 // 32Mi

// partitionContentPVCSize returns the storage request for a content PVC
// sized from sizeBytes - see partitionContentPVCHeadroomBytes for the
// headroom rule.
func partitionContentPVCSize(sizeBytes int64) resource.Quantity {
	requested := sizeBytes + partitionContentPVCHeadroomBytes
	if requested < partitionContentPVCHeadroomBytes {
		// sizeBytes was negative (defensive only: the CRD schema floors
		// it at 0) and overflowed the sum below the headroom itself.
		requested = partitionContentPVCHeadroomBytes
	}
	return *resource.NewQuantity(requested, resource.BinarySI)
}

// ensureContentPVC gets or creates the content PVC named
// store.PVCName(hash), owner-referenced to pc so it is reclaimed
// automatically when pc is deleted (see the type-level doc comment on
// PartitionContentReconciler: this item owns the PVC's lifecycle only,
// not a demand-blocking finalizer). An already-existing PVC is returned
// unchanged - the PVC's spec is not reconciled after creation, since
// spec.sizeBytes never changes (PartitionContentSpec is immutable) and a
// PVC's request size and access modes are themselves immutable once
// bound.
func (r *PartitionContentReconciler) ensureContentPVC(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) (*corev1.PersistentVolumeClaim, error) {
	name := store.PVCName(hash)
	key := client.ObjectKey{Namespace: pc.Namespace, Name: name}

	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("partitioncontent %q: getting content PVC %q: %w", pc.Name, name, err)
	}

	pvc := r.buildContentPVC(pc, name)
	if err := controllerutil.SetControllerReference(pc, pvc, r.Scheme); err != nil {
		return nil, fmt.Errorf("partitioncontent %q: setting content PVC %q owner reference: %w", pc.Name, name, err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("partitioncontent %q: creating content PVC %q: %w", pc.Name, name, err)
	}
	return pvc, nil
}

// buildContentPVC constructs the (not yet created) PVC holding pc's
// content bytes.
func (r *PartitionContentReconciler) buildContentPVC(pc *keziov1alpha2.PartitionContent, name string) *corev1.PersistentVolumeClaim {
	size := partitionContentPVCSize(pc.Spec.SizeBytes)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pc.Namespace,
			Labels: map[string]string{
				partitionContentAppNameLabel:      partitionContentAppNameValue,
				partitionContentAppComponentLabel: partitionContentPVCComponentValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: r.Publish.accessModes(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if r.Publish.StorageClassName != "" {
		pvc.Spec.StorageClassName = &r.Publish.StorageClassName
	}
	return pvc
}
