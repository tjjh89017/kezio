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

package agentserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// newTestServer builds a Server backed by a fake client seeded with
// machines, indexed the same way SetupFieldIndexer configures a real
// manager cache.
func newTestServer(t *testing.T, now time.Time, objs ...client.Object) (*Server, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha2.Machine{}, MachineTokenHashIndexField, IndexMachineTokenHash).
		WithIndex(&keziov1alpha2.Machine{}, MachineSessionTokenHashIndexField, IndexMachineSessionTokenHash).
		WithStatusSubresource(&keziov1alpha2.Machine{}).
		WithObjects(objs...)
	c := builder.Build()

	s := New(c, Config{})
	s.Now = func() time.Time { return now }
	return s, c
}

const testMachineName = "node-01"

// newTestMachine builds a Machine named testMachineName with a live,
// unexpired boot token whose hash is bootserver.HashToken(testToken).
func newTestMachine(now time.Time) *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName, UID: "machine-uid-01"},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: testMachineName + "-bmc"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			SubnetRef:      keziov1alpha2.NameRef{Name: "subnet-01"},
		},
		Status: keziov1alpha2.MachineStatus{
			State: keziov1alpha2.MachineStateInspecting,
			NetBoot: &keziov1alpha2.MachineNetBootStatus{
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
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func doNext(handler http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, agentapi.NextPath, nil)
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sampleInventory() keziov1alpha2.MachineHardwareSpec {
	rotational := false
	return keziov1alpha2.MachineHardwareSpec{
		Disks: []keziov1alpha2.MachineHardwareDisk{
			{DeviceName: "/dev/nvme0n1", SerialNumber: "S123", SizeBytes: 512 << 30, Rotational: &rotational},
		},
		Nics: []keziov1alpha2.MachineHardwareNIC{
			{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:01"},
		},
		MemoryBytes: 16 << 30,
		CPUCount:    8,
	}
}

// TestServer_NeedLeaderElectionIsFalse guards against a regression that
// would leave POST /agent/register unanswered during a rolling update -
// see bootserver.TestServer_NeedLeaderElectionIsFalse's doc comment for
// the full reasoning; this server needs the identical guarantee.
func TestServer_NeedLeaderElectionIsFalse(t *testing.T) {
	s, _ := newTestServer(t, time.Now())
	if s.NeedLeaderElection() {
		t.Fatal("Server.NeedLeaderElection() = true; want false so a rolling update does not leave /agent/register unanswered until the old lease expires")
	}
}

func TestHandleRegister_ValidTokenCreatesHardwareMintsSessionAndInvalidatesToken(t *testing.T) {
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
	if resp.MachineName != testMachineName {
		t.Fatalf("MachineName = %q, want %q", resp.MachineName, testMachineName)
	}
	if resp.SessionToken == "" {
		t.Fatal("SessionToken is empty; want a fresh session credential")
	}
	if resp.SessionTTLSeconds != int64(DefaultSessionTTL.Seconds()) {
		t.Fatalf("SessionTTLSeconds = %d, want %d", resp.SessionTTLSeconds, int64(DefaultSessionTTL.Seconds()))
	}

	// MachineHardware was created, name-aligned and controller-owned.
	var hw keziov1alpha2.MachineHardware
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &hw); err != nil {
		t.Fatalf("Get MachineHardware: %v", err)
	}
	if len(hw.Spec.Disks) != 1 || hw.Spec.Disks[0].SerialNumber != "S123" {
		t.Fatalf("MachineHardware.Spec.Disks = %+v, want the reported inventory", hw.Spec.Disks)
	}
	if len(hw.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %+v, want exactly one", hw.OwnerReferences)
	}
	owner := hw.OwnerReferences[0]
	if owner.Name != testMachineName || owner.Kind != "Machine" || owner.UID != machine.UID {
		t.Fatalf("owner reference = %+v, want a reference to Machine %q (uid %q)", owner, testMachineName, machine.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Fatalf("owner reference Controller = %v, want true", owner.Controller)
	}

	var stored keziov1alpha2.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get Machine: %v", err)
	}
	if stored.Status.NetBoot.TokenHash != "" {
		t.Fatalf("token hash was not invalidated after a successful registration: %q", stored.Status.NetBoot.TokenHash)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha2.MachineConditionAgentRegistered)
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

func TestHandleRegister_UpdatesExistingMachineHardware(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now)
	existing := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Spec: keziov1alpha2.MachineHardwareSpec{
			MemoryBytes: 1 << 30,
			CPUCount:    1,
		},
	}
	s, c := newTestServer(t, now, machine, existing)

	rec := doRegister(t, s.Handler(), testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var hw keziov1alpha2.MachineHardware
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &hw); err != nil {
		t.Fatalf("Get MachineHardware: %v", err)
	}
	if hw.Spec.CPUCount != 8 || hw.Spec.MemoryBytes != 16<<30 {
		t.Fatalf("MachineHardware.Spec was not updated: %+v", hw.Spec)
	}
	if len(hw.OwnerReferences) != 1 || hw.OwnerReferences[0].Name != testMachineName {
		t.Fatalf("OwnerReferences = %+v, want it set on update too", hw.OwnerReferences)
	}
}

func TestHandleRegister_RejectionsAreConstantShape(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		machine *keziov1alpha2.Machine
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
			var objs []client.Object
			if tc.machine != nil {
				objs = append(objs, tc.machine)
			}
			s, _ := newTestServer(t, now, objs...)
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

func TestHandleRegister_ExpiredTokenDoesNotInvalidateOrCreateHardware(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now.Add(-time.Hour)) // already expired
	s, c := newTestServer(t, now, machine)

	rec := doRegister(t, s.Handler(), testToken, agentapi.RegisterRequest{Hardware: sampleInventory()})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var stored keziov1alpha2.Machine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.NetBoot == nil || stored.Status.NetBoot.TokenHash == "" {
		t.Fatalf("expired token's hash was cleared despite the rejection: %+v", stored.Status.NetBoot)
	}

	var hw keziov1alpha2.MachineHardware
	err := c.Get(context.Background(), types.NamespacedName{Name: testMachineName}, &hw)
	if err == nil {
		t.Fatal("MachineHardware was created despite an expired token")
	}
}

func TestHandleRegister_MalformedBodyIsBadRequest(t *testing.T) {
	now := time.Now()
	machine := newTestMachine(now)
	s, _ := newTestServer(t, now, machine)

	req := httptest.NewRequest(http.MethodPost, agentapi.RegisterPath, bytes.NewReader([]byte("not json")))
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

const testSessionToken = "session-0123456789abcdef0123456789abcdef0123456789abcdef01234567"

// newTestMachineWithSession builds a Machine named testMachineName with
// a live, unexpired agent session (hash of testSessionToken), and no
// net-boot token (already consumed, as it would be by the time a real
// agent starts polling).
func newTestMachineWithSession(now time.Time) *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: testMachineName + "-bmc"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			SubnetRef:      keziov1alpha2.NameRef{Name: "subnet-01"},
		},
		Status: keziov1alpha2.MachineStatus{
			State: keziov1alpha2.MachineStateAvailable,
			AgentSession: &keziov1alpha2.MachineAgentSessionStatus{
				TokenHash: bootserver.HashToken(testSessionToken),
				ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
			},
		},
	}
}

func TestHandleNext_ValidSessionReturnsActionWait(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)

	rec := doNext(s.Handler(), testSessionToken)
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
	if resp.PollIntervalSeconds != int32(DefaultPollInterval.Seconds()) {
		t.Fatalf("PollIntervalSeconds = %d, want %d", resp.PollIntervalSeconds, int32(DefaultPollInterval.Seconds()))
	}
}

func TestHandleNext_POSTAlsoAnswers(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)

	req := httptest.NewRequest(http.MethodPost, agentapi.NextPath, nil)
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	req.Header.Set("Authorization", "Bearer "+testSessionToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestHandleNext_RejectionsAreConstantShape(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		machine *keziov1alpha2.Machine
		token   string
	}{
		{
			name:    "missing Authorization header",
			machine: newTestMachineWithSession(now),
			token:   "",
		},
		{
			name:    "unknown session token",
			machine: newTestMachineWithSession(now),
			token:   "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name:    "expired session token",
			machine: newTestMachineWithSession(now.Add(-2 * time.Hour)),
			token:   testSessionToken,
		},
		{
			name:    "boot token cannot authenticate next",
			machine: newTestMachine(now),
			token:   testToken,
		},
	}

	var referenceBody string
	var referenceCode int
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, now, tc.machine)
			rec := doNext(s.Handler(), tc.token)

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

// TestSchemaVersionGuard_EveryRoute exercises requireAgentSchemaVersion
// against every route this server exposes: a missing, lower, or higher
// AgentSchemaVersionHeader is rejected with a 4xx and an ErrorResponse
// before the route handler ever runs.
func TestSchemaVersionGuard_EveryRoute(t *testing.T) {
	now := time.Now()

	newRequest := func(route, method, header string) *http.Request {
		req := httptest.NewRequest(method, route, bytes.NewReader([]byte("{}")))
		if header != "" {
			req.Header.Set(agentapi.AgentSchemaVersionHeader, header)
		}
		req.Header.Set("Authorization", "Bearer "+testSessionToken)
		return req
	}

	routes := []struct {
		name   string
		route  string
		method string
	}{
		{name: "register", route: agentapi.RegisterPath, method: http.MethodPost},
		{name: "next", route: agentapi.NextPath, method: http.MethodGet},
		{name: "progress", route: agentapi.ProgressPath, method: http.MethodPost},
	}

	headerCases := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{name: "matching", header: strconv.Itoa(agentapi.AgentSchemaVersion), wantErr: false},
		{name: "missing", header: "", wantErr: true},
		{name: "lower", header: strconv.Itoa(agentapi.AgentSchemaVersion - 1), wantErr: true},
		{name: "higher", header: strconv.Itoa(agentapi.AgentSchemaVersion + 1), wantErr: true},
		{name: "non-numeric", header: "bogus", wantErr: true},
	}

	for _, route := range routes {
		for _, hc := range headerCases {
			t.Run(route.name+"/"+hc.name, func(t *testing.T) {
				machine := newTestMachineWithSession(now)
				s, _ := newTestServer(t, now, machine)

				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, newRequest(route.route, route.method, hc.header))

				if hc.wantErr {
					if rec.Code < 400 || rec.Code >= 500 {
						t.Fatalf("status = %d, want a 4xx for header %q", rec.Code, hc.header)
					}
					var resp agentapi.ErrorResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error == "" {
						t.Fatalf("body = %s, want a decodable ErrorResponse with a non-empty Error", rec.Body.String())
					}
					return
				}
				// A matching header must pass the guard: register's
				// route-specific auth still needs the register token, not
				// the session token this helper sends, so a 401 here is
				// still "the guard let it through", not a repeat of the
				// missing-header 400 shape.
				if route.name == "register" && rec.Code != http.StatusUnauthorized {
					t.Fatalf("register status = %d, want 401 (wrong-kind token, but past the schema guard)", rec.Code)
				}
				if route.name != "register" && rec.Code == http.StatusBadRequest {
					t.Fatalf("%s status = %d, want the schema guard to pass for a matching header", route.name, rec.Code)
				}
			})
		}
	}
}

func doProgress(handler http.Handler, token string, body agentapi.ProgressRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, agentapi.ProgressPath, bytes.NewReader(b))
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHandleProgress_MissingOrUnknownTokenIsUnauthorized(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)

	req := agentapi.ProgressRequest{RunName: "run-1", RunUID: "uid-1", Step: "WritingContent", State: agentapi.ProgressStateRunning}

	rec := doProgress(s.Handler(), "", req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.Code)
	}

	rec = doProgress(s.Handler(), "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: status = %d, want 401", rec.Code)
	}
}

func TestHandleProgress_MalformedBodyIsBadRequest(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)

	req := httptest.NewRequest(http.MethodPost, agentapi.ProgressPath, bytes.NewReader([]byte("not json")))
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	req.Header.Set("Authorization", "Bearer "+testSessionToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleProgress_DefaultsToContinue(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)
	// s.Abort is left nil.

	rec := doProgress(s.Handler(), testSessionToken, agentapi.ProgressRequest{
		RunName: "run-1", RunUID: "uid-1", Step: "WritingContent", State: agentapi.ProgressStateRunning,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.ProgressResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ProgressActionContinue {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ProgressActionContinue)
	}
}

func TestHandleProgress_InjectedDeciderCanAbort(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now)
	s, _ := newTestServer(t, now, machine)

	var gotMachine, gotRunName, gotRunUID string
	s.Abort = func(_ context.Context, machineName, runName, runUID string) bool {
		gotMachine, gotRunName, gotRunUID = machineName, runName, runUID
		return runUID == "abort-me"
	}

	rec := doProgress(s.Handler(), testSessionToken, agentapi.ProgressRequest{
		RunName: "run-1", RunUID: "abort-me", Step: "WritingContent", State: agentapi.ProgressStateRunning,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.ProgressResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ProgressActionAbort {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ProgressActionAbort)
	}
	if gotMachine != testMachineName || gotRunName != "run-1" || gotRunUID != "abort-me" {
		t.Fatalf("decider saw (%q, %q, %q), want (%q, %q, %q)", gotMachine, gotRunName, gotRunUID, testMachineName, "run-1", "abort-me")
	}

	// A run the decider does not flag still continues.
	rec = doProgress(s.Handler(), testSessionToken, agentapi.ProgressRequest{
		RunName: "run-1", RunUID: "keep-going", Step: "WritingContent", State: agentapi.ProgressStateRunning,
	})
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ProgressActionContinue {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ProgressActionContinue)
	}
}
