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

// These specs exercise the CRD schema (kubebuilder markers and CEL rules)
// through the real envtest apiserver, not the Go type definitions: OpenAPI
// and CEL validation only run there.
var _ = Describe("Machine CRD schema", func() {
	var machineCount int

	newMachine := func() *keziov1alpha3.Machine {
		machineCount++
		return &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-%d", machineCount),
				Namespace: "default",
			},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:01",
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
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

	It("admits an absent bootMACAddress when inspect-disable is absent", func() {
		m := newMachine()
		m.Spec.BootMACAddress = ""
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
	})

	It("rejects an absent bootMACAddress when inspect-disable is \"true\"", func() {
		m := newMachine()
		m.Spec.BootMACAddress = ""
		m.Annotations = map[string]string{keziov1alpha3.MachineAnnotationInspectDisable: "true"}
		Expect(k8sClient.Create(ctx, m)).To(HaveOccurred())
	})
})
