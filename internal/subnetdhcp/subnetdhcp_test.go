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

package subnetdhcp

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

const testFirstLeaseAddress = "192.0.2.10"

func testSubnet() *keziov1alpha3.Subnet {
	gateway := "192.0.2.1"
	return &keziov1alpha3.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-1", Namespace: "site-hq"},
		Spec: keziov1alpha3.SubnetSpec{
			CIDR:          "192.0.2.0/24",
			BootdServerIP: "192.0.2.2",
			DHCP: &keziov1alpha3.SubnetDHCP{
				Mode:            keziov1alpha3.SubnetDHCPModeLease,
				Gateway:         &gateway,
				LeaseRangeStart: testFirstLeaseAddress,
				LeaseRangeEnd:   "192.0.2.12",
			},
		},
	}
}

func TestReserveLowestFree(t *testing.T) {
	subnet := testSubnet()
	addr, changed, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now())
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !changed {
		t.Fatal("Reserve() changed = false, want true for a fresh allocation")
	}
	if addr != testFirstLeaseAddress {
		t.Errorf("Reserve() address = %q, want the lowest free address 192.0.2.10", addr)
	}
	if subnet.Status.DHCP == nil || len(subnet.Status.DHCP.Reservations) != 1 {
		t.Fatalf("subnet.Status.DHCP.Reservations = %+v, want exactly one entry", subnet.Status.DHCP)
	}
	if subnet.Status.DHCP.Revision == "" {
		t.Error("subnet.Status.DHCP.Revision is empty after an allocation")
	}

	addr2, changed2, err := Reserve(subnet, "m2", "aa:bb:cc:dd:ee:02", metav1.Now())
	if err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	if !changed2 {
		t.Fatal("second Reserve() changed = false, want true")
	}
	if addr2 != "192.0.2.11" {
		t.Errorf("second Reserve() address = %q, want 192.0.2.11 (next free after 192.0.2.10)", addr2)
	}
}

func TestReserveIdempotentReuse(t *testing.T) {
	subnet := testSubnet()
	first, changed, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now())
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !changed {
		t.Fatal("first Reserve() changed = false")
	}
	revision := subnet.Status.DHCP.Revision

	second, changed2, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now())
	if err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	if changed2 {
		t.Error("second Reserve() for the same Machine changed = true, want false (idempotent reuse)")
	}
	if second != first {
		t.Errorf("second Reserve() address = %q, want the same address %q", second, first)
	}
	if subnet.Status.DHCP.Revision != revision {
		t.Error("Revision changed on a reuse that made no change")
	}
}

func TestReserveExhaustion(t *testing.T) {
	subnet := testSubnet()
	subnet.Spec.DHCP.LeaseRangeEnd = testFirstLeaseAddress // pool of exactly one address

	if _, _, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now()); err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}

	_, changed, err := Reserve(subnet, "m2", "aa:bb:cc:dd:ee:02", metav1.Now())
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Reserve() error = %v, want ErrPoolExhausted", err)
	}
	if changed {
		t.Error("Reserve() on exhaustion reported changed = true")
	}
	if len(subnet.Status.DHCP.Reservations) != 1 {
		t.Errorf("Reservations = %+v after exhaustion, want the one entry from before untouched", subnet.Status.DHCP.Reservations)
	}
}

func TestReserveExcludesBootdServerIPAndGateway(t *testing.T) {
	subnet := testSubnet()
	subnet.Spec.BootdServerIP = testFirstLeaseAddress
	gateway := "192.0.2.11"
	subnet.Spec.DHCP.Gateway = &gateway

	addr, _, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now())
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if addr != "192.0.2.12" {
		t.Errorf("Reserve() address = %q, want 192.0.2.12 (192.0.2.10/.11 reserved for bootdServerIP/gateway)", addr)
	}
}

func TestReserveEmptyGatewayIsNotReserved(t *testing.T) {
	subnet := testSubnet()
	empty := ""
	subnet.Spec.DHCP.Gateway = &empty
	subnet.Spec.DHCP.LeaseRangeEnd = testFirstLeaseAddress

	addr, _, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now())
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if addr != testFirstLeaseAddress {
		t.Errorf("Reserve() address = %q, want 192.0.2.10 (an empty gateway string reserves nothing)", addr)
	}
}

func TestRelease(t *testing.T) {
	subnet := testSubnet()
	if _, _, err := Reserve(subnet, "m1", "aa:bb:cc:dd:ee:01", metav1.Now()); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if _, _, err := Reserve(subnet, "m2", "aa:bb:cc:dd:ee:02", metav1.Now()); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	if changed := Release(subnet, "m1"); !changed {
		t.Fatal("Release() changed = false, want true for an existing entry")
	}
	if len(subnet.Status.DHCP.Reservations) != 1 || subnet.Status.DHCP.Reservations[0].Machine != "m2" {
		t.Errorf("Reservations after Release(m1) = %+v, want only m2", subnet.Status.DHCP.Reservations)
	}

	if changed := Release(subnet, "m1"); changed {
		t.Error("Release() on an already-released Machine changed = true, want false")
	}
	if changed := Release(subnet, "never-reserved"); changed {
		t.Error("Release() on a Machine with no reservation changed = true, want false")
	}
}

func TestReleaseNilDHCPStatus(t *testing.T) {
	subnet := testSubnet()
	if changed := Release(subnet, "m1"); changed {
		t.Error("Release() on a Subnet with no DHCP status changed = true, want false")
	}
}

func TestLeaseRangeAutoDerived(t *testing.T) {
	subnet := testSubnet()
	subnet.Spec.DHCP.LeaseRangeStart = ""
	subnet.Spec.DHCP.LeaseRangeEnd = ""

	start, end, err := LeaseRange(subnet)
	if err != nil {
		t.Fatalf("LeaseRange() error = %v", err)
	}
	if start.String() != "192.0.2.1" || end.String() != "192.0.2.254" {
		t.Errorf("LeaseRange() = %s-%s, want 192.0.2.1-192.0.2.254", start, end)
	}
}

func TestRevisionStableAndOrderIndependent(t *testing.T) {
	a := []keziov1alpha3.DHCPReservation{
		{Machine: "m1", MAC: "aa:bb:cc:dd:ee:01", Address: testFirstLeaseAddress},
		{Machine: "m2", MAC: "aa:bb:cc:dd:ee:02", Address: "192.0.2.11"},
	}
	b := []keziov1alpha3.DHCPReservation{a[1], a[0]}
	if Revision(a) != Revision(b) {
		t.Error("Revision depends on slice order, want order-independent")
	}
	if Revision(nil) != "" {
		t.Errorf("Revision(nil) = %q, want empty string", Revision(nil))
	}

	c := append([]keziov1alpha3.DHCPReservation{}, a...)
	c[0].Address = "192.0.2.99"
	if Revision(a) == Revision(c) {
		t.Error("Revision did not change when a reservation's address changed")
	}
}
