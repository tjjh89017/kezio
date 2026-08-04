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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// This test reproduces a real CI failure: an Image created the way
// kubectl apply creates it (a YAML manifest with no spec.params at all,
// decoded to unstructured JSON rather than through a typed Go struct)
// went through the real admission chain - including the defaulting
// webhook - and was then updated by this package's reconciler to attach
// the finalizer. That Update was rejected: "spec: Invalid value:
// \"object\": spec is immutable once the Image is created".
//
// The mechanism: ImageSpec.Params used to be a value-typed
// apiextensionsv1.JSON. Even though ImageCustomDefaulter.Default is a
// no-op, controller-runtime's CustomDefaulter handler always re-marshals
// the decoded object to compute the admission patch (see
// sigs.k8s.io/controller-runtime/pkg/webhook/admission/defaulter_custom.go).
// apiextensionsv1.JSON's MarshalJSON emits a literal "null" for its zero
// value, and Go's encoding/json "omitempty" does not suppress a
// zero-value struct field - so the re-marshaled object always carried
// "params": null even though the original request (built from a
// manifest with no params key at all) did not. That mismatch produced a
// spurious "add /spec/params null" patch, applied on every Image create,
// which the whole-object "self == oldSelf" CEL transition rule then
// disagreed with on the next spec-bearing update.
//
// Neither existing envtest suite caught this before the fix: this
// package's suite never started the webhook server (so the finalizer-add
// Update never went through the real admission chain), and
// internal/webhook/v1alpha1's suite - while it does start a real webhook
// server - only calls ImageCustomValidator/ImageCustomDefaulter's
// methods directly in its tests, never issuing a real admission request
// through it either. Only a real k3s apiserver, driving a real
// kubectl-apply-shaped Create through the real webhook server, hit the
// patch-computation bug.
//
// The fix (ImageSpec.Params became a pointer, so its zero value is
// omitted rather than marshaled as an explicit null) and the CRD's
// immutability rule (now scoped per field instead of once over the whole
// struct, with spec.params immutability checked in Go instead of CEL)
// both remove the failure mode; this test's job is to keep it removed.
var _ = Describe("an Image created the way kubectl apply creates it", func() {
	It("survives the defaulting webhook and a subsequent finalizer-add Update with no validation error", func() {
		ctx := context.Background()
		const resourceName = "kubectl-apply-shaped-image"
		namespace := "default"
		key := types.NamespacedName{Name: resourceName, Namespace: namespace}

		// Built from raw unstructured content - the same shape kubectl
		// apply produces from a YAML manifest - rather than a typed
		// keziov1alpha1.Image Go struct, so spec.params really is absent
		// from the wire bytes rather than merely zero-valued in Go.
		image := &unstructured.Unstructured{}
		image.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kezio.kojuro.date",
			Version: "v1alpha1",
			Kind:    "Image",
		})
		image.SetName(resourceName)
		image.SetNamespace(namespace)
		Expect(unstructured.SetNestedMap(image.Object, map[string]interface{}{
			"source": map[string]interface{}{
				"url":    "https://example.com/golden.qcow2",
				"format": keziov1alpha1.ImageFormatQCOW2,
			},
			"bootable": true,
			"osFamily": keziov1alpha1.OSFamilyLinux,
		}, "spec")).To(Succeed())

		Expect(k8sClient.Create(ctx, image)).To(Succeed())

		typedImage := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, typedImage)).To(Succeed())

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, typedImage) })

		By("reconciling to attach the finalizer without a validation error")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	})
})
