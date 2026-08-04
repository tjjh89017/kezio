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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

var _ = Describe("Image Webhook", func() {
	var (
		obj       *keziov1alpha1.Image
		oldObj    *keziov1alpha1.Image
		validator ImageCustomValidator
		defaulter ImageCustomDefaulter
	)

	validSpec := func() keziov1alpha1.ImageSpec {
		return keziov1alpha1.ImageSpec{
			Source: keziov1alpha1.ImageSource{
				URL:      "https://example.invalid/golden.raw",
				Format:   keziov1alpha1.ImageFormatRaw,
				Checksum: "sha256:" + repeat("a", 64),
			},
			OSFamily: keziov1alpha1.OSFamilyLinux,
		}
	}

	BeforeEach(func() {
		obj = &keziov1alpha1.Image{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "image-", Namespace: "default"},
			Spec:       validSpec(),
		}
		oldObj = &keziov1alpha1.Image{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "image-", Namespace: "default"},
			Spec:       validSpec(),
		}
		validator = ImageCustomValidator{Client: k8sClient}
		defaulter = ImageCustomDefaulter{}
		Expect(defaulter).NotTo(BeNil())
	})

	Context("When creating or updating Image under Validating Webhook", func() {
		It("Should admit a fully valid spec on create", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a fully valid spec on update", func() {
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a spec with no checksum set", func() {
			obj.Spec.Source.Checksum = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny creation when the checksum has no algorithm prefix", func() {
			obj.Spec.Source.Checksum = repeat("a", 64)
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.source.checksum"))
		})

		It("Should deny creation when the checksum algorithm is unsupported", func() {
			obj.Spec.Source.Checksum = "crc32:deadbeef"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported checksum algorithm"))
		})

		It("Should deny creation when the sha256 digest is the wrong length", func() {
			obj.Spec.Source.Checksum = "sha256:" + repeat("a", 10)
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("64 hex characters"))
		})

		It("Should deny creation when the digest contains non-hex characters", func() {
			obj.Spec.Source.Checksum = "sha256:" + repeat("g", 64)
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("hexadecimal"))
		})

		It("Should deny a non-Linux Image referencing a PostHook with a chrootScript step", func() {
			postHook := &keziov1alpha1.PostHook{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "posthook-", Namespace: "default"},
				Spec: keziov1alpha1.PostHookSpec{
					Steps: []keziov1alpha1.PostHookStep{
						{ChrootScript: &keziov1alpha1.PostHookScriptSource{Script: "echo hi"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, postHook)).To(Succeed())

			obj.Spec.OSFamily = keziov1alpha1.OSFamilyWindows
			obj.Spec.PostHookRefs = []keziov1alpha1.NameRef{{Name: postHook.Name}}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("chrootScript"))
		})

		It("Should admit a Linux Image referencing a PostHook with a chrootScript step", func() {
			postHook := &keziov1alpha1.PostHook{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "posthook-", Namespace: "default"},
				Spec: keziov1alpha1.PostHookSpec{
					Steps: []keziov1alpha1.PostHookStep{
						{ChrootScript: &keziov1alpha1.PostHookScriptSource{Script: "echo hi"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, postHook)).To(Succeed())

			obj.Spec.OSFamily = keziov1alpha1.OSFamilyLinux
			obj.Spec.PostHookRefs = []keziov1alpha1.NameRef{{Name: postHook.Name}}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a non-Linux Image referencing a missing PostHook", func() {
			obj.Spec.OSFamily = keziov1alpha1.OSFamilyWindows
			obj.Spec.PostHookRefs = []keziov1alpha1.NameRef{{Name: "does-not-exist"}}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should skip validation on update when the object is being deleted", func() {
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"example.com/finalizer"}
			obj.Spec.Source.Checksum = "not-a-checksum"

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny an update that changes spec.params", func() {
			oldObj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
			obj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte(`{"a":2}`)}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.params is immutable"))
		})

		// This is the regression case: a full-object Update built from a
		// freshly-Get'd typed Image (as the reconciler's finalizer-add
		// path does) always carries an explicit spec.params, because
		// apiextensionsv1.JSON's MarshalJSON emits a literal "null" for
		// a zero value that Go's "omitempty" cannot suppress. Treating
		// that as a change from an oldObj whose Params was never set
		// would reject every such Update - the exact failure this
		// validator exists to prevent from CEL. See paramsChanged.
		It("Should admit an update where spec.params round-trips absent-to-null (or null-to-absent)", func() {
			oldObj.Spec.Params = nil
			obj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte("null")}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())

			oldObj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte("null")}
			obj.Spec.Params = nil

			_, err = validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit an update that leaves spec.params content unchanged", func() {
			oldObj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
			obj.Spec.Params = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When creating Image under Defaulting Webhook", func() {
		It("Should not change the spec", func() {
			before := obj.Spec
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec).To(Equal(before))
		})
	})
})

// repeat returns s repeated n times, avoiding a strings import for this
// small test-only need.
func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
