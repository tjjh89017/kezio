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
// and CEL validation only run there.
var _ = Describe("PartitionContent CRD schema", func() {
	var pcCount int

	newPartitionContent := func() *keziov1alpha2.PartitionContent {
		pcCount++
		return &keziov1alpha2.PartitionContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pc-%040x", pcCount),
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
	}

	It("admits a minimal valid PartitionContent", func() {
		pc := newPartitionContent()
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
	})

	It("rejects a PartitionContent with no fsType", func() {
		pc := newPartitionContent()
		pc.Spec.FSType = ""
		Expect(k8sClient.Create(ctx, pc)).To(HaveOccurred())
	})

	It("rejects a PartitionContent with no source", func() {
		pc := newPartitionContent()
		pc.Spec.Source = keziov1alpha2.PartitionContentSource{}
		Expect(k8sClient.Create(ctx, pc)).To(HaveOccurred())
	})

	It("rejects a spec update after creation", func() {
		pc := newPartitionContent()
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		pc.Spec.FSType = "ntfs"
		Expect(k8sClient.Update(ctx, pc)).To(HaveOccurred())
	})

	It("admits a status update that leaves spec untouched", func() {
		pc := newPartitionContent()
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		pc.Status.State = keziov1alpha2.PartitionContentStatePublishing
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	})

	It("rejects an unknown status.state value", func() {
		pc := newPartitionContent()
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		pc.Status.State = "Bogus"
		Expect(k8sClient.Status().Update(ctx, pc)).To(HaveOccurred())
	})

	It("rejects a name with the wrong prefix", func() {
		pc := newPartitionContent()
		pc.Name = fmt.Sprintf("wrong-%040x", pcCount)
		Expect(k8sClient.Create(ctx, pc)).To(HaveOccurred())
	})

	It("rejects a name whose hash is not 40 hex characters", func() {
		pc := newPartitionContent()
		pc.Name = "pc-abc123"
		Expect(k8sClient.Create(ctx, pc)).To(HaveOccurred())
	})

	It("admits a name with a valid pc-prefixed 40-character hex hash", func() {
		pc := newPartitionContent()
		pc.Name = "pc-0123456789abcdef0123456789abcdef01234567"
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
	})
})
