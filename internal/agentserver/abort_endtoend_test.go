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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agent"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// abortE2ERunName is the fixed DeployRun name TestAbortEndToEnd_* uses -
// no GenerateName is needed since the test creates it directly.
const abortE2ERunName = "abort-e2e-run"

// abortE2EFixtureSfdisk mirrors internal/agent/deploy's own basicPlan
// fixture: a single torrent slot is enough to drive Execute into
// seedUntilStopped, the only point in a deploy that reports progress
// (and therefore learns of an abort) on a tight, test-friendly loop
// instead of once per whole phase.
const abortE2EFixtureSfdisk = `{"partitiontable":{"label":"gpt","id":"11111111-1111-1111-1111-111111111111","sectorsize":512,"partitions":[
  {"node":"/dev/loop0p1","start":2048,"size":2048,"type":"0FC63DAF-8483-4772-8E79-3D69D8477DE4"}
]}}`

// abortE2ERunner is a deploy.Runner that succeeds every command; the
// blockdev size query answers comfortably large so Execute's validate
// step never blocks on it.
type abortE2ERunner struct{}

func (abortE2ERunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	if name == "blockdev" {
		return []byte("107374182400\n"), nil // 100 GiB
	}
	return nil, nil
}

func (r abortE2ERunner) RunEnv(ctx context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, stdin, name, args...)
}

// abortE2EEzioClient is a deploy.EzioClient whose torrent never finishes
// on its own - only the deploy's own context being cancelled ever ends
// the seeding loop that polls it.
type abortE2EEzioClient struct {
	mu    sync.Mutex
	added int
}

func (c *abortE2EEzioClient) AddTorrent(context.Context, []byte, string, bool, int32, int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.added++
	return nil
}

func (c *abortE2EEzioClient) addedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.added
}

func (c *abortE2EEzioClient) GetTorrentStatus(context.Context, []string) (map[string]seeder.Torrent, error) {
	return map[string]seeder.Torrent{"deadbeef": {Hash: "deadbeef", IsFinished: false, Total: 100, TotalDone: 10}}, nil
}

func (c *abortE2EEzioClient) PauseTorrent(context.Context, string) error { return nil }
func (c *abortE2EEzioClient) Shutdown(context.Context) error             { return nil }

// abortE2ELauncher is a deploy.EzioLauncher recording whether Stop was
// called - the assertion that a cancelled deploy actually stops its
// local torrents, not merely that Execute returns an error.
type abortE2ELauncher struct {
	client     deploy.EzioClient
	mu         sync.Mutex
	stopCalled bool
}

func (l *abortE2ELauncher) Launch(context.Context) (deploy.EzioHandle, error) {
	return deploy.EzioHandle{Client: l.client, Stop: func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.stopCalled = true
		return nil
	}}, nil
}

func (l *abortE2ELauncher) stopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopCalled
}

// abortE2EFetcher is a deploy.TorrentFetcher returning fixed bytes for
// any URL.
type abortE2EFetcher struct{}

func (abortE2EFetcher) FetchTorrent(context.Context, string) ([]byte, error) {
	return []byte("torrent-bytes"), nil
}

// progressRecorder wraps an http.Handler, recording every decoded
// POST /agent/progress body it forwards - the only way this test can
// observe that the agent's final failed report actually reached the
// controller over the wire, since the DeployRun it names is deleted
// before that report is sent and so leaves nothing for persistProgress
// to write.
type progressRecorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []agentapi.ProgressRequest
}

func (p *progressRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == agentapi.ProgressPath && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req agentapi.ProgressRequest
		if json.Unmarshal(body, &req) == nil {
			p.mu.Lock()
			p.reqs = append(p.reqs, req)
			p.mu.Unlock()
		}
	}
	p.next.ServeHTTP(w, r)
}

func (p *progressRecorder) reports() []agentapi.ProgressRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agentapi.ProgressRequest(nil), p.reqs...)
}

// TestAbortEndToEnd_DeletingRunMidWriteStopsAgentAndReportsFailed drives
// the whole abort channel as one connected path: a real POST
// /agent/progress handler backed by NewAbortDecider, a real
// deploy.Executor writing content over a fake torrent client, and a real
// agent.HTTPReporter/agent.Client in between - the same three processes
// (controller, agent, network) the unit tests around each of them stub
// out individually. Deleting the DeployRun mid-write must make the
// executor's context cancel, its local ezio daemon stop, and its
// deferred final failed report still land on the controller despite
// that same cancellation.
func TestAbortEndToEnd_DeletingRunMidWriteStopsAgentAndReportsFailed(t *testing.T) {
	now := time.Now()
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Status: keziov1alpha3.MachineStatus{
			CurrentRunRef: &keziov1alpha3.NameRef{Name: abortE2ERunName},
		},
	}
	s, c := newTestServer(t, now, machine)
	s.Abort = NewAbortDecider(c)

	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: abortE2ERunName}}
	if err := c.Create(context.Background(), run); err != nil {
		t.Fatalf("create DeployRun: %v", err)
	}

	sessionToken, sessionHash, err := mintSessionToken()
	if err != nil {
		t.Fatalf("mintSessionToken: %v", err)
	}
	machine.Status.AgentSession = &keziov1alpha3.MachineAgentSessionStatus{
		TokenHash: sessionHash,
		ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
	}
	if err := c.Status().Update(context.Background(), machine); err != nil {
		t.Fatalf("recording agent session: %v", err)
	}

	recorder := &progressRecorder{next: s.Handler()}
	srv := httptest.NewServer(recorder)
	defer srv.Close()

	reporter := &agent.HTTPReporter{
		Client:       agent.NewClient(srv.URL),
		SessionToken: sessionToken,
		RetryDelay:   time.Millisecond,
	}
	deployCtx, cancel := context.WithCancel(context.Background())
	reporter.Cancel = cancel

	ezioClient := &abortE2EEzioClient{}
	launcher := &abortE2ELauncher{client: ezioClient}
	executor := &deploy.Executor{
		Runner:       abortE2ERunner{},
		Ezio:         launcher,
		Fetcher:      abortE2EFetcher{},
		Progress:     reporter,
		PollInterval: 5 * time.Millisecond,
	}
	plan := &agentapi.DeployPlan{
		SchemaVersion: agentapi.AgentSchemaVersion,
		RunName:       run.Name,
		RunUID:        string(run.UID),
		MachineName:   testMachineName,
		TargetDisk:    "/dev/sda",
		SfdiskScript:  abortE2EFixtureSfdisk,
		Slots: []agentapi.DeploySlot{
			{Number: 1, Device: "/dev/sda1", Torrent: &agentapi.DeployTorrent{URL: "http://seeder/root.torrent", InfoHash: "deadbeef"}},
		},
		AfterDeploy: keziov1alpha3.AfterDeployReboot,
	}

	execErr := make(chan error, 1)
	go func() { execErr <- executor.Execute(deployCtx, plan) }()

	// Wait for the deploy to actually reach seeding (AddTorrent called)
	// before deleting the run out from under it - deleting any earlier
	// would race the executor's own validate/writePartitionTable steps,
	// which this test is not exercising.
	deadline := time.Now().Add(2 * time.Second)
	for ezioClient.addedCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for AddTorrent to be called")
		}
		time.Sleep(time.Millisecond)
	}

	if err := c.Delete(context.Background(), run); err != nil {
		t.Fatalf("delete DeployRun: %v", err)
	}

	select {
	case err := <-execErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute returned %v, want context.Canceled from the aborted seeding loop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Execute to return after the DeployRun was deleted")
	}

	if !launcher.stopped() {
		t.Error("EzioHandle.Stop was never called: the agent's local torrents outlived the aborted deploy")
	}

	reports := recorder.reports()
	if len(reports) == 0 {
		t.Fatal("no progress reports reached the controller")
	}
	last := reports[len(reports)-1]
	if last.State != agentapi.ProgressStateFailed {
		t.Fatalf("final progress report State = %q, want %q - the deferred failed report never reached the controller", last.State, agentapi.ProgressStateFailed)
	}
	if last.Message == "" {
		t.Error("final failed report carries no message")
	}
	if last.RunName != run.Name {
		t.Errorf("final failed report RunName = %q, want %q", last.RunName, run.Name)
	}
}
