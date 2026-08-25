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
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func newProgressTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.DeployRun{}).
		WithObjects(objs...).
		Build()
}

const progressTestRunName = "m1-run1"

func newProgressTestMachine() *keziov1alpha3.Machine {
	return &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status: keziov1alpha3.MachineStatus{
			CurrentRunRef: &keziov1alpha3.NameRef{Name: progressTestRunName},
		},
	}
}

func newProgressTestRun(t *testing.T, c client.Client) *keziov1alpha3.DeployRun {
	t.Helper()
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: progressTestRunName, Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "m1"}},
	}
	if err := c.Create(context.Background(), run); err != nil {
		t.Fatalf("create DeployRun: %v", err)
	}
	return run
}

func TestPersistProgress_RunningOpensPhaseTiming(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhasePartitioning, State: agentapi.ProgressStateRunning,
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhasePartitioning {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha3.DeployRunPhasePartitioning)
	}
	if len(stored.Status.PhaseTimings) != 1 || stored.Status.PhaseTimings[0].FinishedAt != nil {
		t.Fatalf("PhaseTimings = %+v, want one open entry", stored.Status.PhaseTimings)
	}
}

func TestPersistProgress_RepeatedRunningDoesNotDuplicateTiming(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	for i := 0; i < 3; i++ {
		req := agentapi.ProgressRequest{
			RunName: run.Name, RunUID: string(run.UID),
			Step: keziov1alpha3.DeployRunPhaseWritingContent, State: agentapi.ProgressStateRunning,
			Timestamp: time.Now(),
		}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress[%d]: %v", i, err)
		}
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Status.PhaseTimings) != 1 {
		t.Fatalf("PhaseTimings = %+v, want exactly one entry for repeated running reports of the same phase", stored.Status.PhaseTimings)
	}
}

func TestPersistProgress_PhaseTransitionClosesPreviousTiming(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	steps := []struct {
		step  string
		state string
	}{
		{keziov1alpha3.DeployRunPhasePartitioning, agentapi.ProgressStateRunning},
		{keziov1alpha3.DeployRunPhasePartitioning, agentapi.ProgressStateSucceeded},
		{keziov1alpha3.DeployRunPhaseWritingContent, agentapi.ProgressStateRunning},
	}
	for _, st := range steps {
		req := agentapi.ProgressRequest{RunName: run.Name, RunUID: string(run.UID), Step: st.step, State: st.state, Timestamp: time.Now()}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress(%s/%s): %v", st.step, st.state, err)
		}
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhaseWritingContent {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha3.DeployRunPhaseWritingContent)
	}
	if len(stored.Status.PhaseTimings) != 2 {
		t.Fatalf("PhaseTimings = %+v, want two entries", stored.Status.PhaseTimings)
	}
	if stored.Status.PhaseTimings[0].FinishedAt == nil {
		t.Error("PhaseTimings[0] (Partitioning) is not closed")
	}
	if stored.Status.PhaseTimings[1].FinishedAt != nil {
		t.Error("PhaseTimings[1] (WritingContent) is already closed, want still open")
	}
}

func TestPersistProgress_TerminalSucceededSetsPhaseAndCondition(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhaseSucceeded, State: agentapi.ProgressStateSucceeded,
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhaseSucceeded {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha3.DeployRunPhaseSucceeded)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha3.DeployRunConditionSucceeded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Succeeded condition = %+v, want True", cond)
	}
}

func TestPersistProgress_FailedSetsPhaseFailedWithMessage(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhasePartitioning, State: agentapi.ProgressStateFailed,
		Message:   "sfdisk rejected the partition table",
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhaseFailed {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha3.DeployRunPhaseFailed)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha3.DeployRunConditionSucceeded)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != "sfdisk rejected the partition table" {
		t.Fatalf("Succeeded condition = %+v, want False with the failure message", cond)
	}
	if len(stored.Status.PhaseTimings) != 1 || stored.Status.PhaseTimings[0].FinishedAt == nil {
		t.Fatalf("PhaseTimings = %+v, want the Partitioning entry closed", stored.Status.PhaseTimings)
	}
}

func TestPersistProgress_PartitionsAreUpsertedByNumber(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	reports := [][]agentapi.ProgressPartition{
		{{Number: 1, Percent: 40, BytesDone: 400}},
		{{Number: 1, Percent: 100, BytesDone: 1000}, {Number: 3, Percent: 100, BytesDone: 30}},
	}
	for i, partitions := range reports {
		req := agentapi.ProgressRequest{
			RunName: run.Name, RunUID: string(run.UID),
			Step: keziov1alpha3.DeployRunPhaseWritingContent, State: agentapi.ProgressStateRunning,
			Partitions: partitions,
			Timestamp:  time.Now(),
		}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress[%d]: %v", i, err)
		}
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []keziov1alpha3.DeployRunPartitionProgress{
		{Number: 1, Percent: 100, BytesDone: 1000},
		{Number: 3, Percent: 100, BytesDone: 30},
	}
	if !reflect.DeepEqual(stored.Status.Partitions, want) {
		t.Fatalf("Partitions = %+v, want %+v", stored.Status.Partitions, want)
	}
}

func TestPersistProgress_StampsLastProgressAtFromServerClock(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	// The agent's own clock is deliberately far off: lastProgressAt must
	// come from this process's clock, not req.Timestamp.
	serverNow := time.Now()
	progressNow = func() time.Time { return serverNow }
	t.Cleanup(func() { progressNow = time.Now })

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhaseWritingContent, State: agentapi.ProgressStateRunning,
		Timestamp: serverNow.Add(-24 * time.Hour),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.LastProgressAt == nil {
		t.Fatal("LastProgressAt = nil, want it stamped on every accepted report")
	}
	if !stored.Status.LastProgressAt.Time.Equal(serverNow.Truncate(time.Second)) {
		t.Fatalf("LastProgressAt = %v, want the server clock's %v, not the agent's timestamp", stored.Status.LastProgressAt.Time, serverNow)
	}
}

func TestPersistProgress_MissingRunIsNoop(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{RunName: "does-not-exist", RunUID: "x", Step: "Partitioning", State: agentapi.ProgressStateRunning, Timestamp: time.Now()}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v, want nil for a missing DeployRun", err)
	}
}

// laggingReadClient serves DeployRun reads from a frozen snapshot,
// standing in for the manager's informer cache lagging behind a write
// that already landed in the API server. Writes go through to the real
// store, so an Update built on the snapshot conflicts exactly as it does
// in production. staleReads bounds how many reads are served stale;
// -1 means every read, forever.
type laggingReadClient struct {
	client.Client
	stale      *keziov1alpha3.DeployRun
	staleReads int
}

func (c *laggingReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	run, ok := obj.(*keziov1alpha3.DeployRun)
	if !ok || c.staleReads == 0 || key.Name != c.stale.Name {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	if c.staleReads > 0 {
		c.staleReads--
	}
	c.stale.DeepCopyInto(run)
	return nil
}

// driveToFinalizing replays the report sequence deploy.Executor.Execute
// sends before its terminal one, leaving run at the Finalizing phase.
func driveToFinalizing(t *testing.T, s *Server, machine *keziov1alpha3.Machine, run *keziov1alpha3.DeployRun) {
	t.Helper()
	steps := []struct{ step, state string }{
		{keziov1alpha3.DeployRunPhaseRunningPostHook, agentapi.ProgressStateRunning},
		{keziov1alpha3.DeployRunPhaseRunningPostHook, agentapi.ProgressStateSucceeded},
		{keziov1alpha3.DeployRunPhaseFinalizing, agentapi.ProgressStateRunning},
		{keziov1alpha3.DeployRunPhaseFinalizing, agentapi.ProgressStateSucceeded},
	}
	for _, st := range steps {
		req := agentapi.ProgressRequest{
			RunName: run.Name, RunUID: string(run.UID),
			Step: st.step, State: st.state, Timestamp: time.Now(),
		}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress(%s/%s): %v", st.step, st.state, err)
		}
	}
}

func terminalSucceededRequest(run *keziov1alpha3.DeployRun) agentapi.ProgressRequest {
	return agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhaseSucceeded, State: agentapi.ProgressStateSucceeded,
		Timestamp: time.Now(),
	}
}

func assertRunSucceeded(t *testing.T, c client.Client, run *keziov1alpha3.DeployRun) {
	t.Helper()
	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha3.DeployRunPhaseSucceeded {
		t.Fatalf("Phase = %q, want %q: the terminal report was dropped, so the Machine polls this run forever",
			stored.Status.Phase, keziov1alpha3.DeployRunPhaseSucceeded)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha3.DeployRunConditionSucceeded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Succeeded condition = %+v, want True", cond)
	}
}

func TestPersistProgress_TerminalSucceededSurvivesAStaleRead(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	ctx := context.Background()

	// Snapshot the run one report before the last one Execute sends
	// ahead of its terminal report: this is what a cache that has not
	// yet observed the Finalizing-succeeded write still hands back.
	s := &Server{Client: c}
	driveToFinalizing(t, s, machine, run)
	var stale keziov1alpha3.DeployRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &stale); err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhaseFinalizing, State: agentapi.ProgressStateRunning,
		Partitions: []agentapi.ProgressPartition{{Number: 1, Percent: 100}},
		Timestamp:  time.Now(),
	}
	if err := s.persistProgress(ctx, machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	lagging := &Server{Client: &laggingReadClient{Client: c, stale: &stale, staleReads: 1}}
	if err := lagging.persistProgress(ctx, machine, terminalSucceededRequest(run)); err != nil {
		t.Fatalf("persistProgress(terminal): %v", err)
	}
	assertRunSucceeded(t, c, run)
}

// TestPersistProgress_ExhaustedRetriesReportTheConflict is the stale-read
// case taken past what a retry can fix: the cache never catches up, so
// every attempt conflicts. The retry reads through the cache, so this
// bound is real - what must not happen is losing the report quietly.
// persistProgress has to return the conflict so its caller logs a
// terminal report it failed to record, rather than answering as though
// the write landed.
func TestPersistProgress_ExhaustedRetriesReportTheConflict(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	ctx := context.Background()

	s := &Server{Client: c}
	driveToFinalizing(t, s, machine, run)
	var stale keziov1alpha3.DeployRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &stale); err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(run.UID),
		Step: keziov1alpha3.DeployRunPhaseFinalizing, State: agentapi.ProgressStateRunning,
		Partitions: []agentapi.ProgressPartition{{Number: 1, Percent: 100}},
		Timestamp:  time.Now(),
	}
	if err := s.persistProgress(ctx, machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	lagging := &Server{Client: &laggingReadClient{Client: c, stale: &stale, staleReads: -1}}
	err := lagging.persistProgress(ctx, machine, terminalSucceededRequest(run))
	if err == nil {
		t.Fatal("persistProgress(terminal) = nil, want the conflict: a terminal report that never landed must not be reported as persisted")
	}
	if !apierrors.IsConflict(errors.Unwrap(err)) {
		t.Fatalf("persistProgress(terminal) = %v, want a wrapped conflict", err)
	}
}

func TestPersistProgress_StaleRunUIDIsNoop(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(types.UID("stale-uid")),
		Step: keziov1alpha3.DeployRunPhasePartitioning, State: agentapi.ProgressStateRunning, Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha3.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != "" {
		t.Fatalf("Phase = %q, want untouched (empty) for a stale RunUID", stored.Status.Phase)
	}
}
