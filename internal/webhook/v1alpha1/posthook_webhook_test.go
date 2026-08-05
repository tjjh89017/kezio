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

// These specs submit PostHooks through k8sClient.Create against the real
// envtest apiserver. ValidateCreate/ValidateUpdate above are deliberate
// no-ops; the step/source exactly-one-of invariants and the non-empty
// steps invariant are enforced entirely by the CEL rules compiled into
// config/crd/bases/kezio.kojuro.date_posthooks.yaml, so only real
// admission exercises them.
var _ = Describe("PostHook CRD validation", func() {
	newPostHook := func(steps ...keziov1alpha1.PostHookStep) *keziov1alpha1.PostHook {
		return &keziov1alpha1.PostHook{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "posthook-", Namespace: "default"},
			Spec:       keziov1alpha1.PostHookSpec{Steps: steps},
		}
	}

	Context("PostHookStep exactly-one-of", func() {
		It("Should deny a step with two kinds set", func() {
			step := keziov1alpha1.PostHookStep{
				Builtin: &keziov1alpha1.PostHookBuiltinStep{Name: keziov1alpha1.BuiltinStepMkswap},
				Script:  &keziov1alpha1.PostHookScriptSource{Script: "echo hi"},
			}
			err := k8sClient.Create(ctx, newPostHook(step))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of builtin, script, or chrootScript must be set"))
		})

		It("Should deny a step with no kind set", func() {
			err := k8sClient.Create(ctx, newPostHook(keziov1alpha1.PostHookStep{}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of builtin, script, or chrootScript must be set"))
		})
	})

	Context("PostHookScriptSource exactly-one-of", func() {
		It("Should deny a script step with two sources set", func() {
			step := keziov1alpha1.PostHookStep{
				Script: &keziov1alpha1.PostHookScriptSource{
					Script:       "echo hi",
					ConfigMapRef: &keziov1alpha1.ConfigMapKeyRef{Name: "cm", Key: "run.sh"},
				},
			}
			err := k8sClient.Create(ctx, newPostHook(step))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of script, configMapRef, or secretRef must be set"))
		})

		It("Should deny a chrootScript step with no source set", func() {
			step := keziov1alpha1.PostHookStep{ChrootScript: &keziov1alpha1.PostHookScriptSource{}}
			err := k8sClient.Create(ctx, newPostHook(step))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of script, configMapRef, or secretRef must be set"))
		})
	})

	Context("Steps minimum length", func() {
		It("Should deny an empty steps list", func() {
			err := k8sClient.Create(ctx, newPostHook())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.steps"))
		})
	})

	DescribeTable("Should admit a valid step",
		func(step keziov1alpha1.PostHookStep) {
			postHook := newPostHook(step)
			Expect(k8sClient.Create(ctx, postHook)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, postHook)).To(Succeed()) })
		},
		Entry("builtin step", keziov1alpha1.PostHookStep{
			Builtin: &keziov1alpha1.PostHookBuiltinStep{Name: keziov1alpha1.BuiltinStepMkswap},
		}),
		Entry("script step with inline script", keziov1alpha1.PostHookStep{
			Script: &keziov1alpha1.PostHookScriptSource{Script: "echo hi"},
		}),
		Entry("script step with configMapRef", keziov1alpha1.PostHookStep{
			Script: &keziov1alpha1.PostHookScriptSource{
				ConfigMapRef: &keziov1alpha1.ConfigMapKeyRef{Name: "cm", Key: "run.sh"},
			},
		}),
		Entry("script step with secretRef", keziov1alpha1.PostHookStep{
			Script: &keziov1alpha1.PostHookScriptSource{
				SecretRef: &keziov1alpha1.SecretKeyRef{Name: "secret", Key: "run.sh"},
			},
		}),
		Entry("chrootScript step with inline script", keziov1alpha1.PostHookStep{
			ChrootScript: &keziov1alpha1.PostHookScriptSource{Script: "echo hi"},
		}),
		Entry("chrootScript step with configMapRef", keziov1alpha1.PostHookStep{
			ChrootScript: &keziov1alpha1.PostHookScriptSource{
				ConfigMapRef: &keziov1alpha1.ConfigMapKeyRef{Name: "cm", Key: "run.sh"},
			},
		}),
		Entry("chrootScript step with secretRef", keziov1alpha1.PostHookStep{
			ChrootScript: &keziov1alpha1.PostHookScriptSource{
				SecretRef: &keziov1alpha1.SecretKeyRef{Name: "secret", Key: "run.sh"},
			},
		}),
	)
})
