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

package ingest

import (
	"errors"
	"fmt"
	"syscall"
)

// scratchSpaceHeadroom is subtracted from the store root's reported
// available space before comparing it against a partition's estimated
// content size, mirroring internal/imageservice.StagingSpaceHeadroom:
// it keeps this pre-flight check conservative against filesystem-level
// overhead that a raw available-bytes figure does not capture.
const scratchSpaceHeadroom = 64 << 20 // 64 MiB

// ErrInsufficientScratchSpace reports that the store root's filesystem
// does not currently have enough available space for the content a
// partition is about to be cloned into, per ensureScratchSpace's
// estimate.
var ErrInsufficientScratchSpace = errors.New("insufficient store scratch space")

// statfsFunc reports the number of bytes available to an unprivileged
// process on the filesystem holding path. It exists as an injectable
// seam, mirroring internal/imageservice's statfsFunc: tests simulate
// arbitrary free-space figures without needing a filesystem actually
// sized that way.
type statfsFunc func(path string) (availableBytes uint64, err error)

// realStatfs is the production statfsFunc, backed by syscall.Statfs.
func realStatfs(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // both operands come from the kernel's own statfs reply
}

// ensureScratchSpace fails fast, before partclone writes a single
// content byte into store scratch (see processPartition), when the store
// root's filesystem does not have enough available space for the
// partition about to be cloned.
//
// partitionBytes - the partition's full raw size, already known to the
// pipeline from the partition table dump (see ParsedPartition.SizeBytes)
// - is used as the estimate because it is a safe upper bound: partclone
// -T only ever emits content for the partition's used blocks, which can
// never exceed the partition's own size, so this can only over-estimate
// the actual write, never under-estimate it and let a doomed clone start
// anyway. It is not exact (a mostly-empty partition needs far less), but
// a clear, early, if conservative, rejection here is the goal - not
// reproducing partclone's own space accounting.
//
// statfs nil means realStatfs; tests supply a fake to simulate an
// arbitrary reported free-space figure.
func ensureScratchSpace(statfs statfsFunc, storeRoot string, partitionBytes int64) error {
	if statfs == nil {
		statfs = realStatfs
	}
	if partitionBytes < 0 {
		return fmt.Errorf("partition size %d is negative", partitionBytes)
	}

	available, err := statfs(storeRoot)
	if err != nil {
		return fmt.Errorf("check store scratch space: %w", err)
	}
	if uint64(partitionBytes)+scratchSpaceHeadroom > available {
		return fmt.Errorf("%w: partition needs up to %d bytes, %d bytes available on the store volume",
			ErrInsufficientScratchSpace, partitionBytes, available)
	}
	return nil
}
