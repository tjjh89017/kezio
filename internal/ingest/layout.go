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

package ingest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// LayoutConfigMapDataKey is the ConfigMap data key holding the verbatim
// `sfdisk --json` dump. internal/agentserver's deploy plan builder reads
// this same key by its own literal constant (sfdiskJSONKey) - keep both
// in sync if this value ever changes.
const LayoutConfigMapDataKey = "sfdisk.json"

// LayoutConfigMapName returns the deterministic ConfigMap name for an
// Image's sfdisk layout dump. Shared between the ingest binary, which
// creates/updates this ConfigMap directly through the Kubernetes API on
// success, and the Image controller, which only verifies it exists
// before recording it in status.disk.layoutRef.
func LayoutConfigMapName(imageName string) string {
	return imageName + "-layout"
}

// ImageOwnerRef identifies the Image an ingest run is for, so the layout
// ConfigMap it writes can carry an owner reference back to that Image
// (for cascading garbage collection) without granting the ingest Job's
// ServiceAccount permission to read Images themselves - the Image
// controller passes these fields to the Job as environment variables
// instead (see internal/controller's buildIngestJob).
type ImageOwnerRef struct {
	Name       string
	UID        types.UID
	APIVersion string
	Kind       string
}

// LayoutWriter persists the layout ConfigMap once ingest has captured a
// disk's sfdisk dump. The real implementation talks to the Kubernetes
// API directly (see cmd/ingest); tests wire a fake.
type LayoutWriter interface {
	WriteLayoutConfigMap(ctx context.Context, sfdiskJSON string) error
}

// WriteLayoutConfigMap creates or updates the layout ConfigMap for
// owner's Image in namespace, owner-ref'd to it, holding sfdiskJSON
// under LayoutConfigMapDataKey. This is the ingest Job's own side of
// moving the sfdisk dump off the container termination message (see
// Result's doc comment): the Job writes this ConfigMap itself rather
// than relaying the dump back through the Image controller, since the
// controller's own result channel is the 4KiB-capped termination
// message this exists to stop overflowing.
func WriteLayoutConfigMap(ctx context.Context, client kubernetes.Interface, namespace string, owner ImageOwnerRef, sfdiskJSON string) error {
	name := LayoutConfigMapName(owner.Name)
	cms := client.CoreV1().ConfigMaps(namespace)
	ownerRefs := []metav1.OwnerReference{{
		APIVersion: owner.APIVersion,
		Kind:       owner.Kind,
		Name:       owner.Name,
		UID:        owner.UID,
	}}

	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				OwnerReferences: ownerRefs,
			},
			Data: map[string]string{LayoutConfigMapDataKey: sfdiskJSON},
		}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create layout configmap %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get layout configmap %s: %w", name, err)
	}

	updated := existing.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string]string{}
	}
	updated.Data[LayoutConfigMapDataKey] = sfdiskJSON
	updated.OwnerReferences = ownerRefs
	if _, err := cms.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update layout configmap %s: %w", name, err)
	}
	return nil
}

// clientsetLayoutWriter adapts WriteLayoutConfigMap to the LayoutWriter
// interface, binding a fixed client/namespace/owner for the lifetime of
// one ingest run.
type clientsetLayoutWriter struct {
	client    kubernetes.Interface
	namespace string
	owner     ImageOwnerRef
}

func (w *clientsetLayoutWriter) WriteLayoutConfigMap(ctx context.Context, sfdiskJSON string) error {
	return WriteLayoutConfigMap(ctx, w.client, w.namespace, w.owner, sfdiskJSON)
}

// NewLayoutWriter builds the LayoutWriter a production ingest run wires:
// client is expected to be backed by the Job pod's in-cluster
// ServiceAccount credentials (see cmd/ingest), namespace is the Image's
// namespace, and owner identifies the Image to own the ConfigMap.
func NewLayoutWriter(client kubernetes.Interface, namespace string, owner ImageOwnerRef) LayoutWriter {
	return &clientsetLayoutWriter{client: client, namespace: namespace, owner: owner}
}
