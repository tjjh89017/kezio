/*
Copyright 2026 Date Huang.

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
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// partitionKey identifies one partition across every image plan in a
// DeployPlan: a (disk, number) pair is unique within a single deploy,
// since validate already rejects two image plans sharing a disk.
type partitionKey struct {
	disk   string
	number int32
}

// trackedPartition is one partition's latest known phase/percent, held
// by progressTracker.
type trackedPartition struct {
	phase   string
	percent int32
}

// progressTracker holds Execute's whole-plan progress state: one entry
// per partition, in plan order, plus an index from a content partition's
// BitTorrent info hash back to its partition key so the seeding loop
// (which only sees hashes, from GetTorrentStatus) can update the right
// entry. It is not safe for concurrent use - Execute drives it from a
// single goroutine throughout.
type progressTracker struct {
	order      []partitionKey
	partitions map[partitionKey]*trackedPartition
	hashKey    map[string]partitionKey
	// step is the whole-plan step reported alongside per-partition
	// detail (see agentapi.ProgressRequest.Step). Empty until setStep is
	// first called, matching the wire type's "no overall step to report"
	// zero value.
	step string
}

// newProgressTracker builds a tracker with one zero-value entry
// (phase="", percent=0) per partition across plans, and indexes every
// content partition's info hash up front.
func newProgressTracker(plans []agentapi.ImageDeployPlan) *progressTracker {
	t := &progressTracker{
		partitions: make(map[partitionKey]*trackedPartition),
		hashKey:    make(map[string]partitionKey),
	}
	for _, ip := range plans {
		for _, p := range ip.Partitions {
			k := partitionKey{disk: ip.Disk, number: p.Number}
			t.order = append(t.order, k)
			t.partitions[k] = &trackedPartition{}
			if classify(p) == kindContent {
				t.hashKey[p.InfoHash] = k
			}
		}
	}
	return t
}

// hasContent reports whether the tracked plan has at least one content
// partition - the signal Execute uses to decide whether to launch a
// local ezio daemon at all.
func (t *progressTracker) hasContent() bool {
	return len(t.hashKey) > 0
}

// torrentHashes returns every tracked content partition's info hash, for
// GetTorrentStatus's hash filter.
func (t *progressTracker) torrentHashes() []string {
	hashes := make([]string, 0, len(t.hashKey))
	for h := range t.hashKey {
		hashes = append(hashes, h)
	}
	return hashes
}

// setPhase sets the (disk, number) partition's phase/percent directly -
// used for the non-seeding phases (partitioning, formatting, and a blank
// or swap partition's terminal "done") that never go through
// setTorrentStatus.
func (t *progressTracker) setPhase(disk string, number int32, phase string, percent int32) {
	tp, ok := t.partitions[partitionKey{disk: disk, number: number}]
	if !ok {
		return
	}
	tp.phase = phase
	tp.percent = percent
}

// setTorrentStatus updates the partition tracked under hash from a
// GetTorrentStatus poll result: PartitionPhaseDone at 100% once the
// torrent is both finished and paused (the stop policy's own completion
// signal - see shouldPauseTorrent and allStopped), PartitionPhaseSeeding
// with its live download percent otherwise. A hash not tracked by any
// partition (should not happen: every hash polled came from
// torrentHashes) is silently ignored.
func (t *progressTracker) setTorrentStatus(hash string, torrent seeder.Torrent) {
	k, ok := t.hashKey[hash]
	if !ok {
		return
	}
	tp := t.partitions[k]
	if torrent.IsFinished && torrent.IsPaused {
		tp.phase = agentapi.PartitionPhaseDone
		tp.percent = 100
		return
	}
	tp.phase = agentapi.PartitionPhaseSeeding
	tp.percent = torrentPercent(torrent)
}

// setStep sets the whole-plan step reported alongside every following
// snapshot, until the next call changes it. See
// agentapi.ProgressRequest.Step's doc comment for what each DeployStep*
// value means.
func (t *progressTracker) setStep(step string) {
	t.step = step
}

// snapshot returns a ProgressRequest carrying every tracked partition's
// current progress (in plan order) and the whole-plan step most recently
// set by setStep, ready to send as-is.
func (t *progressTracker) snapshot() agentapi.ProgressRequest {
	partitions := make([]agentapi.PartitionProgress, 0, len(t.order))
	for _, k := range t.order {
		tp := t.partitions[k]
		partitions = append(partitions, agentapi.PartitionProgress{
			Disk:        k.disk,
			Number:      k.number,
			Phase:       tp.phase,
			PercentDone: tp.percent,
		})
	}
	return agentapi.ProgressRequest{Partitions: partitions, Step: t.step}
}
