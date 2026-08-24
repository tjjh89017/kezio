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

package deploy

import (
	"context"
	"slices"
	"testing"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

func TestReporterHelpers(t *testing.T) {
	rec := &RecordingReporter{}
	e := &Executor{Progress: rec}
	plan := basicPlan()

	e.reportRunning(context.Background(), plan, "Partitioning")
	e.reportProgress(context.Background(), plan, "WritingContent", 42, 4200,
		[]agentapi.ProgressPartition{{Number: 3, Percent: 42, BytesDone: 4200}})
	e.reportSucceeded(context.Background(), plan, "WritingContent", "done")
	e.reportFailed(context.Background(), plan, "Finalizing", "boom")
	e.reportPartitionComplete(context.Background(), plan, "WritingContent", 1)

	if len(rec.Reports) != 5 {
		t.Fatalf("got %d reports, want 5", len(rec.Reports))
	}

	r0 := rec.Reports[0]
	if r0.RunName != plan.RunName || r0.RunUID != plan.RunUID {
		t.Errorf("report run identity = %q/%q, want %q/%q", r0.RunName, r0.RunUID, plan.RunName, plan.RunUID)
	}
	if r0.State != agentapi.ProgressStateRunning || r0.Step != "Partitioning" {
		t.Errorf("running report = %+v", r0)
	}

	r1 := rec.Reports[1]
	if r1.PercentDone == nil || *r1.PercentDone != 42 {
		t.Errorf("progress report percent = %v, want 42", r1.PercentDone)
	}
	if r1.BytesDone == nil || *r1.BytesDone != 4200 {
		t.Errorf("progress report bytes = %v, want 4200", r1.BytesDone)
	}
	wantPartitions := []agentapi.ProgressPartition{{Number: 3, Percent: 42, BytesDone: 4200}}
	if !slices.Equal(r1.Partitions, wantPartitions) {
		t.Errorf("progress report partitions = %+v, want %+v", r1.Partitions, wantPartitions)
	}

	r2 := rec.Reports[2]
	if r2.State != agentapi.ProgressStateSucceeded || r2.Message != "done" {
		t.Errorf("succeeded report = %+v", r2)
	}

	r3 := rec.Reports[3]
	if r3.State != agentapi.ProgressStateFailed || r3.Message != "boom" {
		t.Errorf("failed report = %+v", r3)
	}

	r4 := rec.Reports[4]
	wantComplete := []agentapi.ProgressPartition{{Number: 1, Percent: 100}}
	if r4.State != agentapi.ProgressStateRunning || !slices.Equal(r4.Partitions, wantComplete) {
		t.Errorf("partition-complete report = %+v, want the step still running with %+v", r4, wantComplete)
	}
}

func TestNilReporterIsSafe(t *testing.T) {
	e := &Executor{}
	// Every report* helper must be a no-op, not a panic, when Progress
	// is nil - production always wires one, but tests exercising other
	// paths may not.
	e.reportRunning(context.Background(), basicPlan(), "Partitioning")
}

func TestNopReporter(t *testing.T) {
	var r NopReporter
	if err := r.Report(context.Background(), agentapi.ProgressRequest{}); err != nil {
		t.Errorf("NopReporter.Report: %v", err)
	}
}
