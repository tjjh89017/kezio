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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there.
var _ = Describe("ImageImport CRD schema", func() {
	var importCount int

	newImageImport := func() *keziov1alpha2.ImageImport {
		importCount++
		return &keziov1alpha2.ImageImport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("imageimport-%d", importCount),
				Namespace: "default",
			},
			Spec: keziov1alpha2.ImageImportSpec{
				Source: keziov1alpha2.ImportSource{
					URL:      "kezio-staged://ubuntu-2404-golden",
					Checksum: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				},
				ImageName:     fmt.Sprintf("ubuntu-2404-%d", importCount),
				ContentPrefix: fmt.Sprintf("ubuntu-2404-%d", importCount),
			},
		}
	}

	It("admits a minimal valid ImageImport", func() {
		Expect(k8sClient.Create(ctx, newImageImport())).To(Succeed())
	})

	It("rejects a source with a malformed checksum", func() {
		imp := newImageImport()
		imp.Spec.Source.Checksum = "md5:0123456789abcdef"
		Expect(k8sClient.Create(ctx, imp)).To(HaveOccurred())
	})

	It("rejects an empty contentPrefix", func() {
		imp := newImageImport()
		imp.Spec.ContentPrefix = ""
		Expect(k8sClient.Create(ctx, imp)).To(HaveOccurred())
	})

	It("rejects a contentPrefix that cannot form a valid content name", func() {
		imp := newImageImport()
		imp.Spec.ContentPrefix = "Ubuntu_2404"
		Expect(k8sClient.Create(ctx, imp)).To(HaveOccurred())
	})

	It("rejects a contentPrefix that leaves no room for the -p<N> and -content suffixes", func() {
		imp := newImageImport()
		imp.Spec.ContentPrefix = strings.Repeat("a", 241)
		Expect(k8sClient.Create(ctx, imp)).To(HaveOccurred())
	})

	It("rejects an empty imageName", func() {
		imp := newImageImport()
		imp.Spec.ImageName = ""
		Expect(k8sClient.Create(ctx, imp)).To(HaveOccurred())
	})

	It("rejects a spec update after creation", func() {
		imp := newImageImport()
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())

		imp.Spec.ContentPrefix = "something-else"
		Expect(k8sClient.Update(ctx, imp)).To(HaveOccurred())
	})

	It("admits a status update that leaves spec untouched", func() {
		imp := newImageImport()
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())

		imp.Status.State = keziov1alpha2.ImageImportStateReady
		imp.Status.ImageRef = &keziov1alpha2.NameRef{Name: imp.Spec.ImageName}
		Expect(k8sClient.Status().Update(ctx, imp)).To(Succeed())
	})

	It("rejects an unknown status.state value", func() {
		imp := newImageImport()
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())

		imp.Status.State = "not-a-real-value"
		Expect(k8sClient.Status().Update(ctx, imp)).To(HaveOccurred())
	})
})
