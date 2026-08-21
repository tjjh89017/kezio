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

// Subnet's validator is a deliberate no-op (see subnet_webhook.go), so this
// suite only proves the webhook is registered and serves admission
// requests; CRD schema/CEL rules belong in internal/controller's envtest
// suite instead.
var _ = Describe("Subnet Webhook", func() {
	Context("admission round-trip through the webhook server", func() {
		It("admits a valid Subnet", func() {
			created := &keziov1alpha2.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "subnet-admission-roundtrip",
					Namespace: "default",
				},
				Spec: keziov1alpha2.SubnetSpec{
					SiteRef:         keziov1alpha2.NameRef{Name: "site-a"},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: keziov1alpha2.NameRef{Name: "bootd-net"},
					DHCP: keziov1alpha2.SubnetDHCP{
						Mode: keziov1alpha2.SubnetDHCPModeProxy,
					},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})
	})
})
