/*
Copyright 2026 Date Huang.

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// partitionPVCNamePrefix identifies a PVC as one partition's content
// volume, at a glance in `kubectl get pvc`.
const partitionPVCNamePrefix = "kezio-part-"

// AppNameLabel and AppNameValue are the app.kubernetes.io/name label
// key and its fixed value on every kezio-managed workload this package
// builds, shared for the same reason AppComponentLabel is.
const (
	AppNameLabel = "app.kubernetes.io/name"
	AppNameValue = "kezio"
)

// AppComponentLabel is the app.kubernetes.io/component label key,
// shared across every kezio-managed workload this package builds
// (ingest, publish, and per-Image seeder Deployment pods, and partition
// PVCs) so it is declared once rather than repeated as a string
// literal. Exported so internal/agentserver can select the same seeder
// pods by the same labels when resolving a content partition's seeder
// URL (see agentserver's buildPlanPartition).
const AppComponentLabel = "app.kubernetes.io/component"

// SeederComponentValue is AppComponentLabel's value on a per-Image
// seeder Deployment's pods (see buildSeederDeployment). Exported for the
// same reason AppComponentLabel is.
const SeederComponentValue = "ezio-seeder"

// dropAllCapabilities is the "ALL" Linux capability drop entry every
// restricted-PodSecurity container this package builds sets.
const dropAllCapabilities = "ALL"

// maxPartitionPVCNameLength is the Kubernetes PVC name limit (a
// DNS-1123 subdomain, 253 characters) - far more headroom than a Job or
// Deployment name gets, but this still truncates-and-hashes for the
// same reason ingestJobName does: an Image name near the limit plus this
// prefix and a partition suffix could otherwise overflow it.
const maxPartitionPVCNameLength = 253

// minPartitionStorageSize floors a partition PVC's requested capacity: a
// partition with an unusually small UsedBytes (or, defensively, zero)
// still gets a viable request rather than one some CSI drivers reject
// outright.
const minPartitionStorageSize = "1Mi"

// partitionPVCName returns the deterministic PVC name for one partition
// of imageName's content: keyed on (Image name, partition number)
// rather than InfoHash, because a partition PVC is created before its
// content's info hash is known (see reconcilePublishing) - ingest
// reports InfoHash only once content has actually been hashed, but the
// PVC needs to exist first for the publish Job to write into. This is
// stable across an Image's whole lifetime (a partition's number never
// changes; the Image spec is immutable - see ImageSpec's doc comment),
// so it also uniquely identifies "this partition's content, whichever
// content that currently is" - exactly the identity the content and
// reclamation layer needs (see the per-Image seeder Deployment, which
// looks the same name up again to build a read-only mount).
func partitionPVCName(imageName string, number int32) string {
	name := fmt.Sprintf("%s%s-%d", partitionPVCNamePrefix, imageName, number)
	if len(name) <= maxPartitionPVCNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(imageName))
	suffix := fmt.Sprintf("-%s-%d", hex.EncodeToString(sum[:])[:8], number)
	maxBaseLen := maxPartitionPVCNameLength - len(partitionPVCNamePrefix) - len(suffix)
	return partitionPVCNamePrefix + imageName[:maxBaseLen] + suffix
}

// partitionMountPath returns the deterministic in-pod mount path for one
// partition's content volume, shared by the publish Job (which writes
// there - see buildPublishJob) and the per-Image seeder Deployment
// (which reads there, read-only - see buildSeederDeployment). Delegates
// to ingest.PartitionMountPath, the single source of truth cmd/ingest's
// publish mode also uses, so the convention cannot drift between the
// writer and the readers.
func partitionMountPath(number int32) string {
	return ingest.PartitionMountPath(number)
}

// ensurePartitionPVCs creates whichever of image's content partitions
// (InfoHash != "") do not already have their own PVC, sized from the
// partition's UsedBytes (the closest thing to a known footprint ingest
// has - see IngestConfig's doc comment on why this is harder to size
// exactly than a shared pool). It returns every content partition's PVC
// name, keyed by partition number, whether just created or already
// existing - the map reconcilePublishing and buildSeederDeployment both
// need to build their respective volume mounts.
func (r *ImageReconciler) ensurePartitionPVCs(ctx context.Context, image *keziov1alpha1.Image) (map[int32]string, error) {
	names := make(map[int32]string)
	for _, p := range image.Status.Partitions {
		if p.InfoHash == "" {
			continue
		}
		name := partitionPVCName(image.Name, p.Number)
		names[p.Number] = name

		key := types.NamespacedName{Name: name, Namespace: image.Namespace}
		existing := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, key, existing); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get partition pvc %s: %w", name, err)
		}

		pvc, err := r.buildPartitionPVC(image, p)
		if err != nil {
			return nil, err
		}
		if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create partition pvc %s: %w", name, err)
		}
	}
	return names, nil
}

// buildPartitionPVC constructs the (not yet created) PVC holding one
// partition's content, owner-ref'd to image so deleting the Image
// cascades to every partition it composed - the reclamation half of the
// content-and-reclamation layer this type belongs to (see
// partitionPVCName's doc comment): ordinary Kubernetes garbage
// collection, not a finalizer, is what actually frees the capacity.
func (r *ImageReconciler) buildPartitionPVC(image *keziov1alpha1.Image, p keziov1alpha1.ImagePartitionStatus) (*corev1.PersistentVolumeClaim, error) {
	size := resource.NewQuantity(p.UsedBytes, resource.BinarySI)
	floor := resource.MustParse(minPartitionStorageSize)
	if size.Cmp(floor) < 0 {
		size = &floor
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      partitionPVCName(image.Name, p.Number),
			Namespace: image.Namespace,
			Labels: map[string]string{
				AppNameLabel:      AppNameValue,
				AppComponentLabel: "partition-content",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: r.Ingest.partitionAccessModes(),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *size}},
		},
	}
	if r.Ingest.PartitionStorageClassName != "" {
		pvc.Spec.StorageClassName = &r.Ingest.PartitionStorageClassName
	}
	if err := ctrl.SetControllerReference(image, pvc, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on partition pvc: %w", err)
	}
	return pvc, nil
}
