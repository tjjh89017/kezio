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
	"time"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

// Reporter streams a running deploy's progress to the controller, one
// agentapi.ProgressRequest per call. Execute calls it at least twice per
// phase (running, then succeeded or failed) plus, while content is being
// written, once per seeding-status poll tick. A failure to report is
// logged and otherwise ignored by Execute - a missed update does not
// itself justify failing an in-progress deployment; only cancelling the
// context Execute runs under does that. This package never inspects a
// ProgressResponse itself: a Reporter implementation (internal/agent's
// HTTPReporter) is the one that watches for the controller's abort
// action and cancels on it.
type Reporter interface {
	Report(ctx context.Context, req agentapi.ProgressRequest) error
}

// NopReporter discards every report. The zero value is ready to use.
type NopReporter struct{}

// Report implements Reporter.
func (NopReporter) Report(context.Context, agentapi.ProgressRequest) error { return nil }

// RecordingReporter is a Reporter that appends every report it receives,
// for tests that need to assert what Execute reported.
type RecordingReporter struct {
	Reports []agentapi.ProgressRequest
}

// Report implements Reporter.
func (r *RecordingReporter) Report(_ context.Context, req agentapi.ProgressRequest) error {
	r.Reports = append(r.Reports, req)
	return nil
}

// now is overridden in tests so a report's Timestamp is deterministic.
var now = time.Now

// report sends one ProgressRequest built from plan's run identity plus
// the given step/state/message/percent and per-partition detail, through
// e.Progress. A delivery error is logged, not propagated - see Reporter's
// doc comment.
func (e *Executor) report(ctx context.Context, plan *agentapi.DeployPlan, step, state, message string, percentDone, bytesDone *int64, partitions []agentapi.ProgressPartition) {
	if e.Progress == nil {
		return
	}
	req := agentapi.ProgressRequest{
		RunName:    plan.RunName,
		RunUID:     plan.RunUID,
		Step:       step,
		State:      state,
		Message:    message,
		Partitions: partitions,
		Timestamp:  now(),
	}
	if percentDone != nil {
		p := int32(*percentDone) //nolint:gosec // callers clamp to [0,100]
		req.PercentDone = &p
	}
	if bytesDone != nil {
		req.BytesDone = bytesDone
	}
	if err := e.Progress.Report(ctx, req); err != nil {
		e.log("reporting progress failed: %v", err)
	}
}

// reportRunning reports step as having just started.
func (e *Executor) reportRunning(ctx context.Context, plan *agentapi.DeployPlan, step string) {
	e.report(ctx, plan, step, agentapi.ProgressStateRunning, "", nil, nil, nil)
}

// reportProgress reports step still running, with the fine-grained detail
// a content-writing tick carries: the run-wide percent/bytes, and each
// named partition's own share of it.
func (e *Executor) reportProgress(ctx context.Context, plan *agentapi.DeployPlan, step string, percentDone, bytesDone int64, partitions []agentapi.ProgressPartition) {
	e.report(ctx, plan, step, agentapi.ProgressStateRunning, "", &percentDone, &bytesDone, partitions)
}

// reportPartitionComplete reports one partition as fully written, while
// step itself is still running. It is how a partition whose content is
// laid down by a single command (mkfs, mkswap) reaches 100: unlike a
// torrent slot, it has no intermediate progress to poll for.
func (e *Executor) reportPartitionComplete(ctx context.Context, plan *agentapi.DeployPlan, step string, number int32) {
	e.report(ctx, plan, step, agentapi.ProgressStateRunning, "", nil, nil,
		[]agentapi.ProgressPartition{{Number: number, Percent: 100}})
}

// reportSucceeded reports step as finished successfully.
func (e *Executor) reportSucceeded(ctx context.Context, plan *agentapi.DeployPlan, step, message string) {
	e.report(ctx, plan, step, agentapi.ProgressStateSucceeded, message, nil, nil, nil)
}

// reportFailed reports step as failed - Execute's terminal report on any
// failing return (see Execute's doc comment).
func (e *Executor) reportFailed(ctx context.Context, plan *agentapi.DeployPlan, step, message string) {
	e.report(ctx, plan, step, agentapi.ProgressStateFailed, message, nil, nil, nil)
}
