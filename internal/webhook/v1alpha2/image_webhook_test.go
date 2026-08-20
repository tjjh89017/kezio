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

var _ = Describe("Image Webhook", func() {
	var (
		obj       *keziov1alpha2.Image
		oldObj    *keziov1alpha2.Image
		validator ImageCustomValidator
	)

	BeforeEach(func() {
		obj = &keziov1alpha2.Image{}
		oldObj = &keziov1alpha2.Image{}
		validator = ImageCustomValidator{Client: k8sClient}
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

	Context("slot sizeBytes versus referenced PartitionContent lastExtentEnd", func() {
		var pcCount int

		newPartitionContent := func(lastExtentEnd int64) *keziov1alpha2.PartitionContent {
			pcCount++
			pc := &keziov1alpha2.PartitionContent{
				ObjectMeta: metav1.ObjectMeta{
					// "1" leads the hash to keep names distinct from
					// partitioncontent_schema_test.go's own pc-%040x
					// sequence, which starts from the same counter values.
					Name:      fmt.Sprintf("pc-1%039x", pcCount),
					Namespace: "default",
				},
				Spec: keziov1alpha2.PartitionContentSpec{
					FSType:        "ext4",
					UsedBytes:     lastExtentEnd,
					SizeBytes:     lastExtentEnd,
					LastExtentEnd: lastExtentEnd,
					PieceLength:   16384,
					Source: keziov1alpha2.PartitionContentSource{
						ImageName:       "source-image",
						PartitionNumber: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, pc)).To(Succeed())
			return pc
		}

		newImage := func(name string, slots ...keziov1alpha2.ImageSlot) *keziov1alpha2.Image {
			return &keziov1alpha2.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
				},
				Spec: keziov1alpha2.ImageSpec{
					Layout: keziov1alpha2.ImageDiskLayout{
						PartitionTable: keziov1alpha2.PartitionTableGPT,
						SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
						Slots:          slots,
					},
				},
			}
		}

		It("admits a slot whose sizeBytes equals the content's lastExtentEnd", func() {
			pc := newPartitionContent(2048)
			img := newImage("image-slot-size-equal", keziov1alpha2.ImageSlot{
				Number:     1,
				Role:       keziov1alpha2.PartitionRoleData,
				ContentRef: &keziov1alpha2.NameRef{Name: pc.Name},
				SizeBytes:  2048,
			})
			Expect(validator.ValidateCreate(ctx, img)).Error().NotTo(HaveOccurred())
		})

		It("admits a slot whose sizeBytes exceeds the content's lastExtentEnd", func() {
			pc := newPartitionContent(2048)
			img := newImage("image-slot-size-larger", keziov1alpha2.ImageSlot{
				Number:     1,
				Role:       keziov1alpha2.PartitionRoleData,
				ContentRef: &keziov1alpha2.NameRef{Name: pc.Name},
				SizeBytes:  4096,
			})
			Expect(validator.ValidateCreate(ctx, img)).Error().NotTo(HaveOccurred())
		})

		It("denies a slot whose sizeBytes is smaller than the content's lastExtentEnd", func() {
			pc := newPartitionContent(4096)
			img := newImage("image-slot-size-smaller", keziov1alpha2.ImageSlot{
				Number:     3,
				Role:       keziov1alpha2.PartitionRoleData,
				ContentRef: &keziov1alpha2.NameRef{Name: pc.Name},
				SizeBytes:  2048,
			})
			_, err := validator.ValidateCreate(ctx, img)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("slot 3"))
			Expect(err.Error()).To(ContainSubstring(pc.Name))
		})

		It("admits a slot whose contentRef does not resolve, with a warning naming it", func() {
			img := newImage("image-slot-missing-referent", keziov1alpha2.ImageSlot{
				Number:     1,
				Role:       keziov1alpha2.PartitionRoleData,
				ContentRef: &keziov1alpha2.NameRef{Name: "pc-0000000000000000000000000000000000000000"},
				SizeBytes:  2048,
			})
			warnings, err := validator.ValidateCreate(ctx, img)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring("pc-0000000000000000000000000000000000000000")))
		})

		It("admits a slot with contentRef but no declared sizeBytes, with a warning naming it", func() {
			pc := newPartitionContent(4096)
			img := newImage("image-slot-no-size", keziov1alpha2.ImageSlot{
				Number:     1,
				Role:       keziov1alpha2.PartitionRoleData,
				ContentRef: &keziov1alpha2.NameRef{Name: pc.Name},
			})
			warnings, err := validator.ValidateCreate(ctx, img)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring(pc.Name)))
		})

		It("denies when only one of several slots violates the rule, naming the violating slot", func() {
			okContent := newPartitionContent(1024)
			badContent := newPartitionContent(4096)
			img := newImage("image-slot-multi",
				keziov1alpha2.ImageSlot{
					Number:     1,
					Role:       keziov1alpha2.PartitionRoleESP,
					ContentRef: &keziov1alpha2.NameRef{Name: okContent.Name},
					SizeBytes:  2048,
				},
				keziov1alpha2.ImageSlot{
					Number:     2,
					Role:       keziov1alpha2.PartitionRoleData,
					ContentRef: &keziov1alpha2.NameRef{Name: badContent.Name},
					SizeBytes:  2048,
				},
			)
			_, err := validator.ValidateCreate(ctx, img)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("slot 2"))
			Expect(err.Error()).To(ContainSubstring(badContent.Name))
		})

		It("ignores blank and swap/uuid slots that carry no contentRef", func() {
			img := newImage("image-slot-blank-and-swap",
				keziov1alpha2.ImageSlot{
					Number:    1,
					Role:      keziov1alpha2.PartitionRoleData,
					FSType:    "ext4",
					SizeBytes: 1,
				},
				keziov1alpha2.ImageSlot{
					Number:    2,
					Role:      keziov1alpha2.PartitionRoleSwap,
					UUID:      "11111111-1111-1111-1111-111111111111",
					SizeBytes: 1,
				},
			)
			Expect(validator.ValidateCreate(ctx, img)).Error().NotTo(HaveOccurred())
		})
	})

	Context("admission round-trip through the webhook server", func() {
		It("admits a valid Image", func() {
			created := &keziov1alpha2.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "image-admission-roundtrip",
					Namespace: "default",
				},
				Spec: keziov1alpha2.ImageSpec{
					Layout: keziov1alpha2.ImageDiskLayout{
						PartitionTable: keziov1alpha2.PartitionTableGPT,
						SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
						Slots: []keziov1alpha2.ImageSlot{
							{
								Number: 1,
								Role:   keziov1alpha2.PartitionRoleData,
								FSType: "ext4",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})

		It("denies a slot whose sizeBytes is smaller than the referenced PartitionContent's lastExtentEnd", func() {
			pc := &keziov1alpha2.PartitionContent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pc-abcdef0123456789abcdef0123456789abcdef01",
					Namespace: "default",
				},
				Spec: keziov1alpha2.PartitionContentSpec{
					FSType:        "ext4",
					UsedBytes:     4096,
					SizeBytes:     4096,
					LastExtentEnd: 4096,
					PieceLength:   16384,
					Source: keziov1alpha2.PartitionContentSource{
						ImageName:       "image-b",
						PartitionNumber: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, pc)).To(Succeed())

			// The webhook runs against the manager's cache-backed client
			// (SetupImageWebhookWithManager wires mgr.GetClient()), not the
			// k8sClient used above to create pc. That cache may not have
			// synced the just-created PartitionContent by the time the first
			// attempt below runs, in which case the content lookup 404s and
			// the slot is admitted (with a warning) instead of denied. Retry
			// with a fresh candidate name each attempt, so a prior accepted
			// attempt's object can't mask the real assertion behind
			// AlreadyExists, until the deny fires for the expected reason.
			attempt := 0
			Eventually(func(g Gomega) {
				attempt++
				candidate := &keziov1alpha2.Image{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("image-admission-roundtrip-deny-%d", attempt),
						Namespace: "default",
					},
					Spec: keziov1alpha2.ImageSpec{
						Layout: keziov1alpha2.ImageDiskLayout{
							PartitionTable: keziov1alpha2.PartitionTableGPT,
							SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
							Slots: []keziov1alpha2.ImageSlot{
								{
									Number:     1,
									Role:       keziov1alpha2.PartitionRoleData,
									ContentRef: &keziov1alpha2.NameRef{Name: pc.Name},
									SizeBytes:  2048,
								},
							},
						},
					},
				}
				err := k8sClient.Create(ctx, candidate)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("smaller than"))
			}).Should(Succeed())
		})
	})
})
