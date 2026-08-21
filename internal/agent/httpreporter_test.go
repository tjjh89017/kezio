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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

func progressHandler(t *testing.T, respond func(req agentapi.ProgressRequest) agentapi.ProgressResponse) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var req agentapi.ProgressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respond(req))
	}
}

func TestHTTPReporter_ContinuePath(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		progressHandler(t, func(agentapi.ProgressRequest) agentapi.ProgressResponse {
			return agentapi.ProgressResponse{Action: agentapi.ProgressActionContinue}
		})(w, r)
	}))
	defer srv.Close()

	cancelled := false
	r := &HTTPReporter{
		Client:       NewClient(srv.URL),
		SessionToken: "tok-1",
		Cancel:       func() { cancelled = true },
	}

	err := r.Report(context.Background(), agentapi.ProgressRequest{RunName: "run1", Step: "Partitioning", State: agentapi.ProgressStateRunning})
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if gotToken != "Bearer tok-1" {
		t.Errorf("Authorization header = %q, want Bearer tok-1", gotToken)
	}
	if cancelled {
		t.Error("Cancel was called on a continue response")
	}
}

func TestHTTPReporter_AbortCancelsExactlyOnce(t *testing.T) {
	srv := httptest.NewServer(progressHandler(t, func(agentapi.ProgressRequest) agentapi.ProgressResponse {
		return agentapi.ProgressResponse{Action: agentapi.ProgressActionAbort}
	}))
	defer srv.Close()

	var cancelCount atomic.Int32
	r := &HTTPReporter{
		Client:       NewClient(srv.URL),
		SessionToken: "tok-1",
		Cancel:       func() { cancelCount.Add(1) },
	}

	for i := range 3 {
		if err := r.Report(context.Background(), agentapi.ProgressRequest{RunName: "run1", Step: "WritingContent", State: agentapi.ProgressStateRunning}); err != nil {
			t.Fatalf("Report()[%d] error = %v", i, err)
		}
	}
	if got := cancelCount.Load(); got != 1 {
		t.Fatalf("Cancel called %d times, want exactly 1", got)
	}
}

func TestHTTPReporter_FinalFailedReportSentAfterAbort(t *testing.T) {
	var gotSteps []string
	srv := httptest.NewServer(progressHandler(t, func(req agentapi.ProgressRequest) agentapi.ProgressResponse {
		gotSteps = append(gotSteps, req.Step+"/"+req.State)
		return agentapi.ProgressResponse{Action: agentapi.ProgressActionAbort}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r := &HTTPReporter{Client: NewClient(srv.URL), SessionToken: "tok-1", Cancel: cancel}

	// First report triggers abort and cancels ctx.
	if err := r.Report(ctx, agentapi.ProgressRequest{RunName: "run1", Step: "WritingContent", State: agentapi.ProgressStateRunning}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx was not cancelled after an abort response")
	}

	// The deploy's deferred final report is sent with the now-cancelled
	// ctx: it must still reach the controller.
	if err := r.Report(ctx, agentapi.ProgressRequest{RunName: "run1", Step: "WritingContent", State: agentapi.ProgressStateFailed, Message: "context canceled"}); err != nil {
		t.Fatalf("final Report() after ctx cancellation error = %v, want nil (must still be sent)", err)
	}

	if len(gotSteps) != 2 || gotSteps[1] != "WritingContent/failed" {
		t.Fatalf("server saw steps %v, want the final failed report to land", gotSteps)
	}
}

func TestHTTPReporter_TransportErrorsRetriedAndBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &HTTPReporter{
		Client:       NewClient(srv.URL),
		SessionToken: "tok-1",
		RetryDelay:   time.Millisecond,
	}

	start := time.Now()
	err := r.Report(context.Background(), agentapi.ProgressRequest{RunName: "run1", Step: "Partitioning", State: agentapi.ProgressStateRunning})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Report() error = nil, want an error after every attempt failed")
	}
	if got := calls.Load(); got != httpReporterMaxAttempts {
		t.Fatalf("server saw %d calls, want exactly %d (bounded retries)", got, httpReporterMaxAttempts)
	}
	if elapsed > time.Second {
		t.Fatalf("Report() took %s, want it bounded by a short retry delay", elapsed)
	}
}
