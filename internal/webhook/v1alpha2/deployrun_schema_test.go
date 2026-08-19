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
var _ = Describe("DeployRun CRD schema", func() {
	var runCount int

	newDeployRun := func() *keziov1alpha2.DeployRun {
		runCount++
		return &keziov1alpha2.DeployRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-run-%d", runCount),
				Namespace: "default",
			},
			Spec: keziov1alpha2.DeployRunSpec{
				MachineRef: keziov1alpha2.NameRef{Name: "machine-a"},
			},
		}
	}

	It("admits a minimal valid DeployRun", func() {
		r := newDeployRun()
		Expect(k8sClient.Create(ctx, r)).To(Succeed())
	})

	It("rejects a DeployRun with no machineRef", func() {
		r := newDeployRun()
		r.Spec.MachineRef = keziov1alpha2.NameRef{}
		Expect(k8sClient.Create(ctx, r)).To(HaveOccurred())
	})

	It("rejects more than 32 dataImages entries", func() {
		r := newDeployRun()
		for i := 0; i < 33; i++ {
			r.Spec.DataImages = append(r.Spec.DataImages, keziov1alpha2.MachineDataImage{
				ImageRef: keziov1alpha2.NameRef{Name: fmt.Sprintf("data-image-%d", i)},
			})
		}
		Expect(k8sClient.Create(ctx, r)).To(HaveOccurred())
	})

	It("rejects a hooksHash longer than 64 characters", func() {
		r := newDeployRun()
		long := ""
		for i := 0; i < 65; i++ {
			long += "a"
		}
		r.Spec.HooksHash = long
		Expect(k8sClient.Create(ctx, r)).To(HaveOccurred())
	})

	It("rejects a spec update after creation", func() {
		r := newDeployRun()
		Expect(k8sClient.Create(ctx, r)).To(Succeed())

		r.Spec.MachineRef = keziov1alpha2.NameRef{Name: "machine-b"}
		Expect(k8sClient.Update(ctx, r)).To(HaveOccurred())
	})

	It("admits a status update that leaves spec untouched", func() {
		r := newDeployRun()
		Expect(k8sClient.Create(ctx, r)).To(Succeed())

		r.Status.Phase = keziov1alpha2.DeployRunPhasePartitioning
		Expect(k8sClient.Status().Update(ctx, r)).To(Succeed())
	})
})
