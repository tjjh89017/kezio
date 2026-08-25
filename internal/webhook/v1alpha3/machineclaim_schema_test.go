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
	"k8s.io/utils/ptr"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there.
var _ = Describe("MachineClaim CRD schema", func() {
	var claimCount int

	newClaim := func() *keziov1alpha3.MachineClaim {
		claimCount++
		return &keziov1alpha3.MachineClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("claim-schema-test-%d", claimCount),
				Namespace: "default",
			},
			Spec: keziov1alpha3.MachineClaimSpec{
				MachineName: "machine-1",
				ImageRef:    &keziov1alpha3.NameRef{Name: "an-image"},
			},
		}
	}

	It("admits a minimal valid MachineClaim", func() {
		c := newClaim()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
	})

	It("rejects a claim giving both machineName and selector", func() {
		c := newClaim()
		c.Spec.Selector = &keziov1alpha3.MachineClaimSelector{}
		Expect(k8sClient.Create(ctx, c)).To(HaveOccurred())
	})

	It("rejects targetDisk hints where minSizeGigabytes exceeds maxSizeGigabytes", func() {
		c := newClaim()
		c.Spec.TargetDisk = &keziov1alpha3.TargetDiskHints{
			MinSizeGigabytes: ptr.To(int64(500)),
			MaxSizeGigabytes: ptr.To(int64(100)),
		}
		Expect(k8sClient.Create(ctx, c)).To(HaveOccurred())
	})

	It("admits targetDisk hints where minSizeGigabytes is at most maxSizeGigabytes", func() {
		c := newClaim()
		c.Spec.TargetDisk = &keziov1alpha3.TargetDiskHints{
			MinSizeGigabytes: ptr.To(int64(100)),
			MaxSizeGigabytes: ptr.To(int64(500)),
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
	})

	It("rejects a dataImages entry whose targetDisk hints violate the min/max CEL rule", func() {
		c := newClaim()
		c.Spec.DataImages = []keziov1alpha3.MachineDataImage{
			{
				ImageRef: keziov1alpha3.NameRef{Name: "data-image"},
				TargetDisk: &keziov1alpha3.TargetDiskHints{
					MinSizeGigabytes: ptr.To(int64(200)),
					MaxSizeGigabytes: ptr.To(int64(50)),
				},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(HaveOccurred())
	})

	It("rejects an afterDeploy value outside the enum", func() {
		c := newClaim()
		c.Spec.AfterDeploy = "Suspend"
		Expect(k8sClient.Create(ctx, c)).To(HaveOccurred())
	})

	It("defaults afterDeploy to Reboot when absent", func() {
		c := newClaim()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(c.Spec.AfterDeploy).To(Equal(keziov1alpha3.AfterDeployReboot))
	})

	It("rejects a claim giving neither imageRef nor dataImages", func() {
		c := newClaim()
		c.Spec.ImageRef = nil
		Expect(k8sClient.Create(ctx, c)).To(HaveOccurred())
	})
})
