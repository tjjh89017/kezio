/*
Copyright 2026 Date Huang.

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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func newTestMachine(namespace, name string) *keziov1alpha1.Machine {
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

// Drives a single machine through every phase in reconciler call order:
// Register, Inspect, Provision, Deprovision, plus PowerOn/PowerOff.
func TestFakeDeployerStateWalk(t *testing.T) {
	ctx := context.Background()
	factory := NewFactory()
	machine := newTestMachine("default", "node-01")

	d, err := factory.New(machine)
	if err != nil {
		t.Fatalf("factory.New() error = %v", err)
	}

	registerData := &RegisterData{BootMACAddress: "aa:bb:cc:dd:ee:02"}
	result, err := d.Register(ctx, registerData)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if !result.Dirty || result.ErrorMessage != "" {
		t.Errorf("Register() result = %+v, want Dirty=true, no error", result)
	}

	inspectData := &InspectData{}
	result, err = d.Inspect(ctx, inspectData)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !result.Dirty || result.ErrorMessage != "" {
		t.Errorf("Inspect() result = %+v, want Dirty=true, no error", result)
	}
	if inspectData.Hardware == nil {
		t.Fatal("Inspect() left data.Hardware nil, want fake inventory")
	}
	if len(inspectData.Hardware.Disks) == 0 {
		t.Error("Inspect() reported no disks, want at least one fake disk")
	}
	if len(inspectData.Hardware.Nics) != 1 || inspectData.Hardware.Nics[0].MACAddress != registerData.BootMACAddress {
		t.Errorf("Inspect() Nics = %+v, want one NIC with MAC %q", inspectData.Hardware.Nics, registerData.BootMACAddress)
	}
	if inspectData.Hardware.CPUCount == 0 || inspectData.Hardware.MemoryBytes == 0 {
		t.Errorf("Inspect() reported zero CPU or memory: %+v", inspectData.Hardware)
	}

	imageRef := &keziov1alpha1.NameRef{Name: "ubuntu-2404-golden"}
	provisionData := &ProvisionData{ImageRef: imageRef}
	result, err = d.Provision(ctx, provisionData)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !result.Dirty || result.ErrorMessage != "" || result.RequeueAfter != 0 {
		t.Errorf("Provision() result = %+v, want Dirty=true, RequeueAfter=0, no error", result)
	}

	result, err = d.Deprovision(ctx, &DeprovisionData{})
	if err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if !result.Dirty || result.ErrorMessage != "" {
		t.Errorf("Deprovision() result = %+v, want Dirty=true, no error", result)
	}

	if result, err = d.PowerOn(ctx); err != nil || !result.Dirty || result.ErrorMessage != "" {
		t.Errorf("PowerOn() = %+v, %v, want Dirty=true, no error", result, err)
	}
	if result, err = d.PowerOff(ctx); err != nil || !result.Dirty || result.ErrorMessage != "" {
		t.Errorf("PowerOff() = %+v, %v, want Dirty=true, no error", result, err)
	}
}

// Two machines from the same Factory get independent hardware inventories.
func TestFakeDeployerPerMachineState(t *testing.T) {
	ctx := context.Background()
	factory := NewFactory()

	dA, err := factory.New(newTestMachine("default", "node-a"))
	if err != nil {
		t.Fatalf("factory.New(node-a) error = %v", err)
	}
	dB, err := factory.New(newTestMachine("default", "node-b"))
	if err != nil {
		t.Fatalf("factory.New(node-b) error = %v", err)
	}

	if _, err := dA.Register(ctx, &RegisterData{BootMACAddress: "aa:bb:cc:dd:ee:0a"}); err != nil {
		t.Fatalf("Register(node-a) error = %v", err)
	}
	if _, err := dB.Register(ctx, &RegisterData{BootMACAddress: "aa:bb:cc:dd:ee:0b"}); err != nil {
		t.Fatalf("Register(node-b) error = %v", err)
	}

	dataA := &InspectData{}
	if _, err := dA.Inspect(ctx, dataA); err != nil {
		t.Fatalf("Inspect(node-a) error = %v", err)
	}
	dataB := &InspectData{}
	if _, err := dB.Inspect(ctx, dataB); err != nil {
		t.Fatalf("Inspect(node-b) error = %v", err)
	}

	if dataA.Hardware.Nics[0].MACAddress == dataB.Hardware.Nics[0].MACAddress {
		t.Error("node-a and node-b reported the same NIC MAC, want independent state")
	}
	if dataA.Hardware.Disks[0].SerialNumber == dataB.Hardware.Disks[0].SerialNumber {
		t.Error("node-a and node-b reported the same disk serial, want independent state")
	}
}

// A new Deployer built for the same machine (as happens every reconcile)
// still sees state an earlier one recorded.
func TestFakeDeployerStatePersistsAcrossNew(t *testing.T) {
	ctx := context.Background()
	factory := NewFactory()
	machine := newTestMachine("default", "node-01")

	first, err := factory.New(machine)
	if err != nil {
		t.Fatalf("factory.New() error = %v", err)
	}
	if _, err := first.Register(ctx, &RegisterData{BootMACAddress: "aa:bb:cc:dd:ee:03"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	second, err := factory.New(machine)
	if err != nil {
		t.Fatalf("factory.New() error = %v", err)
	}
	data := &InspectData{}
	if _, err := second.Inspect(ctx, data); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if data.Hardware.Nics[0].MACAddress != "aa:bb:cc:dd:ee:03" {
		t.Errorf("Inspect() MAC = %q, want the boot MAC recorded by a prior Register()", data.Hardware.Nics[0].MACAddress)
	}
}

// A FailFunc injects an error only for the targeted machine and phase.
func TestFakeDeployerErrorInjection(t *testing.T) {
	ctx := context.Background()
	failing := types.NamespacedName{Namespace: "default", Name: "node-fail"}
	const wantMsg = "injected inspect failure"

	factory := NewFactory()
	factory.Fail = func(machine types.NamespacedName, phase Phase) string {
		if machine == failing && phase == PhaseInspect {
			return wantMsg
		}
		return ""
	}

	failingDeployer, err := factory.New(newTestMachine(failing.Namespace, failing.Name))
	if err != nil {
		t.Fatalf("factory.New() error = %v", err)
	}
	okDeployer, err := factory.New(newTestMachine("default", "node-ok"))
	if err != nil {
		t.Fatalf("factory.New() error = %v", err)
	}

	// The targeted machine's Register (a different phase) is unaffected.
	if result, err := failingDeployer.Register(ctx, &RegisterData{}); err != nil || result.ErrorMessage != "" {
		t.Errorf("Register() on failing machine = %+v, %v, want no injected error", result, err)
	}

	data := &InspectData{}
	result, err := failingDeployer.Inspect(ctx, data)
	if err != nil {
		t.Fatalf("Inspect() unexpected Go error = %v", err)
	}
	if result.ErrorMessage != wantMsg {
		t.Errorf("Inspect() ErrorMessage = %q, want %q", result.ErrorMessage, wantMsg)
	}
	if data.Hardware != nil {
		t.Errorf("Inspect() set Hardware = %+v on a failed call, want nil", data.Hardware)
	}

	// A different machine's Inspect is unaffected.
	okData := &InspectData{}
	result, err = okDeployer.Inspect(ctx, okData)
	if err != nil {
		t.Fatalf("Inspect() on node-ok error = %v", err)
	}
	if result.ErrorMessage != "" {
		t.Errorf("Inspect() on node-ok ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	if okData.Hardware == nil {
		t.Error("Inspect() on node-ok left Hardware nil, want fake inventory")
	}
}
