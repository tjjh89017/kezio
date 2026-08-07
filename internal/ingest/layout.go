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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// ImageLayoutName returns the deterministic ImageLayout name for an
// Image's sfdisk layout dump. Shared between the ingest binary, which
// creates/updates this ImageLayout directly through the Kubernetes API
// on success, and the Image controller, which only verifies it exists
// before recording it in status.disk.layoutRef.
func ImageLayoutName(imageName string) string {
	return imageName + "-layout"
}

// ImageOwnerRef identifies the Image an ingest run is for, so the
// ImageLayout it writes can carry an owner reference back to that Image
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

// LayoutWriter persists the ImageLayout once ingest has captured a
// disk's sfdisk dump. The real implementation talks to the Kubernetes
// API directly (see cmd/ingest); tests wire a fake.
type LayoutWriter interface {
	WriteLayout(ctx context.Context, sfdiskJSON string) error
}

// WriteImageLayout creates or updates the ImageLayout for owner's Image
// in namespace, owner-ref'd to it, holding sfdiskJSON in
// spec.sfdiskJSON. This is the ingest Job's own side of moving the
// sfdisk dump off the container termination message (see Result's doc
// comment): the Job writes this object itself rather than relaying the
// dump back through the Image controller, since the controller's own
// result channel is the 4KiB-capped termination message this exists to
// stop overflowing.
func WriteImageLayout(ctx context.Context, c client.Client, namespace string, owner ImageOwnerRef, sfdiskJSON string) error {
	name := ImageLayoutName(owner.Name)
	key := client.ObjectKey{Name: name, Namespace: namespace}
	ownerRefs := []metav1.OwnerReference{{
		APIVersion: owner.APIVersion,
		Kind:       owner.Kind,
		Name:       owner.Name,
		UID:        owner.UID,
	}}

	existing := &keziov1alpha1.ImageLayout{}
	switch err := c.Get(ctx, key, existing); {
	case apierrors.IsNotFound(err):
		layout := &keziov1alpha1.ImageLayout{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				OwnerReferences: ownerRefs,
			},
			Spec: keziov1alpha1.ImageLayoutSpec{SfdiskJSON: sfdiskJSON},
		}
		if err := c.Create(ctx, layout); err != nil {
			return fmt.Errorf("create imagelayout %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get imagelayout %s: %w", name, err)
	}

	updated := existing.DeepCopy()
	updated.Spec.SfdiskJSON = sfdiskJSON
	updated.OwnerReferences = ownerRefs
	if err := c.Update(ctx, updated); err != nil {
		return fmt.Errorf("update imagelayout %s: %w", name, err)
	}
	return nil
}

// clientLayoutWriter adapts WriteImageLayout to the LayoutWriter
// interface, binding a fixed client/namespace/owner for the lifetime of
// one ingest run.
type clientLayoutWriter struct {
	client    client.Client
	namespace string
	owner     ImageOwnerRef
}

func (w *clientLayoutWriter) WriteLayout(ctx context.Context, sfdiskJSON string) error {
	return WriteImageLayout(ctx, w.client, w.namespace, w.owner, sfdiskJSON)
}

// NewLayoutWriter builds the LayoutWriter a production ingest run wires:
// c is expected to be backed by the Job pod's in-cluster ServiceAccount
// credentials and a scheme that knows the kezio API group (see
// cmd/ingest), namespace is the Image's namespace, and owner identifies
// the Image to own the ImageLayout.
func NewLayoutWriter(c client.Client, namespace string, owner ImageOwnerRef) LayoutWriter {
	return &clientLayoutWriter{client: c, namespace: namespace, owner: owner}
}
