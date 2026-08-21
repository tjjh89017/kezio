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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func validBuiltinStep() keziov1alpha2.PostHookStep {
	return keziov1alpha2.PostHookStep{
		Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback},
	}
}

func TestValidatePostHookParams(t *testing.T) {
	tests := []struct {
		name    string
		params  []keziov1alpha2.PostHookParam
		wantErr string
	}{
		{
			name:   "no params",
			params: nil,
		},
		{
			name: "unique names",
			params: []keziov1alpha2.PostHookParam{
				{Name: "a"}, {Name: "b"},
			},
		},
		{
			name: "duplicate name",
			params: []keziov1alpha2.PostHookParam{
				{Name: "a"}, {Name: "a"},
			},
			wantErr: `spec.params[1]: duplicate param name "a"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePostHookParams(tt.params)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePostHookStep(t *testing.T) {
	declared := map[string]bool{
		"foo":         true,
		"machineName": true,
		"imageName":   true,
		"targetDisk":  true,
	}

	tests := []struct {
		name    string
		step    keziov1alpha2.PostHookStep
		wantErr string
	}{
		{
			name:    "no kind set",
			step:    keziov1alpha2.PostHookStep{},
			wantErr: "spec.steps[0]: exactly one of builtin, script, or chrootScript must be set",
		},
		{
			name: "builtin only, unrestricted",
			step: validBuiltinStep(),
		},
		{
			name: "restricted builtin without osFamily",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap},
			},
			wantErr: `spec.steps[0].builtin: "mkswap" requires osFamily to be set to "Linux", got ""`,
		},
		{
			name: "restricted builtin with wrong osFamily",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyWindows,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepEfibootmgr},
			},
			wantErr: `spec.steps[0].builtin: "efibootmgr" requires osFamily to be set to "Linux", got "Windows"`,
		},
		{
			name: "restricted builtin with osFamily linux",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepGrowLastPartition},
			},
		},
		{
			name: "script with declared placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .foo }}"},
			},
		},
		{
			name: "script with reserved placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .machineName }}"},
			},
		},
		{
			name: "script with undeclared placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .bogus }}"},
			},
			wantErr: `spec.steps[0].script.script: placeholder "bogus" does not reference a declared param or a reserved name (machineName, imageName, targetDisk)`,
		},
		{
			name: "chrootScript with undeclared placeholder",
			step: keziov1alpha2.PostHookStep{
				ChrootScript: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .bogus }}"},
			},
			wantErr: `spec.steps[0].chrootScript.script: placeholder "bogus" does not reference a declared param or a reserved name (machineName, imageName, targetDisk)`,
		},
		{
			name: "script sourced from configMapRef skips placeholder checks",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "cm", Key: "run.sh"}},
			},
		},
		{
			name: "script with no source set",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{},
			},
			wantErr: "spec.steps[0].script: exactly one of script, configMapRef, or secretRef must be set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePostHookStep(0, tt.step, declared)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeclaredPlaceholderNames(t *testing.T) {
	names := declaredPlaceholderNames([]keziov1alpha2.PostHookParam{{Name: "foo"}})
	for _, want := range []string{"foo", "machineName", "imageName", "targetDisk"} {
		if !names[want] {
			t.Errorf("expected %q to be a declared placeholder name", want)
		}
	}
	if names["bogus"] {
		t.Error("expected bogus to not be declared")
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
