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
var _ = Describe("Site CRD schema", func() {
	var siteCount int

	newSite := func() *keziov1alpha3.Site {
		siteCount++
		return &keziov1alpha3.Site{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-site-%d", siteCount),
				Namespace: "default",
			},
		}
	}

	// newBackRefSubnet creates a Subnet whose spec.siteRef names siteName,
	// satisfying the Site webhook's cross-object back-reference rule so
	// these schema-focused specs can exercise a seeding Site without that
	// rule getting in the way. The named Site must already exist as a real
	// object, since the Subnet webhook now requires spec.siteRef to
	// resolve too.
	newBackRefSubnet := func(siteName string) *keziov1alpha3.Subnet {
		subnet := &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      siteName + "-subnet",
				Namespace: "default",
			},
			Spec: keziov1alpha3.SubnetSpec{
				SiteRef:         keziov1alpha3.NameRef{Name: siteName},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
				DHCP: &keziov1alpha3.SubnetDHCP{
					Mode: keziov1alpha3.SubnetDHCPModeProxy,
				},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		return subnet
	}

	It("admits a Site with no seederSubnetRef and no tracker", func() {
		s := newSite()
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	})

	It("admits a Site with a pinned tracker.ip", func() {
		s := newSite()
		// The Site must exist before its seeding Subnet can name it in
		// spec.siteRef, so it is created bare first and then updated with
		// the seeding fields once the Subnet exists.
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		subnet := newBackRefSubnet(s.Name)
		s.Spec.SeederSubnetRef = &keziov1alpha3.NameRef{Name: subnet.Name}
		s.Spec.Tracker.IP = "192.0.2.3"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	})

	It("admits a Site with a tracker.externalURL", func() {
		s := newSite()
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		subnet := newBackRefSubnet(s.Name)
		s.Spec.SeederSubnetRef = &keziov1alpha3.NameRef{Name: subnet.Name}
		s.Spec.Tracker.ExternalURL = "http://tracker.example.com:6969/announce"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	})

	It("rejects a tracker.ip that is not an IPv4 address", func() {
		s := newSite()
		s.Spec.SeederSubnetRef = &keziov1alpha3.NameRef{Name: "subnet-a"}
		s.Spec.Tracker.IP = "not-an-ip"
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects both tracker.ip and tracker.externalURL set together", func() {
		s := newSite()
		s.Spec.SeederSubnetRef = &keziov1alpha3.NameRef{Name: "subnet-a"}
		s.Spec.Tracker.IP = "192.0.2.3"
		s.Spec.Tracker.ExternalURL = "http://tracker.example.com:6969/announce"
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})
})
