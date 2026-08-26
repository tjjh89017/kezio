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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// newLifecycleTestServer builds a controller stub serving a successful
// /agent/register and a "wait" /agent/next, recording whether register
// was called before the first next - the wiring Run is supposed to
// guarantee.
func newLifecycleTestServer(t *testing.T) (*httptest.Server, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	var registered atomic.Bool
	var nextBeforeRegister atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case agentapi.RegisterPath:
			registered.Store(true)
			_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{
				MachineName:  "node-01",
				SessionToken: "sess-token",
			})
		case agentapi.NextPath:
			if !registered.Load() {
				nextBeforeRegister.Store(true)
			}
			_ = json.NewEncoder(w).Encode(agentapi.NextResponse{Action: agentapi.ActionWait})
		}
	}))
	t.Cleanup(server.Close)
	return server, &registered, &nextBeforeRegister
}

// TestRun_CollectRegisterPollWiredTogether walks Run's whole boot journey
// - collect hardware inventory, register with the controller, then poll
// for the next action - through its real entry point, the same one
// cmd/agent/main.go calls. Testing Collect, Client.Register/Next and
// pollLoop in isolation (as the rest of this package's tests do) cannot
// catch a wiring bug between them (registerWithRetry never invoked, cfg
// fields dropped on the way in); only driving Run itself can.
func TestRun_CollectRegisterPollWiredTogether(t *testing.T) {
	server, registered, nextBeforeRegister := newLifecycleTestServer(t)
	cfg, lines := collectLogs()
	cfg.Cmdline = Cmdline{Server: server.URL, Token: "boot-token"}
	cfg.InventoryRoot = "testdata/host1"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("Run returned %v, want nil (it should run until ctx expires and then return cleanly)", err)
	}

	if !registered.Load() {
		t.Fatal("Run never called /agent/register")
	}
	if nextBeforeRegister.Load() {
		t.Fatal("Run polled /agent/next before registering")
	}

	var sawInventory, sawRegistered bool
	for _, l := range *lines {
		if strings.Contains(l, "collected inventory") {
			sawInventory = true
		}
		if strings.Contains(l, `registered as machine "node-01"`) {
			sawRegistered = true
		}
	}
	if !sawInventory {
		t.Errorf("logs = %v, want a log line about collected inventory", *lines)
	}
	if !sawRegistered {
		t.Errorf("logs = %v, want a log line about registering as node-01", *lines)
	}
}

// TestRun_MissingCmdlineIdlesInsteadOfExiting covers the negative case
// observed in the field: a Machine can legitimately net boot with no
// live token (see Run's doc comment). Run must log once and idle until
// ctx is cancelled rather than returning an error - a non-nil error
// here is what drove cmd/agent/main.go's log.Fatalf into a systemd
// restart loop.
func TestRun_MissingCmdlineIdlesInsteadOfExiting(t *testing.T) {
	cfg, lines := collectLogs()
	cfg.InventoryRoot = "testdata/host1"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("Run returned %v, want nil (idle until ctx is cancelled)", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("Run returned after %s, want it to have blocked until ctx expired", elapsed)
	}

	found := false
	for _, l := range *lines {
		if strings.Contains(l, "kezio.server=") && strings.Contains(l, "kezio.token=") {
			found = true
		}
	}
	if !found {
		t.Errorf("logs = %v, want one line mentioning the missing kezio.server=/kezio.token= cmdline values", *lines)
	}
}

// failNTimesThenRegister is a controller stub whose /agent/register
// handler fails the first n requests with a 500, then succeeds.
func failNTimesThenRegister(t *testing.T, n int) *httptest.Server {
	t.Helper()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= int32(n) {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: "not ready yet"})
			return
		}
		_ = json.NewEncoder(w).Encode(agentapi.RegisterResponse{
			MachineName:  "node-01",
			SessionToken: "sess-token",
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRegisterWithRetry_RetriesThenSucceeds proves registerWithRetry
// retries a failed registration rather than giving up, and returns the
// eventually-successful result. Config.RegisterRetryInterval is set to a
// few milliseconds so the test proves the retry behavior without the
// real 5s DefaultRegisterRetryInterval wait.
func TestRegisterWithRetry_RetriesThenSucceeds(t *testing.T) {
	server := failNTimesThenRegister(t, 2)
	client := NewClient(server.URL)
	cfg, lines := collectLogs()
	cfg.Cmdline = Cmdline{Token: "boot-token"}
	cfg.RegisterRetryInterval = 2 * time.Millisecond

	start := time.Now()
	result, err := registerWithRetry(context.Background(), client, cfg, keziov1alpha3.MachineHardwareSpec{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("registerWithRetry: %v", err)
	}
	if result.MachineName != "node-01" || result.SessionToken != "sess-token" {
		t.Fatalf("result = %+v, want the eventually-successful registration", result)
	}
	if elapsed > time.Second {
		t.Fatalf("registerWithRetry took %s, want it to use the configured millisecond retry interval, not a real multi-second wait", elapsed)
	}

	retryLogs := 0
	for _, l := range *lines {
		if strings.Contains(l, "registration failed, retrying") {
			retryLogs++
		}
	}
	if retryLogs != 2 {
		t.Errorf("saw %d retry log lines, want 2 (one per failed attempt)", retryLogs)
	}
}

// TestRegisterWithRetry_CtxCancelledReturnsPromptly proves an already
// cancelled ctx makes registerWithRetry return ctx.Err() immediately
// rather than blocking for a full retry interval - important because a
// long RegisterRetryInterval must not delay Run's shutdown on ctx
// cancellation.
func TestRegisterWithRetry_CtxCancelledReturnsPromptly(t *testing.T) {
	server := failNTimesThenRegister(t, 1000) // never succeeds
	client := NewClient(server.URL)
	cfg, _ := collectLogs()
	cfg.Cmdline = Cmdline{Token: "boot-token"}
	cfg.RegisterRetryInterval = time.Hour // would hang the test if not honored

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := registerWithRetry(ctx, client, cfg, keziov1alpha3.MachineHardwareSpec{})
	elapsed := time.Since(start)

	if err != context.Canceled {
		t.Fatalf("registerWithRetry error = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("registerWithRetry took %s to notice ctx cancellation, want it to return promptly", elapsed)
	}
}
