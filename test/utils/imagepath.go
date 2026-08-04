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
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// SmallestSeedablePartition returns the partition in partitions with the
// smallest UsedBytes among those that carry an InfoHash (a partition ingest
// never produced content for - for example one restored purely by UUID -
// has no torrent to leech and so is never a candidate). It is a plain
// helper (no cluster access) so the "pick the smallest, e.g. the ESP"
// selection the e2e image-path stage's leecher check relies on can be unit
// tested without a cluster.
func SmallestSeedablePartition(
	partitions []keziov1alpha1.ImagePartitionStatus,
) (keziov1alpha1.ImagePartitionStatus, error) {
	return extremeSeedablePartition(partitions, func(candidate, best int64) bool { return candidate < best })
}

// LargestSeedablePartition returns the partition in partitions with the
// largest UsedBytes among those that carry an InfoHash, the same
// candidate rule SmallestSeedablePartition uses (see its doc comment).
// The seeding benchmark uses this to pick the rootfs - the partition with
// the most meaningful data volume to swarm N leechers against - without
// hardcoding a role name that varies by image layout.
func LargestSeedablePartition(
	partitions []keziov1alpha1.ImagePartitionStatus,
) (keziov1alpha1.ImagePartitionStatus, error) {
	return extremeSeedablePartition(partitions, func(candidate, best int64) bool { return candidate > best })
}

// extremeSeedablePartition returns the partition among those with a
// non-empty InfoHash for which isBetter(candidate.UsedBytes,
// best.UsedBytes) holds against every other candidate - the shared
// selection loop behind SmallestSeedablePartition and
// LargestSeedablePartition.
func extremeSeedablePartition(
	partitions []keziov1alpha1.ImagePartitionStatus,
	isBetter func(candidate, best int64) bool,
) (keziov1alpha1.ImagePartitionStatus, error) {
	var best keziov1alpha1.ImagePartitionStatus
	found := false
	for _, p := range partitions {
		if p.InfoHash == "" {
			continue
		}
		if !found || isBetter(p.UsedBytes, best.UsedBytes) {
			best = p
			found = true
		}
	}
	if !found {
		return keziov1alpha1.ImagePartitionStatus{}, errors.New("no partition with a non-empty infoHash")
	}
	return best, nil
}

// ParseSHA256Sum extracts the hex digest from one line of `sha256sum`
// output ("<64 lowercase hex chars>  <path>"). It is a plain string helper
// so the e2e image-path stage's leecher byte-compare (which runs
// `sha256sum` inside a Pod via `kubectl exec` and needs to parse its
// stdout) can be unit tested without a cluster.
func ParseSHA256Sum(output string) (string, error) {
	const digestLen = 64
	line := strings.TrimSpace(output)
	if len(line) < digestLen {
		return "", fmt.Errorf("sha256sum output %q is shorter than a %d-char digest", output, digestLen)
	}
	digest := line[:digestLen]
	for _, r := range digest {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return "", fmt.Errorf("sha256sum output %q does not start with a lowercase hex digest", output)
		}
	}
	rest := line[digestLen:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", fmt.Errorf("sha256sum output %q has no separator after the digest", output)
	}
	return digest, nil
}
