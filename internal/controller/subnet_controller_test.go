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

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

var _ = Describe("Subnet Controller", func() {
	var subnetCount int

	newSubnet := func() *keziov1alpha2.Subnet {
		subnetCount++
		return &keziov1alpha2.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("subnet-%d", subnetCount),
				Namespace: "default",
			},
			Spec: keziov1alpha2.SubnetSpec{
				SiteRef:         keziov1alpha2.NameRef{Name: "site-a"},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: &keziov1alpha2.NameRef{Name: "bootd-net"},
				DHCP: &keziov1alpha2.SubnetDHCP{
					Mode: keziov1alpha2.SubnetDHCPModeProxy,
				},
			},
		}
	}

	It("admits a valid Subnet and reconciles without error", func() {
		ctx := context.Background()
		subnet := newSubnet()
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		reconciler := &SubnetReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(subnet),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects a lease range with only leaseRangeStart set", func() {
		ctx := context.Background()
		subnet := newSubnet()
		subnet.Spec.DHCP.LeaseRangeStart = "192.0.2.10"
		Expect(k8sClient.Create(ctx, subnet)).To(HaveOccurred())
	})

	It("rejects a lease range with only leaseRangeEnd set", func() {
		ctx := context.Background()
		subnet := newSubnet()
		subnet.Spec.DHCP.LeaseRangeEnd = "192.0.2.200"
		Expect(k8sClient.Create(ctx, subnet)).To(HaveOccurred())
	})

	It("admits a lease range with both leaseRangeStart and leaseRangeEnd set", func() {
		ctx := context.Background()
		subnet := newSubnet()
		subnet.Spec.DHCP.Mode = keziov1alpha2.SubnetDHCPModeLease
		subnet.Spec.DHCP.LeaseRangeStart = "192.0.2.10"
		subnet.Spec.DHCP.LeaseRangeEnd = "192.0.2.200"
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
	})
})
