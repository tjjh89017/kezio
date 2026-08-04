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

package utils

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// CapLeecherCount returns the largest leecher count no greater than
// requested for which requested*contentSizeBytes plus a safetyFraction
// headroom still fits within availableBytes of disk. It is a plain
// arithmetic helper (no cluster or filesystem access) so the seeding
// benchmark's "does N leechers' worth of content actually fit on this
// runner's disk" decision can be unit tested without a real disk or
// cluster.
//
// safetyFraction is the fraction of availableBytes that must stay free
// regardless of N (e.g. 0.2 keeps 20% of the disk unused as headroom for
// the OS, container images, and the store PVC already occupying that
// disk on a single-node k3s cluster) - it must be in [0, 1).
//
// The result is never less than 0 nor greater than requested. A result of
// 0 means not even one leecher's content fits within the headroom-adjusted
// budget; the caller is responsible for deciding whether that is a skip or
// a failure.
func CapLeecherCount(requested int, contentSizeBytes, availableBytes int64, safetyFraction float64) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("requested leecher count %d must not be negative", requested)
	}
	if contentSizeBytes <= 0 {
		return 0, fmt.Errorf("content size %d bytes must be positive", contentSizeBytes)
	}
	if safetyFraction < 0 || safetyFraction >= 1 {
		return 0, fmt.Errorf("safety fraction %v must be in [0, 1)", safetyFraction)
	}
	if requested == 0 {
		return 0, nil
	}
	if availableBytes <= 0 {
		return 0, nil
	}

	budget := float64(availableBytes) * (1 - safetyFraction)
	maxByDisk := int(budget / float64(contentSizeBytes))
	if maxByDisk > requested {
		return requested, nil
	}
	if maxByDisk < 0 {
		return 0, nil
	}
	return maxByDisk, nil
}

// DurationSummary is the min/median/max of a set of measured durations,
// e.g. one per benchmark leecher's completion time.
type DurationSummary struct {
	Min    time.Duration
	Median time.Duration
	Max    time.Duration
}

// SummarizeDurations computes the min, median, and max of durations. It
// does not mutate durations. An empty input is an error: there is no
// meaningful summary of zero samples, and silently returning a zero-value
// summary would look like "every leecher took 0s" rather than "no leecher
// finished".
func SummarizeDurations(durations []time.Duration) (DurationSummary, error) {
	if len(durations) == 0 {
		return DurationSummary{}, errors.New("no durations to summarize")
	}

	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	mid := len(sorted) / 2
	var median time.Duration
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		median = sorted[mid]
	}

	return DurationSummary{
		Min:    sorted[0],
		Median: median,
		Max:    sorted[len(sorted)-1],
	}, nil
}

// ThroughputMBps returns totalBytes served over wall as megabytes (1e6
// bytes, matching how throughput is conventionally reported rather than
// mebibytes) per second. wall must be positive; totalBytes of 0 yields a
// throughput of 0 rather than an error, since "nothing was transferred
// yet" is a valid intermediate sample, not a malformed input.
func ThroughputMBps(totalBytes int64, wall time.Duration) (float64, error) {
	if totalBytes < 0 {
		return 0, fmt.Errorf("total bytes %d must not be negative", totalBytes)
	}
	if wall <= 0 {
		return 0, fmt.Errorf("wall time %s must be positive", wall)
	}
	const bytesPerMB = 1e6
	return float64(totalBytes) / bytesPerMB / wall.Seconds(), nil
}
