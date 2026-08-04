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
	"fmt"
	"strings"
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
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
