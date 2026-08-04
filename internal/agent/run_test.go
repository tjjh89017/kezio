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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// collectLogs returns a Config whose Logf appends every formatted
// message to the returned slice.
func collectLogs() (Config, *[]string) {
	var lines []string
	cfg := Config{Logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	return cfg, &lines
}

func TestLogNextResponse_Wait(t *testing.T) {
	cfg, lines := collectLogs()
	logNextResponse(cfg, agentapi.NextResponse{Action: agentapi.ActionWait})

	if len(*lines) != 1 || (*lines)[0] != "poll: wait" {
		t.Fatalf("logs = %v, want [\"poll: wait\"]", *lines)
	}
}

func TestLogNextResponse_DeployLogsDiskAndPartitionCount(t *testing.T) {
	cfg, lines := collectLogs()
	resp := agentapi.NextResponse{
		Action: agentapi.ActionDeploy,
		Plan: &agentapi.DeployPlan{
			OS: &agentapi.ImageDeployPlan{
				ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
				Disk:     "/dev/nvme0n1",
				Partitions: []agentapi.PlanPartition{
					{Number: 1, Device: "/dev/nvme0n1p1"},
					{Number: 2, Device: "/dev/nvme0n1p2"},
				},
			},
			DataImages: []agentapi.ImageDeployPlan{
				{
					ImageRef:   keziov1alpha1.NameRef{Name: "data-image"},
					Disk:       "/dev/sda",
					Partitions: []agentapi.PlanPartition{{Number: 1, Device: "/dev/sda1"}},
				},
			},
		},
	}
	logNextResponse(cfg, resp)

	if len(*lines) != 2 {
		t.Fatalf("logs = %v, want 2 lines (one per image plan)", *lines)
	}
	if !strings.Contains((*lines)[0], "/dev/nvme0n1") || !strings.Contains((*lines)[0], "2") {
		t.Errorf("OS log line = %q, want it to mention /dev/nvme0n1 and 2 partitions", (*lines)[0])
	}
	if !strings.Contains((*lines)[1], "/dev/sda") || !strings.Contains((*lines)[1], "1") {
		t.Errorf("data image log line = %q, want it to mention /dev/sda and 1 partition", (*lines)[1])
	}
}

// countingRunner is a deploy.Runner that always succeeds and counts how
// many times it ran.
type countingRunner struct {
	calls atomic.Int32
}

func (r *countingRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	r.calls.Add(1)
	return []byte("107374182400\n"), nil // 100 GiB, comfortably large for blockdev --getsize64
}

// noContentPlan is a one-partition, blank-only plan: no torrent, no
// ezio, so pollLoop's deploy path stays exercisable without a fake
// ezioapi server.
func noContentPlan() *agentapi.DeployPlan {
	const fixtureSfdiskJSON = `{"partitiontable":{"label":"gpt","sectorsize":512,"partitions":[{"node":"/dev/loop0p1","start":2048,"size":2048,"type":"0FC63DAF-8483-4772-8E79-3D69D8477DE4"}]}}`
	return &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       "/dev/nvme0n1",
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath("/dev/nvme0n1", 1), Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
			},
		},
		AfterDeploy: keziov1alpha1.AfterDeployReboot,
	}
}

// newDeployTestServer builds a controller stub serving one "deploy"
// response from GET .../next and recording every POST .../progress
// call's partition count.
func newDeployTestServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var progressCalls atomic.Int32
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/next"):
			_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionDeploy, Plan: noContentPlan()})
		case strings.HasSuffix(r.URL.Path, "/progress"):
			progressCalls.Add(1)
			_ = json.NewEncoder(w).Encode(agentapi.ProgressResponse{})
		}
	}))
	t.Cleanup(server.Close)
	return server, &progressCalls
}

func TestPollLoop_ExecutesDeployPlanOnceAndReportsProgress(t *testing.T) {
	server, progressCalls := newDeployTestServer(t)
	client := NewClient(server.URL)
	runner := &countingRunner{}
	executor := &deploy.Executor{
		Runner:       runner,
		Ezio:         nil, // never consulted: noContentPlan has no content partition
		PollInterval: time.Millisecond,
	}
	cfg, _ := collectLogs()
	cfg.Executor = executor

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if err := pollLoop(ctx, client, cfg, RegisterResult{MachineName: "node-01", SessionToken: "sess"}, 20*time.Millisecond); err != nil {
		t.Fatalf("pollLoop returned %v, want nil (it should run until ctx expires and then return cleanly)", err)
	}

	// blockdev --getsize64 + sfdisk + partprobe + mkfs.ext4 + (finalize:
	// no ESP partition here, so no efibootmgr calls) + systemctl reboot
	// = 5 Runner calls for exactly one execution; if the plan were
	// re-executed on a later poll (the controller keeps answering
	// "deploy" every time in this stub) the count would be a multiple of
	// 5 - assert the exact count to catch that regression.
	if got := runner.calls.Load(); got != 5 {
		t.Fatalf("Runner was called %d times, want exactly 5 (one deploy execution, not re-applied on later polls)", got)
	}
	if progressCalls.Load() == 0 {
		t.Fatal("no progress was ever reported to the controller")
	}
}

func TestPollLoop_NilExecutorNeverExecutes(t *testing.T) {
	server, _ := newDeployTestServer(t)
	client := NewClient(server.URL)
	cfg, lines := collectLogs()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_ = pollLoop(ctx, client, cfg, RegisterResult{MachineName: "node-01", SessionToken: "sess"}, 20*time.Millisecond)

	for _, l := range *lines {
		if strings.Contains(l, "executed") {
			t.Fatalf("log line %q suggests a plan was executed despite a nil Executor", l)
		}
	}
}
