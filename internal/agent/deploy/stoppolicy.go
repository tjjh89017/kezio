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

import "github.com/tjjh89017/kezio/internal/seeder"

// minFinishedSeconds and minIdleUploadSeconds are the two halves of the
// "finished and idle" pause condition, both fixed at 15 seconds -
// matching ezio's own reference client (tmp/ezio/utils/ezio_cli.py's
// MIN_FINISHED_TIME/MIN_LAST_UPLOAD defaults) exactly, since the target
// machine seeding to other leechers during a fleet deploy is the same
// situation Clonezilla's ezio_cli.py already handles this way.
const (
	minFinishedSeconds   = 15
	minIdleUploadSeconds = 15
	// uploadRatioLimit is the "upload > N x size" pause threshold.
	uploadRatioLimit = 3
)

// shouldPauseTorrent reports whether t should be paused under the
// deploy-side stop policy, mirroring ezio_cli.py's check_stop exactly:
// a torrent that is not finished, or already paused, is left alone; a
// finished torrent is paused once its cumulative upload exceeds
// uploadRatioLimit times its size, or once it has been finished for more
// than minFinishedSeconds and has gone at least minIdleUploadSeconds
// without uploading anything (LastUpload == -1, ezio's "never uploaded"
// sentinel, counts as idle too).
func shouldPauseTorrent(t seeder.Torrent) bool {
	if t.IsPaused || !t.IsFinished {
		return false
	}
	if t.TotalPayloadUpload > uploadRatioLimit*t.TotalDone {
		return true
	}
	if t.FinishedTime > minFinishedSeconds && (t.LastUpload > minIdleUploadSeconds || t.LastUpload == -1) {
		return true
	}
	return false
}

// allStopped reports whether every torrent in torrents is both finished
// and paused - the signal Executor uses to call Shutdown on the local
// ezio daemon once a deployment's content is fully seeded and no longer
// needs to keep pushing bytes to other leechers.
func allStopped(torrents map[string]seeder.Torrent) bool {
	if len(torrents) == 0 {
		return false
	}
	for _, t := range torrents {
		if !t.IsFinished || !t.IsPaused {
			return false
		}
	}
	return true
}

// torrentPercent returns t's download progress as 0-100, clamped: ezio
// reports total_done/total as raw byte counters, and Total is 0 until
// metadata resolves - never the case here, since AddTorrent always
// supplies a complete .torrent up front, but defended anyway against a
// division by zero.
func torrentPercent(t seeder.Torrent) int32 {
	if t.Total <= 0 {
		if t.IsFinished {
			return 100
		}
		return 0
	}
	percent := t.TotalDone * 100 / t.Total
	switch {
	case percent < 0:
		return 0
	case percent > 100:
		return 100
	default:
		return int32(percent) //nolint:gosec // clamped to [0,100] above
	}
}
