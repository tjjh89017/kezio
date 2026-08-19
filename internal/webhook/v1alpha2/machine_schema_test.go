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
	"k8s.io/utils/ptr"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there.
var _ = Describe("Machine CRD schema", func() {
	var machineCount int

	newMachine := func() *keziov1alpha2.Machine {
		machineCount++
		return &keziov1alpha2.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-%d", machineCount),
				Namespace: "default",
			},
			Spec: keziov1alpha2.MachineSpec{
				BMC: keziov1alpha2.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:01",
				SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
			},
		}
	}

	It("admits a minimal valid Machine", func() {
		m := newMachine()
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
	})

	It("rejects a bootMACAddress that is not a MAC address", func() {
		m := newMachine()
		m.Spec.BootMACAddress = "not-a-mac"
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("rejects a BMC address with no URL scheme", func() {
		m := newMachine()
		m.Spec.BMC.Address = "10.0.0.10"
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("rejects a BMC address whose scheme has no registered driver", func() {
		m := newMachine()
		m.Spec.BMC.Address = "unregistered-scheme://10.0.0.10"
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("rejects targetDisk hints where minSizeGigabytes exceeds maxSizeGigabytes", func() {
		m := newMachine()
		m.Spec.TargetDisk = &keziov1alpha2.TargetDiskHints{
			MinSizeGigabytes: ptr.To(int64(500)),
			MaxSizeGigabytes: ptr.To(int64(100)),
		}
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("admits targetDisk hints where minSizeGigabytes is at most maxSizeGigabytes", func() {
		m := newMachine()
		m.Spec.TargetDisk = &keziov1alpha2.TargetDiskHints{
			MinSizeGigabytes: ptr.To(int64(100)),
			MaxSizeGigabytes: ptr.To(int64(500)),
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
	})

	It("rejects a dataImages entry whose targetDisk hints violate the min/max CEL rule", func() {
		m := newMachine()
		m.Spec.DataImages = []keziov1alpha2.MachineDataImage{
			{
				ImageRef: keziov1alpha2.NameRef{Name: "data-image"},
				TargetDisk: &keziov1alpha2.TargetDiskHints{
					MinSizeGigabytes: ptr.To(int64(200)),
					MaxSizeGigabytes: ptr.To(int64(50)),
				},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("rejects an afterDeploy value outside the enum", func() {
		m := newMachine()
		m.Spec.AfterDeploy = "Suspend"
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})

	It("defaults afterDeploy to Reboot when absent", func() {
		m := newMachine()
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(m.Spec.AfterDeploy).To(Equal(keziov1alpha2.AfterDeployReboot))
	})

	It("admits an absent bootMACAddress when inspect-disable is absent", func() {
		m := newMachine()
		m.Spec.BootMACAddress = ""
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
	})

	It("rejects an absent bootMACAddress when inspect-disable is \"true\"", func() {
		m := newMachine()
		m.Spec.BootMACAddress = ""
		m.Annotations = map[string]string{keziov1alpha2.MachineAnnotationInspectDisable: "true"}
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})
})
