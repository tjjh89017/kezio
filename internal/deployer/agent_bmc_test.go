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
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// testBMCScheme is the URL scheme testBMCConnect registers with
// internal/bmc's registry (see this file's init), so a test Machine can
// point bmcSpec.Address at "kezio-testbmc://<key>" and get a fake,
// in-memory BMC back from agentDeployer.connectBMC instead of a real
// driver trying to reach real hardware.
const testBMCScheme = "kezio-testbmc"

func init() {
	bmc.Register(testBMCScheme, testBMCConnect)
}

// testBMC is a fake bmc.BMC that records every call it receives and lets
// a test script a failure from any method - the same seam pattern
// deployer.FailFunc gives the fake Deployer, applied one level down at
// the BMC itself.
type testBMC struct {
	mu sync.Mutex

	state                  bmc.PowerState
	setOneTimePXEBootCalls int
	powerOnCalls           int
	powerOffCalls          int
	powerCycleCalls        int
	getPowerStateCalls     int

	setOneTimePXEBootErr error
	powerOnErr           error
	powerOffErr          error
	powerCycleErr        error
	getPowerStateErr     error

	gotCreds bmc.Credentials
}

func (f *testBMC) calls() (setPXE, powerOn, powerOff, powerCycle, getState int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setOneTimePXEBootCalls, f.powerOnCalls, f.powerOffCalls, f.powerCycleCalls, f.getPowerStateCalls
}

func (f *testBMC) PowerOn(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerOnCalls++
	if f.powerOnErr != nil {
		return f.powerOnErr
	}
	f.state = bmc.PowerStateOn
	return nil
}

func (f *testBMC) PowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerOffCalls++
	if f.powerOffErr != nil {
		return f.powerOffErr
	}
	f.state = bmc.PowerStateOff
	return nil
}

func (f *testBMC) PowerCycle(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerCycleCalls++
	if f.powerCycleErr != nil {
		return f.powerCycleErr
	}
	f.state = bmc.PowerStateOn
	return nil
}

func (f *testBMC) GetPowerState(context.Context) (bmc.PowerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPowerStateCalls++
	if f.getPowerStateErr != nil {
		return "", f.getPowerStateErr
	}
	return f.state, nil
}

func (f *testBMC) SetOneTimePXEBoot(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setOneTimePXEBootCalls++
	return f.setOneTimePXEBootErr
}

var (
	testBMCsMu sync.Mutex
	testBMCs   = map[string]*testBMC{}
)

// testBMCFor returns (creating an initially-off fake on first use) the
// fake BMC that testBMCConnect will hand back for a
// "kezio-testbmc://<key>" address. Tests call this before Connect to
// pre-script a failure, or after to inspect recorded calls; either way
// the same *testBMC is returned for the same key, keyed independently of
// bmc.Register's process-global registry so concurrent test cases (using
// distinct keys) do not observe each other's calls.
func testBMCFor(key string) *testBMC {
	testBMCsMu.Lock()
	defer testBMCsMu.Unlock()
	f, ok := testBMCs[key]
	if !ok {
		f = &testBMC{state: bmc.PowerStateOff}
		testBMCs[key] = f
	}
	return f
}

func testBMCConnect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	f := testBMCFor(address.Host + address.Path)
	f.mu.Lock()
	f.gotCreds = creds
	f.mu.Unlock()
	return f, nil
}

// testBMCAddress builds a unique "kezio-testbmc://" address for t, so
// parallel subtests never collide on the same *testBMC.
func testBMCAddress(t *testing.T) string {
	t.Helper()
	return testBMCScheme + "://" + strings.ReplaceAll(t.Name(), "/", "-")
}

// newAgentTestClientWithSecret builds a fake client seeded with machine
// and, when secret is non-nil, the BMC credentials Secret Register's
// connectBMC call resolves.
func newAgentTestClientWithSecret(t *testing.T, machine *keziov1alpha1.Machine, secret *corev1.Secret) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (corev1): %v", err)
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha1.Machine{}).
		WithObjects(machine)
	if secret != nil {
		builder = builder.WithObjects(secret)
	}
	return builder.Build()
}

// bmcCredsSecretName is the name every test in this file uses for the
// Machine's bmc.credentialsSecretRef.
const bmcCredsSecretName = "node-01-bmc"

// bmcCredsSecret builds the Secret bmcCredsSecretName names, with the
// well-known username/password keys internal/bmc expects.
func bmcCredsSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: bmcCredsSecretName},
		Data: map[string][]byte{
			bmc.SecretKeyUsername: []byte("admin"),
			bmc.SecretKeyPassword: []byte("s3cr3t"),
		},
	}
}

// TestAgentDeployer_Register_WithBMC_SetsOneTimePXEBootAndPowersOn covers
// the documented Register sequence when a BMC is configured and the
// machine is currently off: SetOneTimePXEBoot then PowerOn (not
// PowerCycle), and the reconciler-visible Result reports no error.
func TestAgentDeployer_Register_WithBMC_SetsOneTimePXEBootAndPowersOn(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key, bmcSpec: machine.Spec.BMC}

	result, err := dep.Register(context.Background(), &RegisterData{BMC: machine.Spec.BMC})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}

	setPXE, powerOn, powerOff, powerCycle, getState := testBMCFor(strings.TrimPrefix(address, testBMCScheme+"://")).calls()
	if setPXE != 1 {
		t.Errorf("SetOneTimePXEBoot calls = %d, want 1", setPXE)
	}
	if getState != 1 {
		t.Errorf("GetPowerState calls = %d, want 1", getState)
	}
	if powerOn != 1 {
		t.Errorf("PowerOn calls = %d, want 1", powerOn)
	}
	if powerOff != 0 {
		t.Errorf("PowerOff calls = %d, want 0", powerOff)
	}
	if powerCycle != 0 {
		t.Errorf("PowerCycle calls = %d, want 0 (machine was off; Register should PowerOn, not PowerCycle)", powerCycle)
	}

	f := testBMCFor(strings.TrimPrefix(address, testBMCScheme+"://"))
	f.mu.Lock()
	gotCreds := f.gotCreds
	f.mu.Unlock()
	if gotCreds.Username != "admin" || gotCreds.Password != "s3cr3t" {
		t.Errorf("gotCreds = %+v, want the resolved secret credentials", gotCreds)
	}
}

// TestAgentDeployer_Register_BMCAlreadyOn_PowerCycles covers the other
// branch of Register's power decision: a machine GetPowerState reports
// already on must be forced through PowerCycle, not PowerOn, to
// guarantee the clean boot the net boot needs.
func TestAgentDeployer_Register_BMCAlreadyOn_PowerCycles(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key, bmcSpec: machine.Spec.BMC}

	// Pre-seed the fake BMC as already on.
	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	testBMCFor(fakeKey).state = bmc.PowerStateOn

	result, err := dep.Register(context.Background(), &RegisterData{BMC: machine.Spec.BMC})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}

	_, powerOn, _, powerCycle, _ := testBMCFor(fakeKey).calls()
	if powerCycle != 1 {
		t.Errorf("PowerCycle calls = %d, want 1", powerCycle)
	}
	if powerOn != 0 {
		t.Errorf("PowerOn calls = %d, want 0 (machine was already on; Register should PowerCycle, not PowerOn)", powerOn)
	}
}

// TestAgentDeployer_Register_NoBMC_FallsBackToNetbootWait covers the
// pre-BMC behavior: a nil bmcSpec (spec.bmc absent) must leave Register's
// netboot-wait no-op unchanged, with no BMC ever contacted.
func TestAgentDeployer_Register_NoBMC_FallsBackToNetbootWait(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	c := newAgentTestClient(t, machine)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key}

	result, err := dep.Register(context.Background(), &RegisterData{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}

	updated := &keziov1alpha1.Machine{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.MachineConditionAgentRegistered)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("AgentRegistered condition = %+v, want False (the pre-BMC netboot-wait reset)", cond)
	}
}

// TestAgentDeployer_ConnectBMC_NonNilEmptyAddress_FallsBackToNetbootWait
// covers the other no-BMC shape: a non-nil bmcSpec whose Address is still
// empty must take the same fallback as a nil bmcSpec, and must never
// attempt to resolve credentials or connect.
func TestAgentDeployer_ConnectBMC_NonNilEmptyAddress_FallsBackToNetbootWait(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	c := newAgentTestClient(t, machine)
	dep := &agentDeployer{
		client:  c,
		key:     types.NamespacedName{Namespace: "default", Name: "node-01"},
		bmcSpec: &keziov1alpha1.MachineBMC{},
	}

	bmcClient, err := dep.connectBMC(context.Background())
	if err != nil {
		t.Fatalf("connectBMC: %v", err)
	}
	if bmcClient != nil {
		t.Fatalf("connectBMC returned %v, want nil (empty address must fall back to netboot-wait)", bmcClient)
	}
}

// TestAgentDeployer_Register_BMCConnectError_RedactsCredentials covers
// the error path when the BMC credentials Secret does not exist: Register
// must report a non-empty ErrorMessage, and that message must never
// contain the Secret's would-be contents (trivially true here since none
// were ever read, but this locks the observable behavior in as a
// regression test).
func TestAgentDeployer_Register_BMCConnectError_RedactsCredentials(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "missing-secret"},
	}
	c := newAgentTestClientWithSecret(t, machine, nil)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key, bmcSpec: machine.Spec.BMC}

	result, err := dep.Register(context.Background(), &RegisterData{BMC: machine.Spec.BMC})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.ErrorMessage == "" {
		t.Fatal("ErrorMessage = \"\", want a non-empty message when the BMC credentials secret is missing")
	}
	if strings.Contains(result.ErrorMessage, "s3cr3t") || strings.Contains(result.ErrorMessage, "admin") {
		t.Fatalf("ErrorMessage leaked credential contents: %q", result.ErrorMessage)
	}
}

// TestAgentDeployer_PowerOn_WithBMC_CallsThrough and its PowerOff sibling
// cover the direct phase methods (no Register/RegisterData involved),
// confirming they call through to the configured BMC.
func TestAgentDeployer_PowerOn_WithBMC_CallsThrough(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	result, err := dep.PowerOn(context.Background())
	if err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	_, powerOn, _, _, _ := testBMCFor(strings.TrimPrefix(address, testBMCScheme+"://")).calls()
	if powerOn != 1 {
		t.Errorf("PowerOn calls = %d, want 1", powerOn)
	}
}

func TestAgentDeployer_PowerOff_WithBMC_CallsThrough(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	result, err := dep.PowerOff(context.Background())
	if err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	_, _, powerOff, _, _ := testBMCFor(strings.TrimPrefix(address, testBMCScheme+"://")).calls()
	if powerOff != 1 {
		t.Errorf("PowerOff calls = %d, want 1", powerOff)
	}
}

// TestAgentDeployer_PowerOnOff_NoBMC_NoOp covers the pre-BMC fallback for
// both power phases: no BMC configured must stay the documented no-op
// (success, Dirty=true), unchanged from before this BMC wiring landed.
func TestAgentDeployer_PowerOnOff_NoBMC_NoOp(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	c := newAgentTestClient(t, machine)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}}

	onResult, err := dep.PowerOn(context.Background())
	if err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if onResult.ErrorMessage != "" || !onResult.Dirty {
		t.Fatalf("PowerOn result = %+v, want a dirty success no-op", onResult)
	}

	offResult, err := dep.PowerOff(context.Background())
	if err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if offResult.ErrorMessage != "" || !offResult.Dirty {
		t.Fatalf("PowerOff result = %+v, want a dirty success no-op", offResult)
	}
}

// TestAgentDeployer_PowerOn_BMCPowerOnFails_ReportsError covers a BMC
// call itself failing (as opposed to connectBMC failing): the driver's
// error must surface as Result.ErrorMessage.
func TestAgentDeployer_PowerOn_BMCPowerOnFails_ReportsError(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = &keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	testBMCFor(fakeKey).powerOnErr = errors.New("bmc: unsupported action")

	result, err := dep.PowerOn(context.Background())
	if err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if result.ErrorMessage == "" {
		t.Fatal("ErrorMessage = \"\", want a non-empty message when the BMC rejects PowerOn")
	}
}
