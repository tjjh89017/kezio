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
const (
	subnetSchemaTestNotAnIP    = "not-an-ip"
	subnetSchemaTestLeaseStart = "192.0.2.50"
	subnetSchemaTestLeaseEnd   = "192.0.2.100"
)

var _ = Describe("Subnet CRD schema", func() {
	var subnetCount int

	newSubnet := func() *keziov1alpha3.Subnet {
		subnetCount++
		return &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-subnet-%d", subnetCount),
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
	}

	// Lease mode requires a gateway, so every lease-mode fixture names
	// one. The empty string - this segment has no exit - keeps each spec
	// about the rule it is named for rather than about routing.
	newLeaseSubnet := func() *keziov1alpha3.Subnet {
		s := newSubnet()
		s.Spec.DHCP.Mode = keziov1alpha3.SubnetDHCPModeLease
		s.Spec.DHCP.Gateway = ptr.To("")
		return s
	}

	It("admits a minimal valid Subnet with a full boot half", func() {
		s := newSubnet()
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	})

	It("rejects a cidr that is not a CIDR", func() {
		s := newSubnet()
		s.Spec.CIDR = "not-a-cidr"
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a bootdServerIP that is not an IPv4 address", func() {
		s := newSubnet()
		s.Spec.BootdServerIP = subnetSchemaTestNotAnIP
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a dhcp.mode value outside the enum", func() {
		s := newSubnet()
		s.Spec.DHCP.Mode = "static"
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a leaseRangeStart that is not an IPv4 address", func() {
		s := newLeaseSubnet()
		s.Spec.DHCP.LeaseRangeStart = subnetSchemaTestNotAnIP
		s.Spec.DHCP.LeaseRangeEnd = subnetSchemaTestLeaseEnd
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a leaseRangeEnd that is not an IPv4 address", func() {
		s := newLeaseSubnet()
		s.Spec.DHCP.LeaseRangeStart = subnetSchemaTestLeaseStart
		s.Spec.DHCP.LeaseRangeEnd = subnetSchemaTestNotAnIP
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a missing siteRef", func() {
		s := newSubnet()
		s.Spec.SiteRef = keziov1alpha3.NameRef{}
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a missing cidr", func() {
		s := newSubnet()
		s.Spec.CIDR = ""
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects bootdServerIP left unset while bootdNetworkRef and dhcp are set (half-declared boot half)", func() {
		s := newSubnet()
		s.Spec.BootdServerIP = ""
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects bootdNetworkRef left unset while bootdServerIP and dhcp are set (half-declared boot half)", func() {
		s := newSubnet()
		s.Spec.BootdNetworkRef = nil
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects dhcp left unset while bootdServerIP and bootdNetworkRef are set (half-declared boot half)", func() {
		s := newSubnet()
		s.Spec.DHCP = nil
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a leaseRangeStart set with no leaseRangeEnd", func() {
		s := newLeaseSubnet()
		s.Spec.DHCP.LeaseRangeStart = subnetSchemaTestLeaseStart
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("rejects a leaseRangeEnd set with no leaseRangeStart", func() {
		s := newLeaseSubnet()
		s.Spec.DHCP.LeaseRangeEnd = subnetSchemaTestLeaseEnd
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})

	It("admits both leaseRangeStart and leaseRangeEnd set together", func() {
		s := newLeaseSubnet()
		s.Spec.DHCP.LeaseRangeStart = subnetSchemaTestLeaseStart
		s.Spec.DHCP.LeaseRangeEnd = subnetSchemaTestLeaseEnd
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	})

	// dhcp.gateway is the router option bootd hands out when bootd is
	// itself the DHCP server. Lease mode must say something; proxy mode,
	// where bootd is not the DHCP server, must not claim what it cannot
	// deliver.
	DescribeTable("admits dhcp.gateway according to dhcp.mode",
		func(mode string, gateway *string, admitted bool, because string) {
			s := newSubnet()
			s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{Mode: mode, Gateway: gateway}
			err := k8sClient.Create(ctx, s)
			if admitted {
				Expect(err).NotTo(HaveOccurred(), because)
				return
			}
			Expect(err).To(HaveOccurred(), because)
			Expect(err.Error()).To(ContainSubstring("gateway"))
		},
		Entry("lease mode naming the segment's router",
			keziov1alpha3.SubnetDHCPModeLease, ptr.To("192.0.2.1"), true,
			"an address is what a machine needs to leave the segment"),
		Entry("lease mode with an empty gateway, a segment with no exit",
			keziov1alpha3.SubnetDHCPModeLease, ptr.To(""), true,
			"the empty string is a decision, not an omission"),
		Entry("lease mode with no gateway at all",
			keziov1alpha3.SubnetDHCPModeLease, nil, false,
			"an absent gateway would leave dnsmasq advertising bootd, which forwards nothing"),
		Entry("lease mode with a gateway that is not an IPv4 address",
			keziov1alpha3.SubnetDHCPModeLease, ptr.To(subnetSchemaTestNotAnIP), false,
			"the pattern admits an IPv4 address or the empty string, nothing else"),
		Entry("proxy mode with no gateway",
			keziov1alpha3.SubnetDHCPModeProxy, nil, true,
			"proxy mode asserts nothing about routing, so there is nothing to be wrong about"),
		Entry("proxy mode with an empty gateway",
			keziov1alpha3.SubnetDHCPModeProxy, ptr.To(""), true,
			`a templated Subnet carrying gateway: "" must not need conditional templating to switch modes`),
		Entry("proxy mode naming an address",
			keziov1alpha3.SubnetDHCPModeProxy, ptr.To("192.0.2.1"), false,
			"bootd is not the DHCP server here; ignoring the address would mislead the operator"),
	)

	It("admits a Subnet with no boot half at all, given a seederNetworkRef", func() {
		s := newSubnet()
		s.Spec.BootdServerIP = ""
		s.Spec.BootdNetworkRef = nil
		s.Spec.DHCP = nil
		s.Spec.SeederNetworkRef = &keziov1alpha3.NameRef{Name: "seeder-nad"}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	})

	It("rejects a Subnet with neither a boot half nor a seederNetworkRef", func() {
		s := newSubnet()
		s.Spec.BootdServerIP = ""
		s.Spec.BootdNetworkRef = nil
		s.Spec.DHCP = nil
		Expect(k8sClient.Create(ctx, s)).To(HaveOccurred())
	})
})
