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

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func TestClient_Register_ReturnsMachineNameAndSessionToken(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{
			MachineName:  "node-01",
			SessionToken: "sess-token",
		})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	result, err := c.Register(context.Background(), "boot-token", keziov1alpha2.MachineHardwareSpec{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.MachineName != "node-01" {
		t.Errorf("MachineName = %q, want node-01", result.MachineName)
	}
	if result.SessionToken != "sess-token" {
		t.Errorf("SessionToken = %q, want sess-token", result.SessionToken)
	}
	if gotAuth != "Bearer boot-token" {
		t.Errorf("Authorization header = %q, want Bearer boot-token", gotAuth)
	}
	if gotPath != agentapi.RegisterPath {
		t.Errorf("request path = %q, want %q", gotPath, agentapi.RegisterPath)
	}
}

func TestClient_Register_RejectsResponseMissingSessionToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{MachineName: "node-01"})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if _, err := c.Register(context.Background(), "boot-token", keziov1alpha2.MachineHardwareSpec{}); err == nil {
		t.Fatal("Register: want an error for a response with no session token")
	}
}

func TestClient_Register_NonOKStatusIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: "unauthorized"})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if _, err := c.Register(context.Background(), "boot-token", keziov1alpha2.MachineHardwareSpec{}); err == nil {
		t.Fatal("Register: want an error for a 401 response")
	}
}

func TestClient_Next_SendsSessionTokenAndParsesResponse(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionWait, PollIntervalSeconds: 7})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	resp, err := c.Next(context.Background(), "sess-token")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotAuth != "Bearer sess-token" {
		t.Errorf("Authorization header = %q, want Bearer sess-token", gotAuth)
	}
	if gotPath != agentapi.NextPath {
		t.Errorf("request path = %q, want %q", gotPath, agentapi.NextPath)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionWait)
	}
	if resp.PollIntervalSeconds != 7 {
		t.Errorf("PollIntervalSeconds = %d, want 7", resp.PollIntervalSeconds)
	}
}

// TestClient_AdvertisesSchemaVersion guards the server-side compatibility
// gate's other half: internal/agentserver can only reason about a
// version-mismatched agent if this client actually sends its supported
// version on every request.
func TestClient_AdvertisesSchemaVersion(t *testing.T) {
	var gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get(agentapi.AgentSchemaVersionHeader)
		_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionWait})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if _, err := c.Next(context.Background(), "sess-token"); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if want := strconv.Itoa(agentapi.AgentSchemaVersion); gotVersion != want {
		t.Errorf("%s header = %q, want %q", agentapi.AgentSchemaVersionHeader, gotVersion, want)
	}
}

func TestClient_Next_NonOKStatusIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: "unauthorized"})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if _, err := c.Next(context.Background(), "sess-token"); err == nil {
		t.Fatal("Next: want an error for a 401 response")
	}
}
