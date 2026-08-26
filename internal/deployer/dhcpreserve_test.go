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

package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

// getSubnet fetches the "default"/"default" Subnet newAgentTestClient
// helpers (agentTestProxySubnet/agentTestLeaseSubnet) always seed.
func getSubnet(t *testing.T, c client.Client) *keziov1alpha3.Subnet {
	t.Helper()
	var subnet keziov1alpha3.Subnet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "default"}, &subnet); err != nil {
		t.Fatalf("Get Subnet: %v", err)
	}
	return &subnet
}

// applyDHCPRevision simulates bootd acknowledging the Subnet's current
// DHCP revision: sets status.dhcp.appliedRevision to status.dhcp.revision.
func applyDHCPRevision(t *testing.T, c client.Client) {
	t.Helper()
	subnet := getSubnet(t, c)
	if subnet.Status.DHCP == nil {
		t.Fatal("Subnet has no status.dhcp to apply")
	}
	subnet.Status.DHCP.AppliedRevision = subnet.Status.DHCP.Revision
	if err := c.Status().Update(context.Background(), subnet); err != nil {
		t.Fatalf("applying DHCP revision: %v", err)
	}
}

func TestAgentDeployerReservesLowestFreeAddressAndWaitsForAck(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), agentTestLeaseSubnet())
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Busy {
		t.Fatalf("Inspect() outcome = %v, want Busy (waiting for bootd's ack)", result.Outcome)
	}
	if result.RequeueAfter != dhcpAckRequeueInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, dhcpAckRequeueInterval)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, powerCycle, _ := f.calls()
	if setPXE != 0 || powerOn != 0 || powerCycle != 0 {
		t.Errorf("BMC calls = setPXE:%d powerOn:%d powerCycle:%d, want 0/0/0 before the DHCP ack", setPXE, powerOn, powerCycle)
	}
	if _, armed := pxeArmedAt(machine); armed {
		t.Error("machine has a PXE-armed marker before the DHCP ack; must not be set until power is actually requested")
	}

	mac, _ := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	subnet := getSubnet(t, c)
	if subnet.Status.DHCP == nil || len(subnet.Status.DHCP.Reservations) != 1 {
		t.Fatalf("Subnet DHCP reservations = %+v, want exactly one", subnet.Status.DHCP)
	}
	res := subnet.Status.DHCP.Reservations[0]
	if res.Machine != machine.Name || res.MAC != mac || res.Address != "192.0.2.10" {
		t.Errorf("reservation = %+v, want {Machine:%q MAC:%q Address:192.0.2.10}", res, machine.Name, mac)
	}

	applyDHCPRevision(t, c)

	result2, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	if result2.Outcome != Continuing {
		t.Fatalf("second Inspect() outcome = %v, want Continuing once bootd has acked", result2.Outcome)
	}
	setPXE, powerOn, _, _, powerCycle, _ = f.calls()
	if setPXE != 1 || powerOn != 1 || powerCycle != 0 {
		t.Errorf("BMC calls after ack = setPXE:%d powerOn:%d powerCycle:%d, want exactly one power action (1/1/0)", setPXE, powerOn, powerCycle)
	}
	if _, armed := pxeArmedAt(machine); !armed {
		t.Error("machine has no PXE-armed marker after the ack and power-on")
	}

	// The reservation is reused (idempotent), not duplicated, across the
	// wait-for-ack polls.
	subnet = getSubnet(t, c)
	if len(subnet.Status.DHCP.Reservations) != 1 {
		t.Errorf("reservations after ack = %+v, want still exactly one (idempotent reuse)", subnet.Status.DHCP.Reservations)
	}
}

func TestAgentDeployerProxyModeSkipsReservationEntirely(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret()) // default proxy-mode Subnet
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing (proxy mode never waits on a reservation)", result.Outcome)
	}
	subnet := getSubnet(t, c)
	if subnet.Status.DHCP != nil {
		t.Errorf("Subnet.Status.DHCP = %+v, want nil for a proxy-mode Subnet", subnet.Status.DHCP)
	}
}

func TestAgentDeployerReservationPoolExhaustedReportsDelayedAndCondition(t *testing.T) {
	machine := agentTestMachine(t)
	leaseSubnet := agentTestLeaseSubnet() // exactly one address, 192.0.2.10
	leaseSubnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
		Reservations: []keziov1alpha3.DHCPReservation{
			{Machine: "someone-else", MAC: "aa:bb:cc:dd:ee:99", Address: "192.0.2.10"},
		},
		Revision: "already-full",
	}
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), leaseSubnet)
	recorder := record.NewFakeRecorder(4)
	d := &AgentDeployer{Client: c, Recorder: recorder}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Inspect() outcome = %v, want Delayed on pool exhaustion", result.Outcome)
	}

	subnet := getSubnet(t, c)
	found := false
	for _, cond := range subnet.Status.Conditions {
		if cond.Type == keziov1alpha3.SubnetConditionDHCPPoolExhausted {
			found = true
			if cond.Status != "True" {
				t.Errorf("DHCPPoolExhausted condition status = %v, want True", cond.Status)
			}
		}
	}
	if !found {
		t.Error("Subnet has no DHCPPoolExhausted condition after exhaustion")
	}
	if len(subnet.Status.DHCP.Reservations) != 1 {
		t.Errorf("reservations after exhaustion = %+v, want the pre-existing one untouched", subnet.Status.DHCP.Reservations)
	}

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "DHCPPoolExhausted") {
			t.Errorf("Event = %q, want it to mention DHCPPoolExhausted", ev)
		}
	default:
		t.Error("no Event recorded on pool exhaustion")
	}
}

// newAgentTestClientSubnetStatusConflictOnce is
// newAgentTestClientMachineStatusConflictOnce's Subnet counterpart: the
// first Subnet status subresource Update fails with an apiserver
// conflict, reproducing two Machines racing to reserve on the same
// Subnet. Every call after the first goes through unintercepted.
func newAgentTestClientSubnetStatusConflictOnce(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}

	conflicted := false
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !conflicted && subResourceName == "status" {
				if _, ok := obj.(*keziov1alpha3.Subnet); ok {
					conflicted = true
					return apierrors.NewConflict(schema.GroupResource{Group: keziov1alpha3.GroupVersion.Group, Resource: "subnets"}, obj.GetName(), errors.New("the object has been modified"))
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.DeployRun{}, &keziov1alpha3.Machine{}, &keziov1alpha3.Subnet{}).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
}

func TestAgentDeployerReservationRetriesSelectionOnConflict(t *testing.T) {
	machine := agentTestMachine(t)
	leaseSubnet := agentTestLeaseSubnet()
	leaseSubnet.Spec.DHCP.LeaseRangeEnd = "192.0.2.11" // room for two addresses
	c := newAgentTestClientSubnetStatusConflictOnce(t, machine, agentTestBMCSecret(), leaseSubnet)
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Busy {
		t.Fatalf("Inspect() outcome = %v, want Busy", result.Outcome)
	}

	subnet := getSubnet(t, c)
	if subnet.Status.DHCP == nil || len(subnet.Status.DHCP.Reservations) != 1 {
		t.Fatalf("reservations after a retried conflict = %+v, want exactly one persisted", subnet.Status.DHCP)
	}
	if subnet.Status.DHCP.Reservations[0].Address != "192.0.2.10" {
		t.Errorf("reserved address = %q, want the lowest free address despite the retried conflict", subnet.Status.DHCP.Reservations[0].Address)
	}
}

func TestAgentDeployerReleasesReservationOnInspectComplete(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), agentTestLeaseSubnet())
	d := &AgentDeployer{Client: c}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("first Inspect() error = %v", err)
	}
	applyDHCPRevision(t, c)
	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	if subnet := getSubnet(t, c); len(subnet.Status.DHCP.Reservations) != 1 {
		t.Fatalf("reservations before completion = %+v, want exactly one", subnet.Status.DHCP.Reservations)
	}

	setAgentRegistered(machine)
	hw := &keziov1alpha3.MachineHardware{}
	hw.Name, hw.Namespace = machine.Name, machine.Namespace
	if err := c.Create(context.Background(), hw); err != nil {
		t.Fatalf("creating MachineHardware: %v", err)
	}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("third Inspect() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("third Inspect() outcome = %v, want Complete", result.Outcome)
	}

	subnet := getSubnet(t, c)
	if subnet.Status.DHCP == nil || len(subnet.Status.DHCP.Reservations) != 0 {
		t.Errorf("reservations after Complete = %+v, want empty (released)", subnet.Status.DHCP)
	}
}

func TestAgentDeployerDeprovisionReleasesReservation(t *testing.T) {
	machine := agentTestMachine(t)
	leaseSubnet := agentTestLeaseSubnet()
	leaseSubnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
		Reservations: []keziov1alpha3.DHCPReservation{
			{Machine: machine.Name, MAC: "aa:bb:cc:dd:ee:ff", Address: "192.0.2.10"},
		},
		Revision: "rev-1",
	}
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), leaseSubnet)
	d := &AgentDeployer{Client: c}

	result, err := d.Deprovision(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Deprovision() outcome = %v, want Complete", result.Outcome)
	}

	subnet := getSubnet(t, c)
	if len(subnet.Status.DHCP.Reservations) != 0 {
		t.Errorf("reservations after Deprovision = %+v, want empty (released)", subnet.Status.DHCP.Reservations)
	}
}
