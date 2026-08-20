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

const validPartitionContentName = "pc-0123456789abcdef0123456789abcdef01234567"

var _ = Describe("PartitionContent Webhook", func() {
	var (
		obj       *keziov1alpha2.PartitionContent
		oldObj    *keziov1alpha2.PartitionContent
		validator PartitionContentCustomValidator
	)

	BeforeEach(func() {
		obj = &keziov1alpha2.PartitionContent{
			ObjectMeta: metav1.ObjectMeta{Name: validPartitionContentName},
		}
		oldObj = &keziov1alpha2.PartitionContent{
			ObjectMeta: metav1.ObjectMeta{Name: validPartitionContentName},
		}
		validator = PartitionContentCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	Context("When creating or updating PartitionContent under Validating Webhook", func() {
		It("admits a valid PartitionContent on create", func() {
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("admits a valid PartitionContent on update", func() {
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().NotTo(HaveOccurred())
		})

		It("rejects a name with the wrong prefix", func() {
			obj.Name = "wrong-0123456789abcdef0123456789abcdef01234567"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("rejects a name with a hash that is too short", func() {
			obj.Name = "pc-0123"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("rejects a name with non-hex characters", func() {
			obj.Name = "pc-g123456789abcdef0123456789abcdef0123456"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})
	})

	Context("admission round-trip through the webhook server", func() {
		It("admits a valid PartitionContent", func() {
			created := &keziov1alpha2.PartitionContent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pc-fedcba9876543210fedcba9876543210fedcba98",
					Namespace: "default",
				},
				Spec: keziov1alpha2.PartitionContentSpec{
					FSType:        "ext4",
					UsedBytes:     1024,
					SizeBytes:     2048,
					LastExtentEnd: 2048,
					PieceLength:   16384,
					Source: keziov1alpha2.PartitionContentSource{
						ImageName:       "image-a",
						PartitionNumber: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})

		It("rejects a name that is not a pc-prefixed info hash", func() {
			created := &keziov1alpha2.PartitionContent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "not-a-valid-name",
					Namespace: "default",
				},
				Spec: keziov1alpha2.PartitionContentSpec{
					FSType:        "ext4",
					UsedBytes:     1024,
					SizeBytes:     2048,
					LastExtentEnd: 2048,
					PieceLength:   16384,
					Source: keziov1alpha2.PartitionContentSource{
						ImageName:       "image-a",
						PartitionNumber: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(HaveOccurred())
		})
	})
})
