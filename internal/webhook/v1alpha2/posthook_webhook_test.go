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

package v1alpha2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// validBuiltinStep builds a step using an OS-family-unrestricted builtin,
// so tests can populate a PostHook with a trivially valid step. The
// per-check unit tests for builtin/script/param validation itself live in
// internal/posthookvalidate; this file only exercises the webhook's own
// admission behavior (which delegates to that package).
func validBuiltinStep() keziov1alpha2.PostHookStep {
	return keziov1alpha2.PostHookStep{
		Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback},
	}
}

var _ = Describe("PostHook Webhook", func() {
	var (
		obj       *keziov1alpha2.PostHook
		oldObj    *keziov1alpha2.PostHook
		validator PostHookCustomValidator
	)

	newValidPostHook := func() *keziov1alpha2.PostHook {
		return &keziov1alpha2.PostHook{
			Spec: keziov1alpha2.PostHookSpec{
				Steps: []keziov1alpha2.PostHookStep{validBuiltinStep()},
			},
		}
	}

	BeforeEach(func() {
		obj = newValidPostHook()
		oldObj = newValidPostHook()
		validator = PostHookCustomValidator{}
	})

	Context("When creating or updating PostHook under Validating Webhook", func() {
		It("admits a valid PostHook on create", func() {
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("admits a valid PostHook on update", func() {
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies a step with no kind set on create", func() {
			obj.Spec.Steps = []keziov1alpha2.PostHookStep{{}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of builtin, script, or chrootScript"))
		})

		It("denies a step with no kind set on update", func() {
			obj.Spec.Steps = []keziov1alpha2.PostHookStep{{}}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
		})

		It("denies a restricted builtin without osFamily=Linux", func() {
			obj.Spec.Steps = []keziov1alpha2.PostHookStep{
				{Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap}},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("mkswap"))
		})

		It("denies a script placeholder that references an undeclared param", func() {
			obj.Spec.Steps = []keziov1alpha2.PostHookStep{
				{Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .bogus }}"}},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bogus"))
		})

		It("admits a script placeholder that references a declared param", func() {
			obj.Spec.Params = []keziov1alpha2.PostHookParam{{Name: "greeting"}}
			obj.Spec.Steps = []keziov1alpha2.PostHookStep{
				{Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .greeting }}"}},
			}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies a duplicate param name", func() {
			obj.Spec.Params = []keziov1alpha2.PostHookParam{{Name: "a"}, {Name: "a"}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate param name"))
		})

		It("admits deletion regardless of spec content", func() {
			obj.Spec.Steps = nil
			Expect(validator.ValidateDelete(ctx, obj)).Error().NotTo(HaveOccurred())
		})
	})

	Context("admission round-trip through the webhook server", func() {
		It("admits a valid PostHook", func() {
			created := &keziov1alpha2.PostHook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "posthook-admission-roundtrip",
					Namespace: "default",
				},
				Spec: keziov1alpha2.PostHookSpec{
					Steps: []keziov1alpha2.PostHookStep{validBuiltinStep()},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})

		It("denies a script step whose placeholder does not reference a declared param", func() {
			candidate := &keziov1alpha2.PostHook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "posthook-admission-roundtrip-deny",
					Namespace: "default",
				},
				Spec: keziov1alpha2.PostHookSpec{
					Steps: []keziov1alpha2.PostHookStep{
						{Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .bogus }}"}},
					},
				},
			}
			err := k8sClient.Create(ctx, candidate)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bogus"))
		})
	})
})
