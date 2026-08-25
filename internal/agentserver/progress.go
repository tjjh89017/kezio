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
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// persistProgress applies req onto the DeployRun it names, in machine's
// namespace. It is a no-op - not an error - when that DeployRun does not
// exist (already GC'd or never created) or its UID does not match
// req.RunUID (a report from a run already superseded by a fresh one of
// the same name): a same-name DeployRun created after the one a stale
// report names must never have that report's progress applied to it.
//
// The read-modify-write retries on conflict, re-reading each time.
// deploy.Executor.Execute sends its last four reports (a phase's
// succeeded, the next phase's running/succeeded, then the terminal
// DeployRunPhaseSucceeded) back to back with no work between them, so
// the cache this server reads through is routinely still serving the
// state before the previous report's write, and the update built on it
// is rejected. Without a retry that rejection is only logged - by
// handleProgress here and by the agent's Reporter - and DeployRun
// status carries the *only* record that the deploy finished, so losing
// the terminal report strands the Machine in Provisioning forever
// (AgentDeployer.Provision reports Continuing for every non-terminal
// phase, with no deadline). The retry reads through the cache like the
// first attempt: it converges as the cache catches up, which is the
// same watch event that would have made the first read correct.
func (s *Server) persistProgress(ctx context.Context, machine *keziov1alpha2.Machine, req agentapi.ProgressRequest) error {
	key := client.ObjectKey{Namespace: machine.Namespace, Name: req.RunName}

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var run keziov1alpha2.DeployRun
		if err := s.Client.Get(ctx, key, &run); err != nil {
			return err
		}
		if req.RunUID != "" && string(run.UID) != req.RunUID {
			return nil
		}
		applyProgressToDeployRun(&run, req)
		return s.Client.Status().Update(ctx, &run)
	})
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return nil
	default:
		return fmt.Errorf("persisting progress onto DeployRun %q: %w", req.RunName, err)
	}
}

// applyProgressToDeployRun maps one ProgressRequest onto run's status in
// place: run.status.phase, partitions, phaseTimings, and - on a terminal
// report - the DeployRunConditionSucceeded condition. It mirrors what
// internal/agent/deploy.Executor emits: reportRunning/reportSucceeded
// pairs for every ordinary phase, and a single, Running-less
// reportSucceeded(DeployRunPhaseSucceeded) as the whole run's terminal
// success signal (see Execute's doc comment) - so a phase transition is
// detected by req.Step differing from the run's current phase, not by
// req.State alone.
func applyProgressToDeployRun(run *keziov1alpha2.DeployRun, req agentapi.ProgressRequest) {
	ts := metav1.NewTime(req.Timestamp)
	enterPhase(run, req.Step, ts)
	applyPartitionProgress(run, req.Partitions)

	switch req.State {
	case agentapi.ProgressStateSucceeded:
		closeCurrentPhaseTiming(run, ts)
		if req.Step == keziov1alpha2.DeployRunPhaseSucceeded {
			setSucceededCondition(run, metav1.ConditionTrue, "DeploySucceeded", progressMessage(req.Message, "deploy completed successfully"))
		}
	case agentapi.ProgressStateFailed:
		closeCurrentPhaseTiming(run, ts)
		run.Status.Phase = keziov1alpha2.DeployRunPhaseFailed
		setSucceededCondition(run, metav1.ConditionFalse, "DeployFailed", progressMessage(req.Message, "deploy failed"))
	}
}

// applyPartitionProgress upserts each reported partition onto
// run.Status.Partitions, keyed by partition number: an entry already
// there is replaced (a report is a snapshot, not a delta - see
// agentapi.ProgressRequest.Partitions), and a partition number not seen
// before is appended, so the list grows in the order the agent first
// reported each partition. A report naming no partition leaves the list
// untouched.
func applyPartitionProgress(run *keziov1alpha2.DeployRun, partitions []agentapi.ProgressPartition) {
	for _, p := range partitions {
		entry := keziov1alpha2.DeployRunPartitionProgress{
			Number:    p.Number,
			Percent:   p.Percent,
			BytesDone: p.BytesDone,
		}
		if i := slices.IndexFunc(run.Status.Partitions, func(e keziov1alpha2.DeployRunPartitionProgress) bool {
			return e.Number == p.Number
		}); i >= 0 {
			run.Status.Partitions[i] = entry
			continue
		}
		run.Status.Partitions = append(run.Status.Partitions, entry)
	}
}

// enterPhase records run entering step, if it is not already the run's
// current phase: closes the previous phase's timing entry and opens a
// new one for step. A repeated report for the phase already current
// (every progress tick during content writing, and a phase's own
// "succeeded" report) is a no-op here by design - see
// applyProgressToDeployRun's doc comment.
func enterPhase(run *keziov1alpha2.DeployRun, step string, ts metav1.Time) {
	if run.Status.Phase == step {
		return
	}
	closeCurrentPhaseTiming(run, ts)
	run.Status.Phase = step
	run.Status.PhaseTimings = append(run.Status.PhaseTimings, keziov1alpha2.DeployRunPhaseTiming{
		Phase:     step,
		StartedAt: ts,
	})
}

// closeCurrentPhaseTiming sets FinishedAt on the last PhaseTimings entry,
// if any and not already closed. Idempotent: closing an already-closed
// entry is a no-op.
func closeCurrentPhaseTiming(run *keziov1alpha2.DeployRun, ts metav1.Time) {
	n := len(run.Status.PhaseTimings)
	if n == 0 {
		return
	}
	if run.Status.PhaseTimings[n-1].FinishedAt == nil {
		run.Status.PhaseTimings[n-1].FinishedAt = &ts
	}
}

// setSucceededCondition sets DeployRunConditionSucceeded on run.
func setSucceededCondition(run *keziov1alpha2.DeployRun, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.DeployRunConditionSucceeded,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
}

// progressMessage returns msg, or fallback when msg is empty - the
// terminal success report's Message is ordinarily empty (see
// deploy.Executor.Execute).
func progressMessage(msg, fallback string) string {
	if msg != "" {
		return msg
	}
	return fallback
}
