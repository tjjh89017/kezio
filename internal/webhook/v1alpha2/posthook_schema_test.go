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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there. The PostHookCustomValidator's own
// rules are covered in posthook_webhook_test.go instead.
var _ = Describe("PostHook CRD schema", func() {
	var postHookCount int

	newPostHook := func() *keziov1alpha2.PostHook {
		postHookCount++
		return &keziov1alpha2.PostHook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-posthook-%d", postHookCount),
				Namespace: "default",
			},
			Spec: keziov1alpha2.PostHookSpec{
				Steps: []keziov1alpha2.PostHookStep{
					{Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback}},
				},
			},
		}
	}

	It("admits a minimal valid PostHook", func() {
		Expect(k8sClient.Create(ctx, newPostHook())).To(Succeed())
	})

	It("rejects an empty steps list", func() {
		ph := newPostHook()
		ph.Spec.Steps = nil
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects a step with none of builtin, script, or chrootScript set", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{{}}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects a step with both builtin and script set", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap},
				Script:  &keziov1alpha2.PostHookScriptSource{Script: "echo hi"},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects a builtin name outside the enum", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: "not-a-builtin"}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects an osFamily value outside the enum", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{
				OSFamily: "Plan9",
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects a script source with none of script, configMapRef, or secretRef set", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{Script: &keziov1alpha2.PostHookScriptSource{}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("rejects a script source with both script and configMapRef set", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{Script: &keziov1alpha2.PostHookScriptSource{
				Script:       "echo hi",
				ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "cm", Key: "run.sh"},
			}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("admits a chrootScript sourced from a secretRef", func() {
		ph := newPostHook()
		ph.Spec.Steps = []keziov1alpha2.PostHookStep{
			{ChrootScript: &keziov1alpha2.PostHookScriptSource{
				SecretRef: &keziov1alpha2.SecretKeyRef{Name: "creds", Key: "token"},
			}},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
	})

	It("rejects duplicate param names via the params list-map key", func() {
		ph := newPostHook()
		ph.Spec.Params = []keziov1alpha2.PostHookParam{
			{Name: "a"}, {Name: "a"},
		}
		Expect(k8sClient.Create(ctx, ph)).To(HaveOccurred())
	})

	It("admits params with distinct names and a default", func() {
		ph := newPostHook()
		defaultVal := "eth0"
		ph.Spec.Params = []keziov1alpha2.PostHookParam{
			{Name: "iface", Default: &defaultVal},
			{Name: "hostname"},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
	})
})
