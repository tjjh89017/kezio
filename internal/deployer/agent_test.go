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
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/bmc"
	"github.com/tjjh89017/kezio/internal/planbuild"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
)

// errTestBMCRejected is a stand-in for a BMC-reported rejection, used
// where a test only needs some non-nil error.
var errTestBMCRejected = errors.New("bmc: rejected")

// setAgentRegistered sets MachineConditionAgentRegistered=True on machine,
// the write internal/agentserver makes at successful registration.
func setAgentRegistered(machine *keziov1alpha2.Machine) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.MachineConditionAgentRegistered,
		Status:             metav1.ConditionTrue,
		Reason:             "AgentRegistered",
		Message:            "test fixture",
		ObservedGeneration: machine.Generation,
	})
}

const agentTestBMCSecretName = "m1-bmc"

// newAgentTestClient builds a fake client seeded with objs, with both the
// kezio and core (Secret) schemes registered.
func newAgentTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&keziov1alpha2.DeployRun{}).WithObjects(objs...).Build()
}

// agentTestBMCSecret builds the Secret agentTestBMCSecretName names, with
// the well-known username/password keys internal/bmc expects.
func agentTestBMCSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: agentTestBMCSecretName},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("s3cr3t"),
		},
	}
}

// agentTestMachine returns a Machine wired to a fake BMC address unique to
// t, with the credentials Secret it names.
func agentTestMachine(t *testing.T) *keziov1alpha2.Machine {
	m := newTestMachine()
	m.Spec.BMC = keziov1alpha2.MachineBMC{
		Address:              fakeBMCAddress(t),
		CredentialsSecretRef: keziov1alpha2.SecretReference{Name: agentTestBMCSecretName},
	}
	return m
}

func TestAgentDeployerInspectFirstPassArmsPXEAndPowersOn(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing", result.Outcome)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, powerCycle, getState := f.calls()
	if getState != 1 {
		t.Errorf("GetPowerState calls = %d, want 1", getState)
	}
	if setPXE != 1 {
		t.Errorf("SetOneTimePXEBoot calls = %d, want 1", setPXE)
	}
	if powerOn != 1 {
		t.Errorf("PowerOn calls = %d, want 1", powerOn)
	}
	if powerCycle != 0 {
		t.Errorf("PowerCycle calls = %d, want 0 (machine was off)", powerCycle)
	}

	armedAt, armed := pxeArmedAt(machine)
	if !armed {
		t.Fatal("machine has no PXE-armed annotation after the first Inspect pass")
	}
	if time.Since(armedAt) > time.Minute {
		t.Errorf("armed timestamp %v is not recent", armedAt)
	}
	if f.gotCreds.Username != "admin" || f.gotCreds.Password != "s3cr3t" {
		t.Errorf("gotCreds = %+v, want the resolved secret credentials", f.gotCreds)
	}
}

func TestAgentDeployerInspectFirstPassAlreadyOnPowerCycles(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}
	fakeBMCForAddress(machine.Spec.BMC.Address).state = bmc.PowerStateOn

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing", result.Outcome)
	}

	_, powerOn, _, _, powerCycle, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if powerCycle != 1 {
		t.Errorf("PowerCycle calls = %d, want 1", powerCycle)
	}
	if powerOn != 0 {
		t.Errorf("PowerOn calls = %d, want 0 (machine was already on; must PowerCycle, not PowerOn)", powerOn)
	}
}

func TestAgentDeployerInspectSecondPassWithoutRegistrationContinuesAndDoesNotReArm(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("first Inspect() error = %v", err)
	}
	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("second Inspect() outcome = %v, want Continuing", result.Outcome)
	}

	setPXE, powerOn, _, _, _, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1 (must not re-arm while already armed)", setPXE, powerOn)
	}
}

func TestAgentDeployerInspectRegisteredButHardwareMissingContinues(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("first Inspect() error = %v", err)
	}
	setAgentRegistered(machine)

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing (registered but no MachineHardware yet)", result.Outcome)
	}
}

func TestAgentDeployerInspectCompleteOnceRegisteredAndHardwarePresent(t *testing.T) {
	machine := agentTestMachine(t)
	hw := &keziov1alpha2.MachineHardware{ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), hw)
	d := &AgentDeployer{Client: c}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("first Inspect() error = %v", err)
	}
	setAgentRegistered(machine)

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Inspect() outcome = %v, want Complete", result.Outcome)
	}
	if _, armed := pxeArmedAt(machine); armed {
		t.Error("PXE-armed annotation still present after Inspect reported Complete, want it cleared")
	}
}

func TestAgentDeployerInspectDeadlineExceededFailsRestart(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	machine.Annotations = map[string]string{
		agentDeployerPXEArmedAnnotation: time.Now().Add(-2 * agentDeployerInspectDeadline).UTC().Format(time.RFC3339),
	}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Inspect() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha2.MachineErrorTypeRestart {
		t.Errorf("Inspect() ErrorType = %q, want %q", result.ErrorType, keziov1alpha2.MachineErrorTypeRestart)
	}
	if result.ErrorMessage == "" {
		t.Error("Inspect() ErrorMessage is empty, want an explanation")
	}
}

func TestAgentDeployerInspectRestartOnFailureReArms(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	machine.Annotations = map[string]string{
		agentDeployerPXEArmedAnnotation: time.Now().Add(-2 * agentDeployerInspectDeadline).UTC().Format(time.RFC3339),
	}

	result, err := d.Inspect(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing (restartOnFailure must re-arm, not report the stale deadline)", result.Outcome)
	}

	setPXE, powerOn, _, _, _, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1", setPXE, powerOn)
	}
	armedAt, armed := pxeArmedAt(machine)
	if !armed || time.Since(armedAt) > time.Minute {
		t.Errorf("PXE-armed annotation not refreshed to a recent timestamp: armedAt=%v armed=%v", armedAt, armed)
	}
}

func TestAgentDeployerInspectMissingCredentialsSecretFailsTransientWithoutLeakingCredentials(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.BMC.CredentialsSecretRef.Name = "does-not-exist"
	c := newAgentTestClient(t, machine)
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Inspect() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha2.MachineErrorTypeTransient {
		t.Errorf("Inspect() ErrorType = %q, want %q", result.ErrorType, keziov1alpha2.MachineErrorTypeTransient)
	}
	if strings.Contains(result.ErrorMessage, "s3cr3t") || strings.Contains(result.ErrorMessage, "admin") {
		t.Fatalf("Inspect() ErrorMessage leaked credential contents: %q", result.ErrorMessage)
	}
}

func TestAgentDeployerInspectNetworkUnreachableReportsDelayed(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.BMC.Address = fakeBMCDialErrScheme + "://10.0.0.10"
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Inspect() outcome = %v, want Delayed", result.Outcome)
	}
	if result.ErrorType != "" || result.ErrorMessage != "" {
		t.Errorf("Inspect() ErrorType/ErrorMessage = %q/%q, want both empty for Delayed", result.ErrorType, result.ErrorMessage)
	}
}

// agentProvisionTestManagerNamespace is the ManagerNamespace every
// Provision test's Builder is configured with, and where the fixture
// default PostHook is created.
const agentProvisionTestManagerNamespace = "kezio-system"

// mustCreateDefaultPostHook creates the shipped default PostHook in
// agentProvisionTestManagerNamespace, already Valid: resolveMachineHooks
// attaches it whenever machine.Spec.PostHookRefs is empty.
func mustCreateDefaultPostHook(t *testing.T, c client.Client) {
	t.Helper()
	ph := &keziov1alpha2.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: posthookdefaults.DefaultFinalizeHookName, Namespace: agentProvisionTestManagerNamespace},
		Spec:       posthookdefaults.Spec(),
	}
	apimeta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type: keziov1alpha2.PostHookConditionValid, Status: metav1.ConditionTrue, Reason: "TestFixture", Message: "fixture",
	})
	if err := c.Create(context.Background(), ph); err != nil {
		t.Fatalf("create default posthook: %v", err)
	}
}

// blankDataImage builds a Ready Image with an ESP slot and a mkfs data
// slot - no PartitionContent or seeder lookup needed, so it is cheap to
// resolve in a Provision test. The ESP slot is required for
// mustCreateDefaultPostHook's default hook (efibootmgr) to resolve its
// derived "part" parameter.
func blankDataImage(name, ns string) *keziov1alpha2.Image {
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: keziov1alpha2.ImageSpec{
			Layout: keziov1alpha2.ImageDiskLayout{
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				SfdiskJSON:     `{"partitiontable":{}}`,
				Slots: []keziov1alpha2.ImageSlot{
					{Number: 1, Role: keziov1alpha2.PartitionRoleESP, FSType: "vfat"},
					{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
				},
			},
		},
	}
	img.Status.State = keziov1alpha2.ImageStateReady
	return img
}

func TestAgentDeployerProvisionFirstPassNotReadyReportsDelayed(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.ImageRef = &keziov1alpha2.NameRef{Name: "img1"}
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Provision() outcome = %v, want Delayed (no MachineHardware yet)", result.Outcome)
	}
}

func TestAgentDeployerProvisionFirstPassValidationErrorReportsFailed(t *testing.T) {
	machine := agentTestMachine(t)
	// No ImageRef and no DataImages: Build rejects this before any lookup.
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha2.MachineErrorTypeTransient {
		t.Errorf("Provision() ErrorType = %q, want %q", result.ErrorType, keziov1alpha2.MachineErrorTypeTransient)
	}
	if result.ErrorMessage == "" {
		t.Error("Provision() ErrorMessage is empty, want an explanation")
	}
}

func TestAgentDeployerProvisionFirstPassSuccessRecordsPendingAndContinues(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.ImageRef = &keziov1alpha2.NameRef{Name: "img1"}
	hw := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace},
		Spec:       keziov1alpha2.MachineHardwareSpec{Disks: []keziov1alpha2.MachineHardwareDisk{{DeviceName: "/dev/vda", SizeBytes: 32 << 30}}},
	}
	img := blankDataImage("img1", machine.Namespace)
	c := newAgentTestClient(t, machine, agentTestBMCSecret(), hw, img)
	mustCreateDefaultPostHook(t, c)
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	run := newTestDeployRun(t, c, machine)

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}
	if run.Status.Phase != keziov1alpha2.DeployRunPhasePending {
		t.Fatalf("run.Status.Phase = %q, want %q", run.Status.Phase, keziov1alpha2.DeployRunPhasePending)
	}
	if len(run.Status.PhaseTimings) != 1 || run.Status.PhaseTimings[0].Phase != keziov1alpha2.DeployRunPhasePending {
		t.Fatalf("run.Status.PhaseTimings = %+v, want one Pending entry", run.Status.PhaseTimings)
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get DeployRun: %v", err)
	}
	if stored.Status.Phase != keziov1alpha2.DeployRunPhasePending {
		t.Fatalf("stored run.Status.Phase = %q, want %q", stored.Status.Phase, keziov1alpha2.DeployRunPhasePending)
	}
}

func TestAgentDeployerProvisionLaterPassSucceededReportsComplete(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha2.DeployRunPhaseSucceeded
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Provision() outcome = %v, want Complete", result.Outcome)
	}
}

func TestAgentDeployerProvisionLaterPassFailedReportsFailedWithMessage(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha2.DeployRunPhaseFailed
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: keziov1alpha2.DeployRunConditionSucceeded, Status: metav1.ConditionFalse, Reason: "DeployFailed", Message: "writing partition table: rejected",
	})
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorMessage != "writing partition table: rejected" {
		t.Errorf("Provision() ErrorMessage = %q, want the DeployRun's recorded failure message", result.ErrorMessage)
	}
}

func TestAgentDeployerProvisionLaterPassInProgressReportsContinuing(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha2.DeployRunPhaseWritingContent
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}
}

func TestAgentDeployerDeprovisionReportsDelayedUnsupported(t *testing.T) {
	machine := agentTestMachine(t)
	d := &AgentDeployer{Client: newAgentTestClient(t)}

	result, err := d.Deprovision(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Deprovision() outcome = %v, want Delayed", result.Outcome)
	}
	if result.ErrorMessage != agentDeployerUnsupportedMessage {
		t.Errorf("Deprovision() ErrorMessage = %q, want %q", result.ErrorMessage, agentDeployerUnsupportedMessage)
	}
}

func TestAgentDeployerRebootHardPowerCycles(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	result, err := d.Reboot(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("Reboot() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Reboot() outcome = %v, want Complete", result.Outcome)
	}
	_, _, _, _, powerCycle, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if powerCycle != 1 {
		t.Errorf("PowerCycle calls = %d, want 1", powerCycle)
	}
}

func TestAgentDeployerRebootSoftPowersOffThenOn(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}

	result, err := d.Reboot(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Reboot() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Reboot() outcome = %v, want Complete", result.Outcome)
	}
	_, powerOn, powerOff, _, powerCycle, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if powerOff != 1 || powerOn != 1 {
		t.Errorf("PowerOff/PowerOn calls = %d/%d, want 1/1", powerOff, powerOn)
	}
	if powerCycle != 0 {
		t.Errorf("PowerCycle calls = %d, want 0 for a soft reboot", powerCycle)
	}
}

func TestAgentDeployerRebootConnectErrorClassified(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.BMC.CredentialsSecretRef.Name = "does-not-exist"
	c := newAgentTestClient(t, machine)
	d := &AgentDeployer{Client: c}

	result, err := d.Reboot(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("Reboot() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Reboot() outcome = %v, want Failed", result.Outcome)
	}
}

func TestAgentDeployerPowerOffGracefulSucceedsWithoutForce(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}
	fakeBMCForAddress(machine.Spec.BMC.Address).state = bmc.PowerStateOn

	result, err := d.PowerOff(context.Background(), machine)
	if err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("PowerOff() outcome = %v, want Complete", result.Outcome)
	}
	_, _, powerOff, forcePowerOff, _, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if powerOff != 1 {
		t.Errorf("PowerOff calls = %d, want 1", powerOff)
	}
	if forcePowerOff != 0 {
		t.Errorf("ForcePowerOff calls = %d, want 0 (graceful power-off already took effect)", forcePowerOff)
	}
}

func TestAgentDeployerPowerOffEscalatesToForceWhenStillOn(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}
	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	f.state = bmc.PowerStateOn
	f.ignorePowerOff = true

	result, err := d.PowerOff(context.Background(), machine)
	if err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("PowerOff() outcome = %v, want Complete", result.Outcome)
	}
	_, _, powerOff, forcePowerOff, _, _ := f.calls()
	if powerOff != 1 || forcePowerOff != 1 {
		t.Errorf("PowerOff/ForcePowerOff calls = %d/%d, want 1/1 (graceful ignored, must escalate)", powerOff, forcePowerOff)
	}
}

func TestAgentDeployerPowerOffForcePowerOffFailureReportsFailed(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c}
	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	f.state = bmc.PowerStateOn
	f.ignorePowerOff = true
	f.forcePowerOffErr = errTestBMCRejected

	result, err := d.PowerOff(context.Background(), machine)
	if err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("PowerOff() outcome = %v, want Failed", result.Outcome)
	}
}
