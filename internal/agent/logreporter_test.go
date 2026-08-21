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
	"strings"
	"testing"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

func TestLogReporter_Report(t *testing.T) {
	cfg, lines := collectLogs()
	r := LogReporter{Logf: cfg.Logf}

	if err := r.Report(context.Background(), agentapi.ProgressRequest{Step: "Partitioning", State: "running"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if err := r.Report(context.Background(), agentapi.ProgressRequest{Step: "Finalizing", State: "failed", Message: "boom"}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	if len(*lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(*lines))
	}
	if !strings.Contains((*lines)[1], "boom") {
		t.Errorf("failed report line = %q, want it to carry the message", (*lines)[1])
	}
}

func TestLogReporter_NilLogfIsSafe(t *testing.T) {
	var r LogReporter
	if err := r.Report(context.Background(), agentapi.ProgressRequest{}); err != nil {
		t.Errorf("Report: %v", err)
	}
}
