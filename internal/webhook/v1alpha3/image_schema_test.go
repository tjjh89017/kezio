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

package v1alpha3

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// notARealValue stands in for any value outside an enum.
const notARealValue = "not-a-real-value"

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there.
var _ = Describe("Image CRD schema", func() {
	var imgCount int

	newImage := func() *keziov1alpha3.Image {
		imgCount++
		return &keziov1alpha3.Image{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("image-%d", imgCount),
				Namespace: "default",
			},
			Spec: keziov1alpha3.ImageSpec{
				OSFamily: keziov1alpha3.OSFamilyLinux,
				Layout: keziov1alpha3.ImageDiskLayout{
					PartitionTable: keziov1alpha3.PartitionTableGPT,
					SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
					Slots: []keziov1alpha3.ImageSlot{
						{
							Number: 1,
							Role:   keziov1alpha3.PartitionRoleESP,
							ContentRef: &keziov1alpha3.NameRef{
								Name: "ubuntu-2404-p1",
							},
						},
						{
							Number: 2,
							Role:   keziov1alpha3.PartitionRoleData,
							FSType: "xfs",
						},
					},
				},
			},
		}
	}

	It("admits a minimal valid Image", func() {
		img := newImage()
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
	})

	It("rejects a slot with both contentRef and fsType", func() {
		img := newImage()
		img.Spec.Layout.Slots[1].ContentRef = &keziov1alpha3.NameRef{
			Name: "ubuntu-2404-p1",
		}
		Expect(k8sClient.Create(ctx, img)).To(HaveOccurred())
	})

	It("rejects duplicate slot numbers", func() {
		img := newImage()
		img.Spec.Layout.Slots = append(img.Spec.Layout.Slots, keziov1alpha3.ImageSlot{
			Number: 1,
			Role:   keziov1alpha3.PartitionRoleData,
			FSType: "ext4",
		})
		Expect(k8sClient.Create(ctx, img)).To(HaveOccurred())
	})

	It("admits a contentRef naming any user-chosen content name", func() {
		img := newImage()
		img.Spec.Layout.Slots[0].ContentRef = &keziov1alpha3.NameRef{Name: "ubuntu-2404-p1"}
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
	})

	It("rejects an unknown osFamily value", func() {
		img := newImage()
		img.Spec.OSFamily = notARealValue
		Expect(k8sClient.Create(ctx, img)).To(HaveOccurred())
	})

	It("rejects an unknown partitionTable value", func() {
		img := newImage()
		img.Spec.Layout.PartitionTable = "bogus"
		Expect(k8sClient.Create(ctx, img)).To(HaveOccurred())
	})

	It("rejects an unknown slot role value", func() {
		img := newImage()
		img.Spec.Layout.Slots[0].Role = "bogus"
		Expect(k8sClient.Create(ctx, img)).To(HaveOccurred())
	})

	It("rejects a spec update after creation", func() {
		img := newImage()
		Expect(k8sClient.Create(ctx, img)).To(Succeed())

		img.Spec.OSFamily = keziov1alpha3.OSFamilyWindows
		Expect(k8sClient.Update(ctx, img)).To(HaveOccurred())
	})

	It("admits a status update that leaves spec untouched", func() {
		img := newImage()
		Expect(k8sClient.Create(ctx, img)).To(Succeed())

		img.Status.State = keziov1alpha3.ImageStateReady
		Expect(k8sClient.Status().Update(ctx, img)).To(Succeed())
	})

	It("rejects an unknown status.state value", func() {
		img := newImage()
		Expect(k8sClient.Create(ctx, img)).To(Succeed())

		img.Status.State = notARealValue
		Expect(k8sClient.Status().Update(ctx, img)).To(HaveOccurred())
	})
})
