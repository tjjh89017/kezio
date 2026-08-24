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
	"reflect"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func newProgressTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha2.DeployRun{}).
		WithObjects(objs...).
		Build()
}

const progressTestRunName = "m1-run1"

func newProgressTestMachine() *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status: keziov1alpha2.MachineStatus{
			CurrentRunRef: &keziov1alpha2.NameRef{Name: progressTestRunName},
		},
	}
}

func newProgressTestRun(t *testing.T, c client.Client) *keziov1alpha2.DeployRun {
	t.Helper()
	run := &keziov1alpha2.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: progressTestRunName, Namespace: "default"},
		Spec:       keziov1alpha2.DeployRunSpec{MachineRef: keziov1alpha2.NameRef{Name: "m1"}},
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
		Step: keziov1alpha2.DeployRunPhasePartitioning, State: agentapi.ProgressStateRunning,
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha2.DeployRunPhasePartitioning {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha2.DeployRunPhasePartitioning)
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
			Step: keziov1alpha2.DeployRunPhaseWritingContent, State: agentapi.ProgressStateRunning,
			Timestamp: time.Now(),
		}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress[%d]: %v", i, err)
		}
	}

	var stored keziov1alpha2.DeployRun
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
		{keziov1alpha2.DeployRunPhasePartitioning, agentapi.ProgressStateRunning},
		{keziov1alpha2.DeployRunPhasePartitioning, agentapi.ProgressStateSucceeded},
		{keziov1alpha2.DeployRunPhaseWritingContent, agentapi.ProgressStateRunning},
	}
	for _, st := range steps {
		req := agentapi.ProgressRequest{RunName: run.Name, RunUID: string(run.UID), Step: st.step, State: st.state, Timestamp: time.Now()}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress(%s/%s): %v", st.step, st.state, err)
		}
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha2.DeployRunPhaseWritingContent {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha2.DeployRunPhaseWritingContent)
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
		Step: keziov1alpha2.DeployRunPhaseSucceeded, State: agentapi.ProgressStateSucceeded,
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha2.DeployRunPhaseSucceeded {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha2.DeployRunPhaseSucceeded)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha2.DeployRunConditionSucceeded)
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
		Step: keziov1alpha2.DeployRunPhasePartitioning, State: agentapi.ProgressStateFailed,
		Message:   "sfdisk rejected the partition table",
		Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != keziov1alpha2.DeployRunPhaseFailed {
		t.Fatalf("Phase = %q, want %q", stored.Status.Phase, keziov1alpha2.DeployRunPhaseFailed)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, keziov1alpha2.DeployRunConditionSucceeded)
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
			Step: keziov1alpha2.DeployRunPhaseWritingContent, State: agentapi.ProgressStateRunning,
			Partitions: partitions,
			Timestamp:  time.Now(),
		}
		if err := s.persistProgress(context.Background(), machine, req); err != nil {
			t.Fatalf("persistProgress[%d]: %v", i, err)
		}
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []keziov1alpha2.DeployRunPartitionProgress{
		{Number: 1, Percent: 100, BytesDone: 1000},
		{Number: 3, Percent: 100, BytesDone: 30},
	}
	if !reflect.DeepEqual(stored.Status.Partitions, want) {
		t.Fatalf("Partitions = %+v, want %+v", stored.Status.Partitions, want)
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

func TestPersistProgress_StaleRunUIDIsNoop(t *testing.T) {
	machine := newProgressTestMachine()
	c := newProgressTestClient(t, machine)
	run := newProgressTestRun(t, c)
	s := &Server{Client: c}

	req := agentapi.ProgressRequest{
		RunName: run.Name, RunUID: string(types.UID("stale-uid")),
		Step: keziov1alpha2.DeployRunPhasePartitioning, State: agentapi.ProgressStateRunning, Timestamp: time.Now(),
	}
	if err := s.persistProgress(context.Background(), machine, req); err != nil {
		t.Fatalf("persistProgress: %v", err)
	}

	var stored keziov1alpha2.DeployRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status.Phase != "" {
		t.Fatalf("Phase = %q, want untouched (empty) for a stale RunUID", stored.Status.Phase)
	}
}
