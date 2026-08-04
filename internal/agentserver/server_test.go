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
		WithStatusSubresource(&keziov1alpha1.Machine{})
	for _, m := range machines {
		builder = builder.WithObjects(m)
	}
	c := builder.Build()

	s := New(c, Config{})
	s.Now = func() time.Time { return now }
	return s, c
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

func TestHandleNext_AlwaysWait(t *testing.T) {
	now := time.Now()
	s, _ := newTestServer(t, now)

	req := httptest.NewRequest(http.MethodGet, agentapi.NextPathPrefix+"node-01"+agentapi.NextPathSuffix, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionWait)
	}
}
