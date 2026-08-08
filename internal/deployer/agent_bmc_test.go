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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// testBMCScheme is registered with internal/bmc's registry (see init), so
// a test Machine pointing bmcSpec.Address at "kezio-testbmc://<key>" gets
// a fake in-memory BMC back from connectBMC instead of real hardware.
const testBMCScheme = "kezio-testbmc"

func init() {
	bmc.Register(testBMCScheme, testBMCConnect)
}

// testBMC is a fake bmc.BMC that records calls and lets a test script a
// failure from any method.
type testBMC struct {
	mu sync.Mutex

	state                  bmc.PowerState
	setOneTimePXEBootCalls int
	powerOnCalls           int
	powerOffCalls          int
	powerCycleCalls        int
	getPowerStateCalls     int

	// getPowerStateOverride, when non-empty, overrides state - simulates a
	// BMC whose read-back disagrees with the commanded power state.
	getPowerStateOverride bmc.PowerState

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
	if f.getPowerStateOverride != "" {
		return f.getPowerStateOverride, nil
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
// fake BMC for a given "kezio-testbmc://<key>" address, keyed separately
// from bmc.Register's process-global registry so concurrent test cases
// using distinct keys don't observe each other's calls.
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

// BMC configured, machine off: SetOneTimePXEBoot then PowerOn (not
// PowerCycle), no error.
func TestAgentDeployer_Register_WithBMC_SetsOneTimePXEBootAndPowersOn(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key, bmcSpec: machine.Spec.BMC}

	result, err := dep.Register(context.Background(), &RegisterData{})
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

// Machine already on (per GetPowerState) must be forced through
// PowerCycle, not PowerOn, for a clean net boot.
func TestAgentDeployer_Register_BMCAlreadyOn_PowerCycles(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
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

	result, err := dep.Register(context.Background(), &RegisterData{})
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

// Missing BMC credentials Secret: Register reports a non-empty
// ErrorMessage that never contains credential contents.
func TestAgentDeployer_Register_BMCConnectError_RedactsCredentials(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "missing-secret"},
	}
	c := newAgentTestClientWithSecret(t, machine, nil)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	dep := &agentDeployer{client: c, key: key, bmcSpec: machine.Spec.BMC}

	result, err := dep.Register(context.Background(), &RegisterData{})
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

// PowerOn/PowerOff called directly (no Register involved) call through to
// the configured BMC.
func TestAgentDeployer_PowerOn_WithBMC_CallsThrough(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
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
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
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

// A BMC PowerOn call itself failing (not connectBMC) must surface as
// Result.ErrorMessage.
func TestAgentDeployer_PowerOn_BMCPowerOnFails_ReportsError(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
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

// Even if PowerOn itself reports success, a disagreeing GetPowerState
// read-back must be reflected in Result.PoweredOn, not the commanded
// state.
func TestAgentDeployer_PowerOn_ObservesDriverReportedState(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	// Simulate hardware that never actually powered on despite ack'ing PowerOn.
	testBMCFor(fakeKey).getPowerStateOverride = bmc.PowerStateOff

	result, err := dep.PowerOn(context.Background())
	if err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty (the PowerOn command itself succeeded)", result.ErrorMessage)
	}
	if result.PoweredOn == nil || *result.PoweredOn {
		t.Fatalf("PoweredOn = %v, want a non-nil false (the driver-reported state, not the commanded true)", result.PoweredOn)
	}
}

// PowerOn's read-back test, mirrored for PowerOff.
func TestAgentDeployer_PowerOff_ObservesDriverReportedState(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	testBMCFor(fakeKey).getPowerStateOverride = bmc.PowerStateOn

	result, err := dep.PowerOff(context.Background())
	if err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty (the PowerOff command itself succeeded)", result.ErrorMessage)
	}
	if result.PoweredOn == nil || !*result.PoweredOn {
		t.Fatalf("PoweredOn = %v, want a non-nil true (the driver-reported state, not the commanded false)", result.PoweredOn)
	}
}

// The read-back itself failing after a successful PowerOn must not
// surface as Result.ErrorMessage, only as a nil Result.PoweredOn.
func TestAgentDeployer_PowerOn_GetPowerStateFails_PoweredOnNil(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	testBMCFor(fakeKey).getPowerStateErr = errors.New("bmc: timed out reading power state")

	result, err := dep.PowerOn(context.Background())
	if err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty (the PowerOn command itself succeeded)", result.ErrorMessage)
	}
	if result.PoweredOn != nil {
		t.Fatalf("PoweredOn = %v, want nil when the read-back itself fails", *result.PoweredOn)
	}
}

// PowerCycle's BMC-driven path, same shape as PowerOn/PowerOff.
func TestAgentDeployer_PowerCycle_WithBMC_CallsThrough(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	result, err := dep.PowerCycle(context.Background())
	if err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	_, _, _, powerCycle, _ := testBMCFor(strings.TrimPrefix(address, testBMCScheme+"://")).calls()
	if powerCycle != 1 {
		t.Errorf("PowerCycle calls = %d, want 1", powerCycle)
	}
	if result.PoweredOn == nil || !*result.PoweredOn {
		t.Fatalf("PoweredOn = %v, want a non-nil true after a successful power-cycle", result.PoweredOn)
	}
}

// A BMC PowerCycle call itself failing.
func TestAgentDeployer_PowerCycle_BMCPowerCycleFails_ReportsError(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	address := testBMCAddress(t)
	machine.Spec.BMC = keziov1alpha1.MachineBMC{
		Address:              address,
		CredentialsSecretRef: keziov1alpha1.SecretReference{Name: bmcCredsSecretName},
	}
	secret := bmcCredsSecret()
	c := newAgentTestClientWithSecret(t, machine, secret)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}, bmcSpec: machine.Spec.BMC}

	fakeKey := strings.TrimPrefix(address, testBMCScheme+"://")
	testBMCFor(fakeKey).powerCycleErr = errors.New("bmc: unsupported action")

	result, err := dep.PowerCycle(context.Background())
	if err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	if result.ErrorMessage == "" {
		t.Fatal("ErrorMessage = \"\", want a non-empty message when the BMC rejects PowerCycle")
	}
}
