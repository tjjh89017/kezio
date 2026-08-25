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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// CIDR/IP patterns and the boot-half grouping rules are CEL rules on the
// CRD schema and are covered in subnet_schema_test.go instead. This suite
// covers the one rule that needs a cross-object read: spec.siteRef.
var _ = Describe("Subnet Webhook", func() {
	Context("admission round-trip through the webhook server", func() {
		It("admits a valid Subnet", func() {
			created := &keziov1alpha3.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "subnet-admission-roundtrip",
					Namespace: "default",
				},
				Spec: keziov1alpha3.SubnetSpec{
					SiteRef:         keziov1alpha3.NameRef{Name: "site-a"},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
					DHCP: &keziov1alpha3.SubnetDHCP{
						Mode: keziov1alpha3.SubnetDHCPModeProxy,
					},
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})
	})

	Context("spec.siteRef", func() {
		var validator SubnetCustomValidator

		BeforeEach(func() {
			validator = SubnetCustomValidator{Client: k8sClient}
		})

		It("denies a siteRef naming a Site that does not exist", func() {
			subnet := &keziov1alpha3.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "subnet-dangling-siteref", Namespace: "default"},
				Spec: keziov1alpha3.SubnetSpec{
					SiteRef:         keziov1alpha3.NameRef{Name: "no-such-site"},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
					DHCP: &keziov1alpha3.SubnetDHCP{
						Mode: keziov1alpha3.SubnetDHCPModeProxy,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, subnet)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no-such-site"))
		})

		It("admits a siteRef naming a Site that exists", func() {
			subnet := &keziov1alpha3.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "subnet-resolving-siteref", Namespace: "default"},
				Spec: keziov1alpha3.SubnetSpec{
					SiteRef:         keziov1alpha3.NameRef{Name: "site-a"},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
					DHCP: &keziov1alpha3.SubnetDHCP{
						Mode: keziov1alpha3.SubnetDHCPModeProxy,
					},
				},
			}
			Expect(validator.ValidateCreate(ctx, subnet)).Error().NotTo(HaveOccurred())
		})

		It("checks the siteRef again on update", func() {
			subnet := &keziov1alpha3.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "subnet-siteref-update-check", Namespace: "default"},
				Spec: keziov1alpha3.SubnetSpec{
					SiteRef:         keziov1alpha3.NameRef{Name: "no-such-site"},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
					DHCP: &keziov1alpha3.SubnetDHCP{
						Mode: keziov1alpha3.SubnetDHCPModeProxy,
					},
				},
			}
			Expect(validator.ValidateUpdate(ctx, subnet, subnet)).Error().To(HaveOccurred())
		})
	})
})
