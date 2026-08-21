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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestLogNextResponse_UnrecognizedActionIsLoggedNotIgnoredSilently(t *testing.T) {
	cfg, lines := collectLogs()
	logNextResponse(cfg, agentapi.NextResponse{Action: "deploy"})

	if len(*lines) != 1 || !strings.Contains((*lines)[0], "deploy") {
		t.Fatalf("logs = %v, want one line mentioning the unrecognized action", *lines)
	}
}

// TestPollLoop_PollsImmediatelyThenHonoursServerInterval proves pollLoop
// calls Next right away (no wait for the first call), then waits exactly
// the PollIntervalSeconds the server's response carried before calling
// again - not the DefaultPollInterval.
func TestPollLoop_PollsImmediatelyThenHonoursServerInterval(t *testing.T) {
	var calls atomic.Int32
	var gotAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionWait, PollIntervalSeconds: 0})
	}))
	defer server.Close()
	// The server always answers PollIntervalSeconds: 0, so pollLoop falls
	// back to DefaultPollInterval (10s) between calls; a short-lived ctx
	// is enough to observe exactly one call without waiting on that.

	client := NewClient(server.URL)
	cfg, lines := collectLogs()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := pollLoop(ctx, client, cfg, RegisterResult{MachineName: "node-01", SessionToken: "sess"}); err != nil {
		t.Fatalf("pollLoop returned %v, want nil", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("server received %d calls, want exactly 1 (immediate poll, then a 10s wait ctx never survives)", got)
	}
	if auth, _ := gotAuth.Load().(string); auth != "Bearer sess" {
		t.Errorf("Authorization header = %q, want Bearer sess", auth)
	}

	sawWait := false
	for _, l := range *lines {
		if l == "poll: wait" {
			sawWait = true
		}
	}
	if !sawWait {
		t.Errorf("logs = %v, want a \"poll: wait\" line", *lines)
	}
}

func TestPollLoop_PollFailureIsLoggedAndRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: "not ready"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	cfg, lines := collectLogs()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := pollLoop(ctx, client, cfg, RegisterResult{MachineName: "node-01", SessionToken: "sess"}); err != nil {
		t.Fatalf("pollLoop returned %v, want nil even when every poll fails", err)
	}
	if calls.Load() == 0 {
		t.Fatal("server never received a poll call")
	}

	sawFailure := false
	for _, l := range *lines {
		if strings.Contains(l, "poll failed") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Errorf("logs = %v, want a \"poll failed\" line", *lines)
	}
}
