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

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func TestClient_Register_ReturnsMachineNameAndSessionToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{
			MachineName:  "node-01",
			SessionToken: "sess-token",
		})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	result, err := c.Register(context.Background(), "boot-token", &keziov1alpha1.MachineHardwareStatus{})
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
}

func TestClient_Register_RejectsResponseMissingSessionToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{MachineName: "node-01"})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if _, err := c.Register(context.Background(), "boot-token", &keziov1alpha1.MachineHardwareStatus{}); err == nil {
		t.Fatal("Register: want an error for a response with no session token")
	}
}

func TestClient_Next_SendsSessionTokenAndParsesDeployPlan(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(agentapi.NextResponse{
			Action: agentapi.ActionDeploy,
			Plan: &agentapi.DeployPlan{
				OS: &agentapi.ImageDeployPlan{
					ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
					Disk:     "/dev/nvme0n1",
					Partitions: []agentapi.PlanPartition{
						{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
					},
				},
				AfterDeploy: keziov1alpha1.AfterDeployReboot,
			},
		})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	resp, err := c.Next(context.Background(), "node-01", "sess-token")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if gotAuth != "Bearer sess-token" {
		t.Errorf("Authorization header = %q, want Bearer sess-token", gotAuth)
	}
	if gotPath != "/agent/machines/node-01/next" {
		t.Errorf("request path = %q, want /agent/machines/node-01/next", gotPath)
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
}

func TestClient_ReportProgress_SendsSessionTokenAndBody(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotReq agentapi.ProgressRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(agentapi.ProgressResponse{})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	req := agentapi.ProgressRequest{
		Partitions: []agentapi.PartitionProgress{
			{Disk: "/dev/nvme0n1", Number: 2, Phase: agentapi.PartitionPhaseSeeding, PercentDone: 55},
		},
	}
	if err := c.ReportProgress(context.Background(), "node-01", "sess-token", req); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer sess-token" {
		t.Errorf("Authorization header = %q, want Bearer sess-token", gotAuth)
	}
	if gotPath != "/agent/machines/node-01/progress" {
		t.Errorf("request path = %q, want /agent/machines/node-01/progress", gotPath)
	}
	if len(gotReq.Partitions) != 1 || gotReq.Partitions[0].PercentDone != 55 {
		t.Fatalf("request body = %+v, want the posted partition progress", gotReq)
	}
}

func TestClient_ReportProgress_NonOKStatusIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: "unauthorized"})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	if err := c.ReportProgress(context.Background(), "node-01", "sess-token", agentapi.ProgressRequest{}); err == nil {
		t.Fatal("ReportProgress: want an error for a 401 response")
	}
}

func TestClient_Next_WaitResponseHasNoPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionWait})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	resp, err := c.Next(context.Background(), "node-01", "sess-token")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionWait)
	}
	if resp.Plan != nil {
		t.Fatalf("Plan = %+v, want nil alongside ActionWait", resp.Plan)
	}
}
