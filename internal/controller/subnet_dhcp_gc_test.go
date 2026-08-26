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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

var _ = Describe("Subnet DHCP reservation garbage collection", func() {
	var count int

	newLeaseSubnet := func() *keziov1alpha3.Subnet {
		count++
		gateway := "192.0.2.1"
		return &keziov1alpha3.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("dhcp-gc-subnet-%d", count), Namespace: "default"},
			Spec: keziov1alpha3.SubnetSpec{
				SiteRef:         keziov1alpha3.NameRef{Name: "site-a"},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: &keziov1alpha3.NameRef{Name: "bootd-net"},
				DHCP: &keziov1alpha3.SubnetDHCP{
					Mode:            keziov1alpha3.SubnetDHCPModeLease,
					Gateway:         &gateway,
					LeaseRangeStart: "192.0.2.10",
					LeaseRangeEnd:   "192.0.2.20",
				},
			},
		}
	}

	newGCMachine := func(name, subnetName, mac string) *keziov1alpha3.Machine {
		return &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              "redfish://" + name,
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: name + "-bmc"},
				},
				BootMACAddress: mac,
				SubnetRef:      keziov1alpha3.NameRef{Name: subnetName},
			},
		}
	}

	reconcileOnce := func(subnet *keziov1alpha3.Subnet) {
		reconciler := &SubnetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(subnet)})
		Expect(err).NotTo(HaveOccurred())
	}

	It("keeps a reservation whose Machine still matches MAC and subnetRef", func() {
		ctx := context.Background()
		subnet := newLeaseSubnet()
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		machine := newGCMachine("dhcp-gc-keep", subnet.Name, "aa:bb:cc:dd:ee:01")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: machine.Name, MAC: "aa:bb:cc:dd:ee:01", Address: "192.0.2.10", Since: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		reconcileOnce(subnet)

		var got keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &got)).To(Succeed())
		Expect(got.Status.DHCP.Reservations).To(HaveLen(1))
	})

	It("drops a reservation whose Machine no longer exists", func() {
		ctx := context.Background()
		subnet := newLeaseSubnet()
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: "never-existed", MAC: "aa:bb:cc:dd:ee:02", Address: "192.0.2.11", Since: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		reconcileOnce(subnet)

		var got keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &got)).To(Succeed())
		Expect(got.Status.DHCP.Reservations).To(BeEmpty())
	})

	It("drops a reservation whose Machine's MAC no longer matches", func() {
		ctx := context.Background()
		subnet := newLeaseSubnet()
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		machine := newGCMachine("dhcp-gc-mac-changed", subnet.Name, "aa:bb:cc:dd:ee:03")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: machine.Name, MAC: "aa:bb:cc:dd:ee:99", Address: "192.0.2.12", Since: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		reconcileOnce(subnet)

		var got keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &got)).To(Succeed())
		Expect(got.Status.DHCP.Reservations).To(BeEmpty())
	})

	It("drops a reservation whose Machine's subnetRef points elsewhere now", func() {
		ctx := context.Background()
		subnet := newLeaseSubnet()
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		otherSubnet := newLeaseSubnet()
		Expect(k8sClient.Create(ctx, otherSubnet)).To(Succeed())
		machine := newGCMachine("dhcp-gc-moved", otherSubnet.Name, "aa:bb:cc:dd:ee:04")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: machine.Name, MAC: "aa:bb:cc:dd:ee:04", Address: "192.0.2.13", Since: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		reconcileOnce(subnet)

		var got keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &got)).To(Succeed())
		Expect(got.Status.DHCP.Reservations).To(BeEmpty())
	})

	It("never touches reservations on a proxy-mode Subnet", func() {
		ctx := context.Background()
		subnet := newLeaseSubnet()
		subnet.Spec.DHCP.Mode = keziov1alpha3.SubnetDHCPModeProxy
		subnet.Spec.DHCP.Gateway = nil
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: "never-existed", MAC: "aa:bb:cc:dd:ee:05", Address: "192.0.2.14", Since: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		reconcileOnce(subnet)

		var got keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &got)).To(Succeed())
		Expect(got.Status.DHCP.Reservations).To(HaveLen(1))
	})
})
