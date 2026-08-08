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

package agentserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// newTestServer builds a Server backed by a fake client seeded with
// machines, indexed the same way SetupFieldIndexer configures a real
// manager cache.
func newTestServer(t *testing.T, now time.Time, machines ...*keziov1alpha1.Machine) (*Server, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha1.Machine{}, MachineTokenHashIndexField, IndexMachineTokenHash).
		WithIndex(&keziov1alpha1.Machine{}, MachineNameIndexField, IndexMachineName).
		WithStatusSubresource(&keziov1alpha1.Machine{})
	for _, m := range machines {
		builder = builder.WithObjects(m)
	}
	c := builder.Build()

	s := New(c, Config{})
	s.Now = func() time.Time { return now }
	return s, c
}

// TestServer_NeedLeaderElectionIsFalse guards against a regression that
// broke agent registration during a rolling update - see
// bootserver.TestServer_NeedLeaderElectionIsFalse's doc comment for the
// full reasoning; this server needs the identical guarantee for the
// same reason (a rolling update must not leave POST /agent/register
// unanswered until the previous pod's un-released leader lease expires).
func TestServer_NeedLeaderElectionIsFalse(t *testing.T) {
	s, _ := newTestServer(t, time.Now())
	if s.NeedLeaderElection() {
		t.Fatal("Server.NeedLeaderElection() = true; want false so a rolling update does not leave /agent/register unanswered until the old lease expires")
	}
}

// testMachineName is the name every test machine in this file uses.
const testMachineName = "node-01"

// newTestMachine builds a Machine named testMachineName with a live,
// unexpired boot token whose hash is bootserver.HashToken(testToken).
func newTestMachine(now time.Time) *keziov1alpha1.Machine {
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Spec: keziov1alpha1.MachineSpec{
			BMC: keziov1alpha1.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: testMachineName + "-bmc"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateInspecting,
			NetBoot: &keziov1alpha1.MachineNetBootStatus{
				TokenHash: bootserver.HashToken(testToken),
				ExpiresAt: metav1.NewTime(now.Add(30 * time.Minute)),
			},
		},
	}
}

func doRegister(t *testing.T, handler http.Handler, token string, body agentapi.RegisterRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, agentapi.RegisterPath, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sampleInventory() keziov1alpha1.MachineHardwareStatus {
	rotational := false
	return keziov1alpha1.MachineHardwareStatus{
		Disks: []keziov1alpha1.MachineHardwareDisk{
			{DeviceName: "/dev/nvme0n1", SerialNumber: "S123", SizeBytes: 512 << 30, Rotational: &rotational},
		},
		Nics: []keziov1alpha1.MachineHardwareNIC{
			{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:01"},
		},
		MemoryBytes: 16 << 30,
		CPUCount:    8,
	}
}

func TestHandleRegister_ValidTokenIngestsInventoryAndInvalidatesToken(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now)
	s, c := newTestServer(t, now, machine)
	handler := s.Handler()

	rec := doRegister(t, handler, testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var resp agentapi.RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.MachineName != "node-01" {
		t.Fatalf("MachineName = %q, want node-01", resp.MachineName)
	}
	if resp.SessionToken == "" {
		t.Fatal("SessionToken is empty; want a fresh session credential")
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: "node-01"}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Hardware == nil || len(stored.Status.Hardware.Disks) != 1 {
		t.Fatalf("hardware inventory was not ingested: %+v", stored.Status.Hardware)
	}
	if stored.Status.Hardware.Disks[0].SerialNumber != "S123" {
		t.Fatalf("ingested disk serial = %q, want S123", stored.Status.Hardware.Disks[0].SerialNumber)
	}
	if stored.Status.NetBoot.TokenHash != "" {
		t.Fatalf("token hash was not invalidated after a successful registration: %q", stored.Status.NetBoot.TokenHash)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionAgentRegistered)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("AgentRegistered condition = %+v, want True", cond)
	}
	if stored.Status.AgentSession == nil || stored.Status.AgentSession.TokenHash == "" {
		t.Fatalf("AgentSession = %+v, want a stored session hash", stored.Status.AgentSession)
	}
	if stored.Status.AgentSession.TokenHash == resp.SessionToken {
		t.Fatal("status.agentSession.tokenHash stores the plaintext session token; want only its hash")
	}
	if stored.Status.AgentSession.TokenHash != bootserver.HashToken(resp.SessionToken) {
		t.Fatalf("status.agentSession.tokenHash = %q, want the SHA-256 hash of the returned session token", stored.Status.AgentSession.TokenHash)
	}

	// Reusing the same (now-invalidated) token must fail exactly like an
	// unknown token: no Machine matches its hash any more.
	rec2 := doRegister(t, handler, testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("reused token: status = %d, want 401", rec2.Code)
	}
}

func TestHandleRegister_RejectionsAreConstantShape(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		machine *keziov1alpha1.Machine
		token   string
	}{
		{
			name:    "missing Authorization header",
			machine: newTestMachine(now),
			token:   "",
		},
		{
			name:    "unknown token",
			machine: newTestMachine(now),
			token:   "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name:    "expired token",
			machine: newTestMachine(now.Add(-time.Hour)),
			token:   testToken,
		},
		{
			name:    "no machine at all",
			machine: nil,
			token:   testToken,
		},
	}

	var referenceBody string
	var referenceCode int
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var machines []*keziov1alpha1.Machine
			if tc.machine != nil {
				machines = append(machines, tc.machine)
			}
			s, _ := newTestServer(t, now, machines...)
			rec := doRegister(t, s.Handler(), tc.token, agentapi.RegisterRequest{Hardware: sampleInventory()})

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if i == 0 {
				referenceCode = rec.Code
				referenceBody = rec.Body.String()
			} else if rec.Code != referenceCode || rec.Body.String() != referenceBody {
				t.Fatalf("response for %q differs from the reference 401 response (code=%d body=%q)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleRegister_ExpiredTokenDoesNotInvalidateOrIngest(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now.Add(-time.Hour)) // already expired
	s, c := newTestServer(t, now, machine)

	rec := doRegister(t, s.Handler(), testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: "node-01"}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Hardware != nil {
		t.Fatalf("hardware was ingested despite an expired token: %+v", stored.Status.Hardware)
	}
}

func TestHandleRegister_MalformedBodyIsBadRequest(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now)
	s, _ := newTestServer(t, now, machine)

	req := httptest.NewRequest(http.MethodPost, agentapi.RegisterPath, bytes.NewReader([]byte("not json")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// testSessionToken is the plaintext session credential every next-test
// machine in this file is seeded with; its hash is what
// newTestMachineWithSession stores in status.agentSession.tokenHash.
const testSessionToken = "session-0123456789abcdef0123456789abcdef0123456789abcdef01234567"

// newTestMachineWithSession builds a Machine named testMachineName with
// a live, unexpired agent session (hash of testSessionToken), and no
// net-boot token (already consumed, as it would be by the time a real
// agent starts polling).
func newTestMachineWithSession(now time.Time) *keziov1alpha1.Machine {
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Spec: keziov1alpha1.MachineSpec{
			BMC: keziov1alpha1.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: testMachineName + "-bmc"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateAvailable,
			AgentSession: &keziov1alpha1.MachineAgentSessionStatus{
				TokenHash: bootserver.HashToken(testSessionToken),
				ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
			},
		},
	}
}

// doNext issues a GET .../next request advertising the current
// agentapi.PlanSchemaVersion, the same as a real, up-to-date agent -
// every test in this file that is not specifically exercising the
// compatibility gate wants that default. doNextWithSchemaVersion covers
// the gate's own tests (an absent or mismatched version).
func doNext(handler http.Handler, name, token string) *httptest.ResponseRecorder {
	return doNextWithSchemaVersion(handler, name, token, strconv.Itoa(agentapi.PlanSchemaVersion))
}

// doNextWithSchemaVersion issues a GET .../next request advertising
// schemaVersion verbatim in agentapi.AgentSchemaVersionHeader; an empty
// string omits the header entirely, simulating an agent built before it
// existed.
func doNextWithSchemaVersion(handler http.Handler, name, token, schemaVersion string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, agentapi.NextPathPrefix+name+agentapi.NextPathSuffix, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if schemaVersion != "" {
		req.Header.Set(agentapi.AgentSchemaVersionHeader, schemaVersion)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHandleNext_NotProvisioningReturnsWait(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now) // State: Available
	s, _ := newTestServer(t, now, machine)

	rec := doNext(s.Handler(), testMachineName, testSessionToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionWait)
	}
	if resp.Plan != nil {
		t.Fatalf("Plan = %+v, want nil alongside ActionWait", resp.Plan)
	}
}

func TestHandleNext_ProvisioningReadyReturnsDeployPlan(t *testing.T) {
	now := time.Now()
	// readyImage fixes its Image/ConfigMap namespace at "default"; the
	// test machine has no namespace of its own (matching every other
	// test machine in this file), so the imageRef names it explicitly
	// rather than relying on ResolveNamespace's machine-namespace
	// fallback.
	imageRef := keziov1alpha1.NameRef{Namespace: "default", Name: "os-image"}
	machine := newTestMachineWithSession(now)
	machine.Spec.ImageRef = &imageRef
	machine.Status.State = keziov1alpha1.MachineStateProvisioning
	machine.Status.Provisioning = &keziov1alpha1.MachineProvisioningStatus{
		Image: &keziov1alpha1.MachineProvisionedImage{
			ImageRef:   imageRef,
			TargetDisk: "/dev/nvme0n1",
		},
	}
	image, cm := readyImage("os-image", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
	})
	// newTestServer's fake client needs a scheme that also knows
	// ImageLayout; build this test's client directly instead of going
	// through newTestServer.
	c := newPlanTestClient(t, machine, image, cm)
	s := New(c, Config{TrackerURL: testTrackerURL})
	s.Now = func() time.Time { return now }

	rec := doNext(s.Handler(), testMachineName, testSessionToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionDeploy {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionDeploy)
	}
	if resp.Plan == nil || resp.Plan.OS == nil || resp.Plan.OS.Disk != "/dev/nvme0n1" {
		t.Fatalf("Plan = %+v, want an OS plan targeting /dev/nvme0n1", resp.Plan)
	}
	if len(resp.Plan.OS.Partitions) != 1 {
		t.Fatalf("len(Plan.OS.Partitions) = %d, want 1", len(resp.Plan.OS.Partitions))
	}
	if resp.Plan.SchemaVersion != agentapi.PlanSchemaVersion {
		t.Fatalf("Plan.SchemaVersion = %d, want %d", resp.Plan.SchemaVersion, agentapi.PlanSchemaVersion)
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionAgentCompatible)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("AgentCompatible condition = %+v, want True for a matching schema version", cond)
	}
}

// provisioningReadyMachineAndClient builds the same ready-to-deploy
// Machine/Image/ImageLayout fixture TestHandleNext_ProvisioningReadyReturnsDeployPlan
// uses, factored out so the compatibility-gate tests below can reuse it
// without duplicating the setup.
func provisioningReadyMachineAndClient(t *testing.T, now time.Time) (client.Client, *Server) {
	t.Helper()
	imageRef := keziov1alpha1.NameRef{Namespace: "default", Name: "os-image"}
	machine := newTestMachineWithSession(now)
	machine.Spec.ImageRef = &imageRef
	machine.Status.State = keziov1alpha1.MachineStateProvisioning
	machine.Status.Provisioning = &keziov1alpha1.MachineProvisioningStatus{
		Image: &keziov1alpha1.MachineProvisionedImage{
			ImageRef:   imageRef,
			TargetDisk: "/dev/nvme0n1",
		},
	}
	image, cm := readyImage("os-image", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
	})
	c := newPlanTestClient(t, machine, image, cm)
	s := New(c, Config{TrackerURL: testTrackerURL})
	s.Now = func() time.Time { return now }
	return c, s
}

// TestHandleNext_AbsentSchemaVersionHoldsAtWaitAndSetsIncompatibleCondition
// exercises the exact regression this gate exists for: an agent built
// before AgentSchemaVersionHeader existed sends no version at all. Even
// with every other readiness condition met (the fixture this test shares
// with TestHandleNext_ProvisioningReadyReturnsDeployPlan would otherwise
// hand out a plan), the server must answer ActionWait and record why.
func TestHandleNext_AbsentSchemaVersionHoldsAtWaitAndSetsIncompatibleCondition(t *testing.T) {
	now := time.Now()
	c, s := provisioningReadyMachineAndClient(t, now)

	rec := doNextWithSchemaVersion(s.Handler(), testMachineName, testSessionToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait || resp.Plan != nil {
		t.Fatalf("Action/Plan = %q/%+v, want %q/nil despite the deploy otherwise being ready", resp.Action, resp.Plan, agentapi.ActionWait)
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionAgentCompatible)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("AgentCompatible condition = %+v, want False for an agent advertising no schema version", cond)
	}
	if cond.Reason != "AgentIncompatible" {
		t.Fatalf("Reason = %q, want AgentIncompatible", cond.Reason)
	}
	if !strings.Contains(cond.Message, strconv.Itoa(agentapi.PlanSchemaVersion)) {
		t.Fatalf("Message = %q, want it to name the server's required schema version %d", cond.Message, agentapi.PlanSchemaVersion)
	}
}

// TestHandleNext_MismatchedSchemaVersionHoldsAtWait covers the other
// incompatible case: the agent advertises a version, but not the one the
// server requires.
func TestHandleNext_MismatchedSchemaVersionHoldsAtWait(t *testing.T) {
	now := time.Now()
	_, s := provisioningReadyMachineAndClient(t, now)

	rec := doNextWithSchemaVersion(s.Handler(), testMachineName, testSessionToken, strconv.Itoa(agentapi.PlanSchemaVersion+1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait || resp.Plan != nil {
		t.Fatalf("Action/Plan = %q/%+v, want %q/nil for a mismatched schema version", resp.Action, resp.Plan, agentapi.ActionWait)
	}
}

// TestHandleNext_CompatibleAgentClearsPriorIncompatibleCondition proves
// the condition lifecycle's other direction: once a compatible agent
// polls, a previously recorded incompatible condition flips back to
// True rather than being stuck False forever.
func TestHandleNext_CompatibleAgentClearsPriorIncompatibleCondition(t *testing.T) {
	now := time.Now()
	c, s := provisioningReadyMachineAndClient(t, now)

	// First poll: an old agent with no version, recording the
	// incompatible condition.
	doNextWithSchemaVersion(s.Handler(), testMachineName, testSessionToken, "")

	var midway keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &midway); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cond := apimeta.FindStatusCondition(midway.Status.Conditions, keziov1alpha1.MachineConditionAgentCompatible); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("AgentCompatible condition after the incompatible poll = %+v, want False", cond)
	}

	// Second poll: a compatible agent (a fresh live boot, or a reflashed
	// image) - the condition must flip to True and the plan must flow.
	rec := doNext(s.Handler(), testMachineName, testSessionToken)
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionDeploy || resp.Plan == nil {
		t.Fatalf("Action/Plan = %q/%+v, want %q with a plan once a compatible agent polls", resp.Action, resp.Plan, agentapi.ActionDeploy)
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionAgentCompatible)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("AgentCompatible condition after the compatible poll = %+v, want True", cond)
	}
}

func doProgress(t *testing.T, handler http.Handler, name, token string, body agentapi.ProgressRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, agentapi.NextPathPrefix+name+agentapi.ProgressPathSuffix, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHandleProgress_ValidSessionRecordsCondition(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, c := newTestServer(t, now, machine)

	rec := doProgress(t, s.Handler(), testMachineName, testSessionToken, agentapi.ProgressRequest{
		Partitions: []agentapi.PartitionProgress{
			{Disk: "/dev/nvme0n1", Number: 2, Phase: agentapi.PartitionPhaseSeeding, PercentDone: 42},
			{Disk: "/dev/nvme0n1", Number: 1, Phase: agentapi.PartitionPhaseDone, PercentDone: 100},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ProvisioningProgress condition = %+v, want True", cond)
	}
	if cond.Reason != agentapi.PartitionPhaseSeeding {
		t.Fatalf("Reason = %q, want %q (the furthest-behind partition, sorted before the done one)", cond.Reason, agentapi.PartitionPhaseSeeding)
	}
	if !strings.Contains(cond.Message, "seeding 42%") || !strings.Contains(cond.Message, "done 100%") {
		t.Fatalf("Message = %q, want it to mention both partitions", cond.Message)
	}
}

func TestHandleProgress_StepOverridesReasonWithTerminalSuccess(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, c := newTestServer(t, now, machine)

	rec := doProgress(t, s.Handler(), testMachineName, testSessionToken, agentapi.ProgressRequest{
		Partitions: []agentapi.PartitionProgress{
			{Disk: "/dev/nvme0n1", Number: 1, Phase: agentapi.PartitionPhaseDone, PercentDone: 100},
		},
		Step: agentapi.DeployStepRebootingToDisk,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ProvisioningProgress condition = %+v, want True", cond)
	}
	// Step, when the agent reports one, wins over the per-partition
	// furthest-phase reason summarizeProgress would otherwise compute -
	// here that would have been agentapi.PartitionPhaseDone, not the
	// terminal deploy-succeeded step.
	if cond.Reason != agentapi.DeployStepRebootingToDisk {
		t.Fatalf("Reason = %q, want the reported terminal step %q", cond.Reason, agentapi.DeployStepRebootingToDisk)
	}
}

func TestHandleProgress_StepMessageOverridesPartitionSummary(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, c := newTestServer(t, now, machine)

	rec := doProgress(t, s.Handler(), testMachineName, testSessionToken, agentapi.ProgressRequest{
		Step:        agentapi.DeployStepFinalizing,
		StepMessage: "creating UEFI boot entry",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil {
		t.Fatal("ProvisioningProgress condition was not set")
	}
	if cond.Reason != agentapi.DeployStepFinalizing {
		t.Fatalf("Reason = %q, want %q", cond.Reason, agentapi.DeployStepFinalizing)
	}
	if cond.Message != "creating UEFI boot entry" {
		t.Fatalf("Message = %q, want the agent-supplied StepMessage", cond.Message)
	}
}

func TestHandleProgress_RejectionsAreConstantShape(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		machine *keziov1alpha1.Machine
		reqName string
		token   string
	}{
		{
			name:    "missing Authorization header",
			machine: newTestMachineWithSession(now),
			reqName: testMachineName,
			token:   "",
		},
		{
			name:    "no such machine",
			machine: nil,
			reqName: "no-such-machine",
			token:   testSessionToken,
		},
		{
			name:    "wrong session token",
			machine: newTestMachineWithSession(now),
			reqName: testMachineName,
			token:   "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
	}

	var referenceBody string
	var referenceCode int
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var machines []*keziov1alpha1.Machine
			if tc.machine != nil {
				machines = append(machines, tc.machine)
			}
			s, _ := newTestServer(t, now, machines...)
			rec := doProgress(t, s.Handler(), tc.reqName, tc.token, agentapi.ProgressRequest{})

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if i == 0 {
				referenceCode = rec.Code
				referenceBody = rec.Body.String()
			} else if rec.Code != referenceCode || rec.Body.String() != referenceBody {
				t.Fatalf("response for %q differs from the reference 401 response (code=%d body=%q)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAgentJourney_RegisterThenNextThenProgress walks the real path an
// agent takes end to end: register with the boot token, capture the
// session token register actually mints, then use that same token (never
// a hardcoded fixture) to call next and progress. This guards the seam
// the other tests in this file don't: every next/progress test above
// seeds its Machine with newTestMachineWithSession, which hand-installs
// bootserver.HashToken(testSessionToken) directly via the fake client and
// so never exercises mintSessionToken()'s write path at all. A regression
// that mints and validates session hashes inconsistently would pass every
// one of those tests yet reject every real agent in the field.
func TestAgentJourney_RegisterThenNextThenProgress(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now)
	s, c := newTestServer(t, now, machine)
	handler := s.Handler()

	// Step 1: register with the boot token and capture the session token
	// the handler actually mints - not testSessionToken.
	regRec := doRegister(t, handler, testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if regRec.Code != http.StatusOK {
		t.Fatalf("register: status = %d, body = %s, want 200", regRec.Code, regRec.Body.String())
	}
	var regResp agentapi.RegisterResponse
	if err := json.Unmarshal(regRec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decoding register response: %v", err)
	}
	if regResp.SessionToken == "" {
		t.Fatal("register: SessionToken is empty; want a fresh session credential")
	}

	// Step 2: poll next using the register-minted token. The Machine is
	// still Inspecting (not Provisioning), so a clean 200 ActionWait
	// proves the token was accepted; only an unexpected 401 would signal
	// the mint-vs-validate seam is broken.
	nextRec := doNext(handler, testMachineName, regResp.SessionToken)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("next: status = %d, body = %s, want 200 (register-minted session token was rejected)", nextRec.Code, nextRec.Body.String())
	}
	var nextResp agentapi.NextResponse
	if err := json.Unmarshal(nextRec.Body.Bytes(), &nextResp); err != nil {
		t.Fatalf("decoding next response: %v", err)
	}
	if nextResp.Action != agentapi.ActionWait {
		t.Fatalf("next: Action = %q, want %q", nextResp.Action, agentapi.ActionWait)
	}

	// Step 3: report progress using the same register-minted token.
	progRec := doProgress(t, handler, testMachineName, regResp.SessionToken, agentapi.ProgressRequest{
		Partitions: []agentapi.PartitionProgress{
			{Disk: "/dev/nvme0n1", Number: 1, Phase: agentapi.PartitionPhaseSeeding, PercentDone: 50},
		},
	})
	if progRec.Code != http.StatusOK {
		t.Fatalf("progress: status = %d, body = %s, want 200 (register-minted session token was rejected)", progRec.Code, progRec.Body.String())
	}

	// Step 4: the Machine now carries both the hardware register ingested
	// and the progress condition recorded via the register-issued
	// session - proving state survived across all three real handler
	// calls, chained end to end.
	var stored keziov1alpha1.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Hardware == nil || len(stored.Status.Hardware.Disks) != 1 || stored.Status.Hardware.Disks[0].SerialNumber != "S123" {
		t.Fatalf("hardware inventory registered earlier in the journey did not survive: %+v", stored.Status.Hardware)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ProvisioningProgress condition = %+v, want True (recorded via the register-issued session)", cond)
	}
}

func TestHandleNext_RejectionsAreConstantShape(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		machine *keziov1alpha1.Machine
		reqName string
		token   string
	}{
		{
			name:    "missing Authorization header",
			machine: newTestMachineWithSession(now),
			reqName: testMachineName,
			token:   "",
		},
		{
			name:    "no such machine",
			machine: nil,
			reqName: "no-such-machine",
			token:   testSessionToken,
		},
		{
			name:    "wrong session token",
			machine: newTestMachineWithSession(now),
			reqName: testMachineName,
			token:   "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name: "expired session",
			machine: func() *keziov1alpha1.Machine {
				m := newTestMachineWithSession(now)
				m.Status.AgentSession.ExpiresAt = metav1.NewTime(now.Add(-time.Minute))
				return m
			}(),
			reqName: testMachineName,
			token:   testSessionToken,
		},
		{
			name: "no session minted yet",
			machine: func() *keziov1alpha1.Machine {
				m := newTestMachineWithSession(now)
				m.Status.AgentSession = nil
				return m
			}(),
			reqName: testMachineName,
			token:   testSessionToken,
		},
	}

	var referenceBody string
	var referenceCode int
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var machines []*keziov1alpha1.Machine
			if tc.machine != nil {
				machines = append(machines, tc.machine)
			}
			s, _ := newTestServer(t, now, machines...)
			rec := doNext(s.Handler(), tc.reqName, tc.token)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if i == 0 {
				referenceCode = rec.Code
				referenceBody = rec.Body.String()
			} else if rec.Code != referenceCode || rec.Body.String() != referenceBody {
				t.Fatalf("response for %q differs from the reference 401 response (code=%d body=%q)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}
