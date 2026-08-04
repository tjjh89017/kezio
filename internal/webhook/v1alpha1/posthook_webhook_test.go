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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

var _ = Describe("PostHook Webhook", func() {
	var (
		obj       *keziov1alpha1.PostHook
		oldObj    *keziov1alpha1.PostHook
		validator PostHookCustomValidator
		defaulter PostHookCustomDefaulter
	)

	validSpec := func() keziov1alpha1.PostHookSpec {
		return keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{ChrootScript: &keziov1alpha1.PostHookScriptSource{Script: "echo hi"}},
				{Builtin: &keziov1alpha1.PostHookBuiltinStep{Name: keziov1alpha1.BuiltinStepMkswap}},
			},
		}
	}

	BeforeEach(func() {
		obj = &keziov1alpha1.PostHook{Spec: validSpec()}
		oldObj = &keziov1alpha1.PostHook{Spec: validSpec()}
		validator = PostHookCustomValidator{}
		defaulter = PostHookCustomDefaulter{}
		Expect(defaulter).NotTo(BeNil())
	})

	Context("When creating, updating, or deleting PostHook under the (no-op) Validating Webhook", func() {
		It("Should admit a spec containing a chrootScript step on create", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a spec containing a chrootScript step on update", func() {
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit deletion", func() {
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit an object being deleted on update", func() {
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"example.com/finalizer"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When creating PostHook under the (no-op) Defaulting Webhook", func() {
		It("Should not change the spec", func() {
			before := obj.Spec
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec).To(Equal(before))
		})
	})
})
