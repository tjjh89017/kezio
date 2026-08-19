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
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha2.DeployRun{}).
		Build()
}

func newTestMachine() *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default", UID: types.UID("uid-m1")},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "m1-bmc"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:ff",
			SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
		},
	}
}

func newTestDeployRun(t *testing.T, c client.Client, name string, machine *keziov1alpha2.Machine) *keziov1alpha2.DeployRun {
	t.Helper()
	run := &keziov1alpha2.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha2.DeployRunSpec{
			MachineRef: keziov1alpha2.NameRef{Name: machine.Name},
		},
	}
	if err := c.Create(context.Background(), run); err != nil {
		t.Fatalf("Create(DeployRun) error = %v", err)
	}
	return run
}

func TestFakeDeployerInspectWritesSyntheticMachineHardware(t *testing.T) {
	c := newFakeClient(t)
	machine := newTestMachine()
	f := &FakeDeployer{Client: c}

	result, err := f.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Inspect() outcome = %v, want Complete", result.Outcome)
	}

	var hw keziov1alpha2.MachineHardware
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "m1"}, &hw); err != nil {
		t.Fatalf("Get(MachineHardware) error = %v", err)
	}
	if len(hw.Spec.Disks) == 0 {
		t.Error("MachineHardware.Spec.Disks is empty, want at least one fabricated disk")
	}
	if len(hw.Spec.Nics) != 1 || hw.Spec.Nics[0].MACAddress != machine.Spec.BootMACAddress {
		t.Errorf("MachineHardware.Spec.Nics = %+v, want one NIC matching BootMACAddress %q", hw.Spec.Nics, machine.Spec.BootMACAddress)
	}
	if len(hw.OwnerReferences) != 1 || hw.OwnerReferences[0].Name != machine.Name || hw.OwnerReferences[0].UID != machine.UID {
		t.Errorf("MachineHardware.OwnerReferences = %+v, want one owner reference to %q/%q", hw.OwnerReferences, machine.Name, machine.UID)
	}
}

func TestFakeDeployerInspectIsIdempotent(t *testing.T) {
	c := newFakeClient(t)
	machine := newTestMachine()
	f := &FakeDeployer{Client: c}

	if _, err := f.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("first Inspect() error = %v", err)
	}
	result, err := f.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("second Inspect() outcome = %v, want Complete", result.Outcome)
	}
}

func TestFakeDeployerProvisionWalksPhasesToSucceeded(t *testing.T) {
	c := newFakeClient(t)
	machine := newTestMachine()
	run := newTestDeployRun(t, c, "m1-run1", machine)
	f := &FakeDeployer{Client: c}

	wantPhases := []string{
		keziov1alpha2.DeployRunPhasePending,
		keziov1alpha2.DeployRunPhasePartitioning,
		keziov1alpha2.DeployRunPhaseWritingContent,
		keziov1alpha2.DeployRunPhaseRunningPostHook,
		keziov1alpha2.DeployRunPhaseFinalizing,
		keziov1alpha2.DeployRunPhaseSucceeded,
	}

	for i, wantPhase := range wantPhases {
		result, err := f.Provision(context.Background(), machine, run, false)
		if err != nil {
			t.Fatalf("Provision() call %d error = %v", i, err)
		}
		if run.Status.Phase != wantPhase {
			t.Fatalf("Provision() call %d phase = %q, want %q", i, run.Status.Phase, wantPhase)
		}

		last := len(wantPhases) - 1
		if i == last {
			if result.Outcome != Complete {
				t.Errorf("final Provision() outcome = %v, want Complete", result.Outcome)
			}
		} else if result.Outcome != Continuing {
			t.Errorf("Provision() call %d outcome = %v, want Continuing", i, result.Outcome)
		}
	}

	if len(run.Status.PhaseTimings) != len(wantPhases) {
		t.Fatalf("len(PhaseTimings) = %d, want %d", len(run.Status.PhaseTimings), len(wantPhases))
	}
	for i, timing := range run.Status.PhaseTimings {
		if timing.Phase != wantPhases[i] {
			t.Errorf("PhaseTimings[%d].Phase = %q, want %q", i, timing.Phase, wantPhases[i])
		}
		if i < len(run.Status.PhaseTimings)-1 && timing.FinishedAt == nil {
			t.Errorf("PhaseTimings[%d].FinishedAt is nil, want it closed once the next phase starts", i)
		}
	}
	if last := run.Status.PhaseTimings[len(run.Status.PhaseTimings)-1]; last.FinishedAt == nil {
		t.Error("final PhaseTimings entry has nil FinishedAt, want it closed on Complete")
	}

	found := false
	for _, cond := range run.Status.Conditions {
		if cond.Type == keziov1alpha2.DeployRunConditionSucceeded {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("Succeeded condition status = %q, want %q", cond.Status, metav1.ConditionTrue)
			}
		}
	}
	if !found {
		t.Error("DeployRun.Status.Conditions has no Succeeded condition after the run completed")
	}
}

func TestFakeDeployerProvisionRejectsCallAfterTerminal(t *testing.T) {
	c := newFakeClient(t)
	machine := newTestMachine()
	run := newTestDeployRun(t, c, "m1-run1", machine)
	f := &FakeDeployer{Client: c}

	for i := 0; i < 6; i++ {
		if _, err := f.Provision(context.Background(), machine, run, false); err != nil {
			t.Fatalf("Provision() call %d error = %v", i, err)
		}
	}

	if _, err := f.Provision(context.Background(), machine, run, false); err == nil {
		t.Fatal("Provision() after the run reached Succeeded returned no error, want a transient error")
	}
}

func TestFakeDeployerInspectFuncOverride(t *testing.T) {
	machine := newTestMachine()
	wantErr := errors.New("simulated transient failure")
	f := &FakeDeployer{
		InspectFunc: func(_ context.Context, m *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
			if m.Name != machine.Name {
				t.Errorf("InspectFunc received machine %q, want %q", m.Name, machine.Name)
			}
			if !restartOnFailure {
				t.Error("InspectFunc received restartOnFailure = false, want true")
			}
			return Result{}, wantErr
		},
	}

	_, err := f.Inspect(context.Background(), machine, true)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Inspect() error = %v, want %v", err, wantErr)
	}
}

func TestFakeDeployerProvisionFuncOverrideCanScriptEveryOutcome(t *testing.T) {
	machine := newTestMachine()
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}

	cases := []struct {
		name   string
		result Result
		err    error
	}{
		{"complete", Result{Outcome: Complete}, nil},
		{"continuing", Result{Outcome: Continuing}, nil},
		{"busy", Result{Outcome: Busy, RequeueAfter: 5 * time.Second}, nil},
		{"failed", Result{Outcome: Failed, ErrorType: keziov1alpha2.MachineErrorTypeRestart, ErrorMessage: "boom"}, nil},
		{"transient", Result{}, errors.New("simulated transient failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &FakeDeployer{
				ProvisionFunc: func(_ context.Context, _ *keziov1alpha2.Machine, _ *keziov1alpha2.DeployRun, restartOnFailure bool) (Result, error) {
					if !restartOnFailure {
						t.Error("ProvisionFunc received restartOnFailure = false, want true")
					}
					return tc.result, tc.err
				},
			}
			result, err := f.Provision(context.Background(), machine, run, true)
			if !errors.Is(err, tc.err) && (err == nil) != (tc.err == nil) {
				t.Fatalf("Provision() error = %v, want %v", err, tc.err)
			}
			if result != tc.result {
				t.Fatalf("Provision() result = %+v, want %+v", result, tc.result)
			}
		})
	}
}

func TestFakeDeployerDeprovisionDefaultsToInstantComplete(t *testing.T) {
	machine := newTestMachine()
	f := &FakeDeployer{}

	result, err := f.Deprovision(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Deprovision() outcome = %v, want Complete", result.Outcome)
	}
}

func TestFakeDeployerPowerOffDefaultsToInstantComplete(t *testing.T) {
	machine := newTestMachine()
	f := &FakeDeployer{}

	result, err := f.PowerOff(context.Background(), machine)
	if err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("PowerOff() outcome = %v, want Complete", result.Outcome)
	}
}

func TestFakeDeployerDeprovisionFuncOverride(t *testing.T) {
	machine := newTestMachine()
	wantErr := errors.New("simulated transient failure")
	f := &FakeDeployer{
		DeprovisionFunc: func(_ context.Context, m *keziov1alpha2.Machine, restartOnFailure bool) (Result, error) {
			if m.Name != machine.Name {
				t.Errorf("DeprovisionFunc received machine %q, want %q", m.Name, machine.Name)
			}
			if !restartOnFailure {
				t.Error("DeprovisionFunc received restartOnFailure = false, want true")
			}
			return Result{}, wantErr
		},
	}

	_, err := f.Deprovision(context.Background(), machine, true)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Deprovision() error = %v, want %v", err, wantErr)
	}
}

func TestFakeDeployerPowerOffFuncOverride(t *testing.T) {
	machine := newTestMachine()
	wantErr := errors.New("simulated transient failure")
	f := &FakeDeployer{
		PowerOffFunc: func(_ context.Context, m *keziov1alpha2.Machine) (Result, error) {
			if m.Name != machine.Name {
				t.Errorf("PowerOffFunc received machine %q, want %q", m.Name, machine.Name)
			}
			return Result{}, wantErr
		},
	}

	_, err := f.PowerOff(context.Background(), machine)
	if !errors.Is(err, wantErr) {
		t.Fatalf("PowerOff() error = %v, want %v", err, wantErr)
	}
}

func TestFakeDeployerRebootDefaultsToInstantComplete(t *testing.T) {
	machine := newTestMachine()
	f := &FakeDeployer{}

	result, err := f.Reboot(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("Reboot() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Reboot() outcome = %v, want Complete", result.Outcome)
	}
}

func TestFakeDeployerRebootFuncOverride(t *testing.T) {
	machine := newTestMachine()
	wantErr := errors.New("simulated transient failure")
	var gotHard bool
	f := &FakeDeployer{
		RebootFunc: func(_ context.Context, m *keziov1alpha2.Machine, hard bool) (Result, error) {
			if m.Name != machine.Name {
				t.Errorf("RebootFunc received machine %q, want %q", m.Name, machine.Name)
			}
			gotHard = hard
			return Result{}, wantErr
		},
	}

	_, err := f.Reboot(context.Background(), machine, true)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Reboot() error = %v, want %v", err, wantErr)
	}
	if !gotHard {
		t.Error("RebootFunc received hard = false, want true")
	}
}

// TestFakeGoSourceDoesNotImportBMC guards the doc comment on FakeDeployer:
// this stage's fast lane must never dial a BMC, and this parses fake.go's
// own import list rather than trusting the comment to stay accurate.
func TestFakeGoSourceDoesNotImportBMC(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report this test file's path")
	}
	fakeGoPath := filepath.Join(filepath.Dir(thisFile), "fake.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fakeGoPath, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", fakeGoPath, err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, "/internal/bmc") {
			t.Fatalf("fake.go imports %q; the fake deployer must never dial a BMC", path)
		}
	}
}

// TestFakeDeployerInspectIgnoresUnresolvedReferences proves the fake path
// skips reference resolution: subnetRef, postHookRefs, and imageRef name
// objects of kinds that do not exist yet, and Inspect must complete without
// ever trying to read them.
func TestFakeDeployerInspectIgnoresUnresolvedReferences(t *testing.T) {
	c := newFakeClient(t)
	machine := newTestMachine()
	machine.Spec.SubnetRef = keziov1alpha2.NameRef{Name: "no-such-subnet"}
	machine.Spec.PostHookRefs = []keziov1alpha2.NameRef{{Name: "no-such-hook"}}
	imageRef := keziov1alpha2.NameRef{Name: "no-such-image"}
	machine.Spec.ImageRef = &imageRef
	f := &FakeDeployer{Client: c}

	result, err := f.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v, want nil: the fake deployer must not resolve subnetRef/postHookRefs/imageRef", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Inspect() outcome = %v, want Complete", result.Outcome)
	}
}
