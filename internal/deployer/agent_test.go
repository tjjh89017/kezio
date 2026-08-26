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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bmc"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/planbuild"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
)

// errTestBMCRejected is a stand-in for a BMC-reported rejection, used
// where a test only needs some non-nil error.
var errTestBMCRejected = errors.New("bmc: rejected")

// agentTestMissingSecretName names a credentials Secret that is never
// created, for tests exercising connectBMC's "Secret not found" path.
const agentTestMissingSecretName = "does-not-exist"

// setAgentRegistered sets MachineConditionAgentRegistered=True on machine,
// the write internal/agentserver makes at successful registration.
func setAgentRegistered(machine *keziov1alpha3.Machine) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha3.MachineConditionAgentRegistered,
		Status:             metav1.ConditionTrue,
		Reason:             "AgentRegistered",
		Message:            "test fixture",
		ObservedGeneration: machine.Generation,
	})
}

const agentTestBMCSecretName = "m1-bmc"

// newAgentTestClient builds a fake client seeded with objs, with both the
// kezio and core (Secret) schemes registered. A proxy-mode Subnet named
// "default" (newTestMachine's spec.subnetRef) is seeded automatically
// unless objs already names one: reserveAndAwaitDHCP resolves it on every
// arm call, and proxy mode is a no-op there, keeping every test that
// predates DHCP reservations unaffected.
func newAgentTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	hasSubnet := false
	for _, o := range objs {
		if _, ok := o.(*keziov1alpha3.Subnet); ok {
			hasSubnet = true
			break
		}
	}
	if !hasSubnet {
		objs = append(objs, agentTestProxySubnet())
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.DeployRun{}, &keziov1alpha3.Machine{}, &keziov1alpha3.Subnet{}).
		WithObjects(objs...).
		Build()
}

// agentTestProxySubnet is the default Subnet newAgentTestClient seeds:
// proxy mode, so reserveAndAwaitDHCP always proceeds with no reservation.
func agentTestProxySubnet() *keziov1alpha3.Subnet {
	return &keziov1alpha3.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: keziov1alpha3.SubnetSpec{
			SiteRef: keziov1alpha3.NameRef{Name: "site"},
			CIDR:    "192.0.2.0/24",
			DHCP:    &keziov1alpha3.SubnetDHCP{Mode: keziov1alpha3.SubnetDHCPModeProxy},
		},
	}
}

// agentTestLeaseSubnet is a lease-mode counterpart to agentTestProxySubnet,
// for tests exercising DHCP reservation allocation: a small range with
// room for exactly one address before exhaustion.
func agentTestLeaseSubnet() *keziov1alpha3.Subnet {
	gateway := "192.0.2.1"
	return &keziov1alpha3.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: keziov1alpha3.SubnetSpec{
			SiteRef: keziov1alpha3.NameRef{Name: "site"},
			CIDR:    "192.0.2.0/24",
			DHCP: &keziov1alpha3.SubnetDHCP{
				Mode:            keziov1alpha3.SubnetDHCPModeLease,
				Gateway:         &gateway,
				LeaseRangeStart: "192.0.2.10",
				LeaseRangeEnd:   "192.0.2.10",
			},
		},
	}
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
func agentTestMachine(t *testing.T) *keziov1alpha3.Machine {
	m := newTestMachine()
	m.Spec.BMC = keziov1alpha3.MachineBMC{
		Address:              fakeBMCAddress(t),
		CredentialsSecretRef: keziov1alpha3.SecretReference{Name: agentTestBMCSecretName},
	}
	return m
}

// agentTestClaimWithImage builds a MachineClaim naming imageName as its
// OS image and wires machine.spec.claimRef to it, the deploy intent's
// only home since it moved off Machine.spec. Include the returned claim
// in newAgentTestClient's objs so resolveClaimIntent can read it back.
func agentTestClaimWithImage(machine *keziov1alpha3.Machine, imageName string) *keziov1alpha3.MachineClaim {
	claim := &keziov1alpha3.MachineClaim{
		ObjectMeta: metav1.ObjectMeta{Name: machine.Name + "-claim", Namespace: machine.Namespace, UID: types.UID(machine.Name + "-claim-uid")},
		Spec: keziov1alpha3.MachineClaimSpec{
			MachineName: machine.Name,
			ImageRef:    &keziov1alpha3.NameRef{Name: imageName},
		},
	}
	machine.Spec.ClaimRef = &keziov1alpha3.MachineClaimReference{
		Name:      claim.Name,
		Namespace: claim.Namespace,
		UID:       claim.UID,
	}
	return claim
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

// TestAgentDeployerInspectFirstPassIssuesBootToken pins that arming a
// net boot mints its registration token right there, through Tokens -
// not on some later grub.cfg fetch (see bootserver.TokenStore's doc
// comment): the persisted hash must already resolve, via the same
// TokenStore, to a plaintext token by the time armPXEAndPowerOn returns.
func TestAgentDeployerInspectFirstPassIssuesBootToken(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{Client: c, Tokens: tokens}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}

	if machine.Status.NetBoot == nil || machine.Status.NetBoot.TokenHash == "" {
		t.Fatalf("machine.Status.NetBoot was not set by arming the boot: %+v", machine.Status.NetBoot)
	}
	mac, ok := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		t.Fatalf("test machine's boot MAC does not normalize: %q", machine.Spec.BootMACAddress)
	}
	if token, ok := tokens.Lookup(mac, machine.Status.NetBoot.TokenHash); !ok || token == "" {
		t.Fatalf("TokenStore has no plaintext token matching the persisted hash")
	}

	var stored keziov1alpha3.Machine
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(machine), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot == nil || stored.Status.NetBoot.TokenHash != machine.Status.NetBoot.TokenHash {
		t.Fatalf("boot token hash was not persisted to the API server: %+v", stored.Status.NetBoot)
	}
}

// TestAgentDeployerInspectSkipsMintingWithNoTokenStore pins that a nil
// Tokens field (this package's own tests that do not care about the boot
// token, and any Deployer not wired to a live bootserver) never blocks
// arming: the machine still gets PXE-armed and powered on with no
// status.netBoot written at all.
func TestAgentDeployerInspectSkipsMintingWithNoTokenStore(t *testing.T) {
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
	if machine.Status.NetBoot != nil {
		t.Fatalf("machine.Status.NetBoot = %+v, want nil with no TokenStore wired", machine.Status.NetBoot)
	}
}

// newAgentTestClientMachineStatusConflictOnce builds a fake client exactly
// like newAgentTestClient, except the first Machine status subresource
// Update call fails with an apiserver conflict - reproducing the observed
// production race (internal/agentserver writing status.agentSession to
// the same Machine at the exact moment issueBootToken persists the boot
// token). Every call after the first goes through unintercepted.
func newAgentTestClientMachineStatusConflictOnce(t *testing.T, objs ...client.Object) client.Client {
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
				if _, ok := obj.(*keziov1alpha3.Machine); ok {
					conflicted = true
					return apierrors.NewConflict(schema.GroupResource{Group: keziov1alpha3.GroupVersion.Group, Resource: "machines"}, obj.GetName(), errors.New("the object has been modified"))
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}

	hasSubnet := false
	for _, o := range objs {
		if _, ok := o.(*keziov1alpha3.Subnet); ok {
			hasSubnet = true
			break
		}
	}
	if !hasSubnet {
		objs = append(objs, agentTestProxySubnet())
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.DeployRun{}, &keziov1alpha3.Machine{}, &keziov1alpha3.Subnet{}).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
}

// TestAgentDeployerInspectFirstPassSurvivesStatusConflictAndPowersOnOnce
// pins the fix for the confirmed lab bug: an apiserver conflict on the
// boot token's status write (issueBootToken's Status().Update, racing
// internal/agentserver's own status.agentSession write) must not leave
// armPXEAndPowerOn returning an error after it already power-cycled the
// machine - persistence is retried to success, recorded, and the BMC is
// powered on exactly once.
func TestAgentDeployerInspectFirstPassSurvivesStatusConflictAndPowersOnOnce(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClientMachineStatusConflictOnce(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{Client: c, Tokens: tokens}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Inspect() outcome = %v, want Continuing", result.Outcome)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, powerCycle, _ := f.calls()
	if setPXE != 1 {
		t.Errorf("SetOneTimePXEBoot calls = %d, want 1", setPXE)
	}
	if powerOn != 1 || powerCycle != 0 {
		t.Errorf("PowerOn/PowerCycle calls = %d/%d, want 1/0 (must power on exactly once despite the retried conflict)", powerOn, powerCycle)
	}

	if _, armed := pxeArmedAt(machine); !armed {
		t.Error("machine has no PXE-armed annotation after a retried status conflict")
	}
	if machine.Status.NetBoot == nil || machine.Status.NetBoot.TokenHash == "" {
		t.Fatalf("machine.Status.NetBoot was not persisted after the retried conflict: %+v", machine.Status.NetBoot)
	}

	var stored keziov1alpha3.Machine
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(machine), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot == nil || stored.Status.NetBoot.TokenHash != machine.Status.NetBoot.TokenHash {
		t.Fatalf("boot token hash was not actually persisted to the API server: %+v", stored.Status.NetBoot)
	}
}

// TestAgentDeployerProvisionFirstPassSurvivesStatusConflictAndPowersOnOnce
// is TestAgentDeployerInspectFirstPassSurvivesStatusConflictAndPowersOnOnce
// for Provision's own boot-into-agent step (armProvisionBootAndPowerOn),
// which mints and persists its boot token/marker through the identical
// issueBootToken path.
func TestAgentDeployerProvisionFirstPassSurvivesStatusConflictAndPowersOnOnce(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClientMachineStatusConflictOnce(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{
		Client:      c,
		PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace},
		Tokens:      tokens,
	}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, powerCycle, _ := f.calls()
	if setPXE != 1 {
		t.Errorf("SetOneTimePXEBoot calls = %d, want 1", setPXE)
	}
	if powerOn != 1 || powerCycle != 0 {
		t.Errorf("PowerOn/PowerCycle calls = %d/%d, want 1/0 (must power on exactly once despite the retried conflict)", powerOn, powerCycle)
	}

	if _, armed := provisionBootMarker(machine); !armed {
		t.Error("machine has no provision-boot marker after a retried status conflict")
	}
	if machine.Status.NetBoot == nil || machine.Status.NetBoot.TokenHash == "" {
		t.Fatalf("machine.Status.NetBoot was not persisted after the retried conflict: %+v", machine.Status.NetBoot)
	}
}

func TestAgentDeployerConnectBMCInsecureSkipVerifyAnnotation(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "absent verifies", want: false},
		{name: "true skips verification", value: "true", set: true, want: true},
		{name: "false verifies", value: "false", set: true, want: false},
		// The webhook rejects this value, but a Machine that predates the
		// webhook (or bypasses it) must still verify rather than read as
		// enabled.
		{name: "near-miss verifies", value: "True", set: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := agentTestMachine(t)
			if tt.set {
				machine.Annotations = map[string]string{
					keziov1alpha3.MachineAnnotationBMCInsecureSkipVerify: tt.value,
				}
			}
			c := newAgentTestClient(t, machine, agentTestBMCSecret())
			d := &AgentDeployer{Client: c}

			if _, err := d.Inspect(context.Background(), machine, false); err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}

			f := fakeBMCForAddress(machine.Spec.BMC.Address)
			f.mu.Lock()
			got := f.gotOpts.InsecureSkipVerify
			f.mu.Unlock()
			if got != tt.want {
				t.Errorf("InsecureSkipVerify = %v, want %v", got, tt.want)
			}
		})
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
	hw := &keziov1alpha3.MachineHardware{ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace}}
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
	if result.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Errorf("Inspect() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeRestart)
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
	machine.Spec.BMC.CredentialsSecretRef.Name = agentTestMissingSecretName
	c := newAgentTestClient(t, machine)
	d := &AgentDeployer{Client: c}

	result, err := d.Inspect(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Inspect() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha3.MachineErrorTypeTransient {
		t.Errorf("Inspect() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeTransient)
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
	ph := &keziov1alpha3.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: posthookdefaults.DefaultFinalizeHookName, Namespace: agentProvisionTestManagerNamespace},
		Spec:       posthookdefaults.Spec(),
	}
	apimeta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type: keziov1alpha3.PostHookConditionValid, Status: metav1.ConditionTrue, Reason: "TestFixture", Message: "fixture",
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
func blankDataImage(name, ns string) *keziov1alpha3.Image {
	img := &keziov1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: keziov1alpha3.ImageSpec{
			Layout: keziov1alpha3.ImageDiskLayout{
				PartitionTable: keziov1alpha3.PartitionTableGPT,
				SfdiskJSON:     `{"partitiontable":{}}`,
				Slots: []keziov1alpha3.ImageSlot{
					{Number: 1, Role: keziov1alpha3.PartitionRoleESP, FSType: "vfat"},
					{Number: 2, Role: keziov1alpha3.PartitionRoleData, FSType: "ext4"},
				},
			},
		},
	}
	img.Status.State = keziov1alpha3.ImageStateReady
	return img
}

// setFreshAgentSession sets machine.status.agentSession to a freshly
// minted session carrying hash, the write internal/agentserver's
// handleRegister makes on every registration. Provision's boot-into-agent
// step keys off this hash changing (see agentSessionFresh's doc comment),
// not off MachineConditionAgentRegistered - that condition is set True
// once and never cleared, so it cannot tell a fresh registration for this
// attempt apart from a stale one an earlier Inspect already left in
// place.
func setFreshAgentSession(machine *keziov1alpha3.Machine, hash string) {
	machine.Status.AgentSession = &keziov1alpha3.MachineAgentSessionStatus{
		TokenHash: hash,
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
}

func TestAgentDeployerProvisionFirstPassArmsPXEAndPowersOn(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}
	if run.Status.Phase != "" {
		t.Fatalf("run.Status.Phase = %q, want empty (plan not validated until the agent is confirmed booted)", run.Status.Phase)
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

	if _, armed := provisionBootMarker(machine); !armed {
		t.Fatal("machine has no provision-boot marker after the first Provision pass")
	}
}

// TestAgentDeployerProvisionFirstPassIssuesFreshBootToken covers "a new
// DeployRun boot mints a new token": a Provision boot armed after an
// earlier Inspect boot already had one outstanding must mint its own,
// distinct token, and TokenStore's single-outstanding-token-per-MAC
// invariant (see its doc comment) means the earlier one stops resolving
// at all once the new one is armed - superseded, not merely shadowed.
func TestAgentDeployerProvisionFirstPassIssuesFreshBootToken(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	tokens := bootserver.NewTokenStore()
	d := &AgentDeployer{
		Client:      c,
		PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace},
		Tokens:      tokens,
	}

	mac, ok := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		t.Fatalf("test machine's boot MAC does not normalize: %q", machine.Spec.BootMACAddress)
	}

	if _, err := d.Inspect(context.Background(), machine, false); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	firstHash := machine.Status.NetBoot.TokenHash
	firstToken, ok := tokens.Lookup(mac, firstHash)
	if !ok {
		t.Fatalf("first (Inspect) token not found in the TokenStore")
	}

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	secondHash := machine.Status.NetBoot.TokenHash
	if secondHash == firstHash {
		t.Fatalf("Provision's boot did not mint a fresh token hash: still %q", secondHash)
	}
	secondToken, ok := tokens.Lookup(mac, secondHash)
	if !ok {
		t.Fatalf("second (Provision) token not found in the TokenStore")
	}
	if secondToken == firstToken {
		t.Fatalf("Provision's boot token equals Inspect's earlier one, want a fresh token")
	}
	if _, ok := tokens.Lookup(mac, firstHash); ok {
		t.Fatalf("Inspect's superseded token hash still resolves after Provision armed a new one")
	}
}

func TestAgentDeployerProvisionFirstPassAlreadyOnPowerCycles(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	fakeBMCForAddress(machine.Spec.BMC.Address).state = bmc.PowerStateOn

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}

	_, powerOn, _, _, powerCycle, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if powerCycle != 1 {
		t.Errorf("PowerCycle calls = %d, want 1", powerCycle)
	}
	if powerOn != 0 {
		t.Errorf("PowerOn calls = %d, want 0 (machine was already on; must PowerCycle, not PowerOn)", powerOn)
	}
}

func TestAgentDeployerProvisionSecondPassWithoutFreshSessionContinuesAndDoesNotReArm(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("second Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("second Provision() outcome = %v, want Continuing", result.Outcome)
	}
	if run.Status.Phase != "" {
		t.Fatalf("run.Status.Phase = %q, want empty (no fresh agent session observed yet)", run.Status.Phase)
	}

	setPXE, powerOn, _, _, _, _ := fakeBMCForAddress(machine.Spec.BMC.Address).calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1 (must not re-arm while already armed)", setPXE, powerOn)
	}
}

// stalePriorAgentSession simulates an earlier Inspect leaving machine
// with a still-valid, but stale, agent session before Provision ever
// arms its own boot marker - the case agentSessionFresh must not
// mistake for this attempt's own agent having booted.
func stalePriorAgentSession(machine *keziov1alpha3.Machine) {
	setFreshAgentSession(machine, "stale-session-hash")
}

func TestAgentDeployerProvisionStalePriorSessionDoesNotCountAsBooted(t *testing.T) {
	machine := agentTestMachine(t)
	stalePriorAgentSession(machine)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("second Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("second Provision() outcome = %v, want Continuing (the pre-existing session predates arming, not a fresh registration)", result.Outcome)
	}
	if run.Status.Phase != "" {
		t.Fatalf("run.Status.Phase = %q, want empty", run.Status.Phase)
	}
}

func TestAgentDeployerProvisionBootDeadlineExceededFailsRestart(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	marker := agentDeployerProvisionBootMarker{ArmedAt: time.Now().Add(-2 * agentDeployerProvisionBootDeadline)}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	machine.Annotations = map[string]string{agentDeployerProvisionBootAnnotation: string(data)}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Errorf("Provision() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeRestart)
	}
	if result.ErrorMessage == "" {
		t.Error("Provision() ErrorMessage is empty, want an explanation")
	}
}

func TestAgentDeployerProvisionBootDeadlineExceededRestartOnFailureReArms(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	marker := agentDeployerProvisionBootMarker{ArmedAt: time.Now().Add(-2 * agentDeployerProvisionBootDeadline)}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	machine.Annotations = map[string]string{agentDeployerProvisionBootAnnotation: string(data)}

	result, err := d.Provision(context.Background(), machine, run, true)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing (restartOnFailure must re-arm, not report the stale deadline)", result.Outcome)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, _, _ := f.calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1", setPXE, powerOn)
	}
	newMarker, armed := provisionBootMarker(machine)
	if !armed || time.Since(newMarker.ArmedAt) > time.Minute {
		t.Errorf("provision-boot marker not refreshed to a recent timestamp: marker=%+v armed=%v", newMarker, armed)
	}
}

// seedInProgressProvisionAttempt writes a mid-run DeployRun status onto
// run (WritingContent, with partitions, phase timings, and a not-yet-
// terminal Succeeded=False condition), the shape restartOnFailure must
// either preserve untouched or discard wholesale depending on its value.
func seedInProgressProvisionAttempt(t *testing.T, c client.Client, run *keziov1alpha3.DeployRun) {
	t.Helper()
	run.Status.Phase = keziov1alpha3.DeployRunPhaseWritingContent
	run.Status.Partitions = []keziov1alpha3.DeployRunPartitionProgress{{Number: 1, Percent: 42}}
	run.Status.PhaseTimings = []keziov1alpha3.DeployRunPhaseTiming{
		{Phase: keziov1alpha3.DeployRunPhasePending, StartedAt: metav1.Now()},
		{Phase: keziov1alpha3.DeployRunPhaseWritingContent, StartedAt: metav1.Now()},
	}
	lastProgress := metav1.Now()
	run.Status.LastProgressAt = &lastProgress
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: keziov1alpha3.DeployRunConditionSucceeded, Status: metav1.ConditionFalse, Reason: "InProgress", Message: "in progress",
	})
	if err := c.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("seed in-progress DeployRun status: %v", err)
	}
}

func TestAgentDeployerProvisionRestartOnFailureDiscardsInProgressAttempt(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	run := newTestDeployRun(t, c, machine)
	seedInProgressProvisionAttempt(t, c, run)

	result, err := d.Provision(context.Background(), machine, run, true)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing (restart discards the abandoned attempt and reboots)", result.Outcome)
	}
	if run.Status.Phase != "" {
		t.Errorf("run.Status.Phase = %q, want empty: the abandoned attempt's phase must be discarded", run.Status.Phase)
	}
	if len(run.Status.Partitions) != 0 || len(run.Status.PhaseTimings) != 0 || len(run.Status.Conditions) != 0 || run.Status.LastProgressAt != nil {
		t.Errorf("run.Status = %+v, want Partitions/PhaseTimings/LastProgressAt/Conditions all cleared", run.Status)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, _, _ := f.calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1 (restart must boot the machine again)", setPXE, powerOn)
	}
	if _, armed := provisionBootMarker(machine); !armed {
		t.Error("machine has no provision-boot marker after restart, want a fresh one armed")
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get DeployRun: %v", err)
	}
	if stored.Status.Phase != "" {
		t.Errorf("stored run.Status.Phase = %q, want empty", stored.Status.Phase)
	}
}

func TestAgentDeployerProvisionResumeDoesNotDiscardInProgressAttempt(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	run := newTestDeployRun(t, c, machine)
	seedInProgressProvisionAttempt(t, c, run)

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}
	if run.Status.Phase != keziov1alpha3.DeployRunPhaseWritingContent {
		t.Errorf("run.Status.Phase = %q, want %q untouched", run.Status.Phase, keziov1alpha3.DeployRunPhaseWritingContent)
	}
	if len(run.Status.Partitions) != 1 || len(run.Status.PhaseTimings) != 2 || len(run.Status.Conditions) != 1 {
		t.Errorf("run.Status = %+v, want Partitions/PhaseTimings/Conditions left as seeded", run.Status)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, _, _ := f.calls()
	if setPXE != 0 || powerOn != 0 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 0/0 (resuming must not reboot the machine)", setPXE, powerOn)
	}
}

func TestAgentDeployerProvisionMissingCredentialsSecretFailsTransientWithoutLeakingCredentials(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.BMC.CredentialsSecretRef.Name = agentTestMissingSecretName
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine)
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha3.MachineErrorTypeTransient {
		t.Errorf("Provision() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeTransient)
	}
	if strings.Contains(result.ErrorMessage, "s3cr3t") || strings.Contains(result.ErrorMessage, "admin") {
		t.Fatalf("Provision() ErrorMessage leaked credential contents: %q", result.ErrorMessage)
	}
}

func TestAgentDeployerProvisionNetworkUnreachableReportsDelayed(t *testing.T) {
	machine := agentTestMachine(t)
	machine.Spec.BMC.Address = fakeBMCDialErrScheme + "://10.0.0.10"
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Provision() outcome = %v, want Delayed", result.Outcome)
	}
	if result.ErrorType != "" || result.ErrorMessage != "" {
		t.Errorf("Provision() ErrorType/ErrorMessage = %q/%q, want both empty for Delayed", result.ErrorType, result.ErrorMessage)
	}
}

func TestAgentDeployerProvisionAgentBootedNotReadyReportsDelayed(t *testing.T) {
	machine := agentTestMachine(t)
	claim := agentTestClaimWithImage(machine, "img1")
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, claim, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	setFreshAgentSession(machine, "session-hash-1")

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Delayed {
		t.Fatalf("Provision() outcome = %v, want Delayed (no MachineHardware yet)", result.Outcome)
	}
}

func TestAgentDeployerProvisionAgentBootedValidationErrorReportsFailed(t *testing.T) {
	machine := agentTestMachine(t)
	// No ImageRef and no DataImages: Build rejects this before any lookup.
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	setFreshAgentSession(machine, "session-hash-1")

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha3.MachineErrorTypeTransient {
		t.Errorf("Provision() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeTransient)
	}
	if result.ErrorMessage == "" {
		t.Error("Provision() ErrorMessage is empty, want an explanation")
	}
	// The plan-build failure is not a boot problem: the marker must stay
	// armed so a retry does not power-cycle the machine again.
	if _, armed := provisionBootMarker(machine); !armed {
		t.Error("provision-boot marker cleared after a plan-build failure, want it kept armed")
	}
}

func TestAgentDeployerProvisionAgentBootedSuccessRecordsPendingAndContinues(t *testing.T) {
	machine := agentTestMachine(t)
	claim := agentTestClaimWithImage(machine, "img1")
	hw := &keziov1alpha3.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace},
		Spec:       keziov1alpha3.MachineHardwareSpec{Disks: []keziov1alpha3.MachineHardwareDisk{{DeviceName: "/dev/vda", SizeBytes: 32 << 30}}},
	}
	img := blankDataImage("img1", machine.Namespace)
	c := newAgentTestClient(t, machine, claim, agentTestBMCSecret(), hw, img)
	mustCreateDefaultPostHook(t, c)
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	run := newTestDeployRun(t, c, machine)

	if _, err := d.Provision(context.Background(), machine, run, false); err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	setFreshAgentSession(machine, "session-hash-1")

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("Provision() outcome = %v, want Continuing", result.Outcome)
	}
	if run.Status.Phase != keziov1alpha3.DeployRunPhasePending {
		t.Fatalf("run.Status.Phase = %q, want %q", run.Status.Phase, keziov1alpha3.DeployRunPhasePending)
	}
	if len(run.Status.PhaseTimings) != 1 || run.Status.PhaseTimings[0].Phase != keziov1alpha3.DeployRunPhasePending {
		t.Fatalf("run.Status.PhaseTimings = %+v, want one Pending entry", run.Status.PhaseTimings)
	}
	if run.Status.LastProgressAt == nil {
		t.Error("run.Status.LastProgressAt = nil, want the stall clock started at the Pending commit")
	}
	if _, armed := provisionBootMarker(machine); armed {
		t.Error("provision-boot marker still present after Provision committed the run to Pending, want it cleared")
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get DeployRun: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhasePending {
		t.Fatalf("stored run.Status.Phase = %q, want %q", stored.Status.Phase, keziov1alpha3.DeployRunPhasePending)
	}
}

func TestAgentDeployerProvisionLaterPassSucceededReportsComplete(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
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
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhaseFailed
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: keziov1alpha3.DeployRunConditionSucceeded, Status: metav1.ConditionFalse, Reason: "DeployFailed", Message: "writing partition table: rejected",
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
	if result.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Errorf("Provision() ErrorType = %q, want %q (an agent-reported Failed run has abandoned its attempt)", result.ErrorType, keziov1alpha3.MachineErrorTypeRestart)
	}
	if result.ErrorMessage != "writing partition table: rejected" {
		t.Errorf("Provision() ErrorMessage = %q, want the DeployRun's recorded failure message", result.ErrorMessage)
	}
}

// TestAgentDeployerProvisionRestartOnFailureAfterAgentReportedFailedStartsFreshAttempt
// proves the self-heal path this ErrorType exists for: a Provision call
// that observes an agent-reported Failed run, followed by the next call
// with restartOnFailure true (as the controller sends once it has
// recorded ErrorType Restart), discards the failed run's state and
// re-arms the boot exactly like any other restart.
func TestAgentDeployerProvisionRestartOnFailureAfterAgentReportedFailedStartsFreshAttempt(t *testing.T) {
	machine := agentTestMachine(t)
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}
	run := newTestDeployRun(t, c, machine)
	run.Status.Phase = keziov1alpha3.DeployRunPhaseFailed
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: keziov1alpha3.DeployRunConditionSucceeded, Status: metav1.ConditionFalse, Reason: "DeployFailed", Message: "agent reported failed",
	})
	if err := c.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("seed Failed DeployRun status: %v", err)
	}

	firstResult, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	if firstResult.Outcome != Failed || firstResult.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Fatalf("first Provision() = %+v, want Failed/Restart", firstResult)
	}

	result, err := d.Provision(context.Background(), machine, run, true)
	if err != nil {
		t.Fatalf("restart Provision() error = %v", err)
	}
	if result.Outcome != Continuing {
		t.Fatalf("restart Provision() outcome = %v, want Continuing (a fresh attempt on the same run)", result.Outcome)
	}
	if run.Status.Phase != "" || len(run.Status.Conditions) != 0 {
		t.Errorf("run.Status = %+v, want Phase/Conditions cleared for a fresh attempt", run.Status)
	}

	f := fakeBMCForAddress(machine.Spec.BMC.Address)
	setPXE, powerOn, _, _, _, _ := f.calls()
	if setPXE != 1 || powerOn != 1 {
		t.Errorf("SetOneTimePXEBoot/PowerOn calls = %d/%d, want 1/1 (restart must re-arm and power the machine on again)", setPXE, powerOn)
	}
	if _, armed := provisionBootMarker(machine); !armed {
		t.Error("machine has no provision-boot marker after restarting past an agent-reported Failed run")
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get DeployRun: %v", err)
	}
	if stored.Status.Phase != "" {
		t.Errorf("stored run.Status.Phase = %q, want empty", stored.Status.Phase)
	}
}

func TestAgentDeployerProvisionLaterPassInProgressReportsContinuing(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhaseWritingContent
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

func TestAgentDeployerProvisionLaterPassRecentProgressReportsContinuing(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhaseWritingContent
	recent := metav1.NewTime(time.Now().Add(-time.Minute))
	run.Status.LastProgressAt = &recent
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

func TestAgentDeployerProvisionLaterPassStalledFailsRestart(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhaseWritingContent
	stale := metav1.NewTime(time.Now().Add(-2 * agentDeployerProvisionStallDeadline))
	run.Status.LastProgressAt = &stale
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed {
		t.Fatalf("Provision() outcome = %v, want Failed", result.Outcome)
	}
	if result.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Errorf("Provision() ErrorType = %q, want %q", result.ErrorType, keziov1alpha3.MachineErrorTypeRestart)
	}
	if !strings.Contains(result.ErrorMessage, keziov1alpha3.DeployRunPhaseWritingContent) {
		t.Errorf("Provision() ErrorMessage = %q, want it to name the stalled phase", result.ErrorMessage)
	}
}

// TestAgentDeployerProvisionLaterPassStalledWithoutLastProgressFallsBackToPhaseStart
// covers a run written before status.lastProgressAt existed: the stall
// baseline falls back to the current phase's own start time rather than
// leaving such a run unbounded.
func TestAgentDeployerProvisionLaterPassStalledWithoutLastProgressFallsBackToPhaseStart(t *testing.T) {
	machine := agentTestMachine(t)
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "m1-run1", Namespace: "default"}}
	run.Status.Phase = keziov1alpha3.DeployRunPhasePending
	run.Status.PhaseTimings = []keziov1alpha3.DeployRunPhaseTiming{
		{Phase: keziov1alpha3.DeployRunPhasePending, StartedAt: metav1.NewTime(time.Now().Add(-2 * agentDeployerProvisionStallDeadline))},
	}
	c := newAgentTestClient(t, machine, agentTestBMCSecret())
	d := &AgentDeployer{Client: c, PlanBuilder: &planbuild.Builder{Client: c, ManagerNamespace: agentProvisionTestManagerNamespace}}

	result, err := d.Provision(context.Background(), machine, run, false)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Outcome != Failed || result.ErrorType != keziov1alpha3.MachineErrorTypeRestart {
		t.Fatalf("Provision() = %+v, want Failed/Restart", result)
	}
}

func TestAgentDeployerDeprovisionReportsComplete(t *testing.T) {
	machine := agentTestMachine(t)
	d := &AgentDeployer{Client: newAgentTestClient(t)}

	result, err := d.Deprovision(context.Background(), machine, false)
	if err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if result.Outcome != Complete {
		t.Fatalf("Deprovision() outcome = %v, want Complete", result.Outcome)
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
	machine.Spec.BMC.CredentialsSecretRef.Name = agentTestMissingSecretName
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
