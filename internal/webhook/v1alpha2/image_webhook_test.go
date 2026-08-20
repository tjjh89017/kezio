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

var _ = Describe("Image Webhook", func() {
	var (
		obj       *keziov1alpha2.Image
		oldObj    *keziov1alpha2.Image
		validator ImageCustomValidator
	)

	BeforeEach(func() {
		obj = &keziov1alpha2.Image{}
		oldObj = &keziov1alpha2.Image{}
		validator = ImageCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	Context("When creating or updating Image under Validating Webhook", func() {
		It("admits a valid Image on create", func() {
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("admits a valid Image on update", func() {
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().NotTo(HaveOccurred())
		})
	})

	Context("admission round-trip through the webhook server", func() {
		It("admits a valid Image", func() {
			created := &keziov1alpha2.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "image-admission-roundtrip",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})
	})
})
