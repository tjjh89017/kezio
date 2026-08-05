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

package imageservice

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// StagingSpaceHeadroom is subtracted from the staging filesystem's
// reported available space before comparing it against a declared
// upload length. It keeps admission conservative against the
// filesystem-level overhead (metadata blocks, reserved blocks, and the
// small amount of space Staging.Receive itself uses for a temp file
// during a rename) that a raw available-bytes figure does not capture.
const StagingSpaceHeadroom = 64 << 20 // 64 MiB

// ErrInsufficientSpace reports that an upload's declared length would
// not fit in the staging volume's currently available physical space,
// once every other upload already admitted but not yet finished is
// accounted for.
var ErrInsufficientSpace = errors.New("insufficient staging space")

// ErrQuotaExceeded reports that an upload's declared length would push
// the staging directory's logical usage (already-completed uploads plus
// every other upload currently admitted) over the configured quota.
var ErrQuotaExceeded = errors.New("staging quota exceeded")

// statfsFunc reports the number of bytes available to an unprivileged
// process on the filesystem holding path - what a real statfs(2) call
// reports as "available" (Bavail), which already excludes blocks
// reserved for privileged processes, unlike "free" (Bfree). It exists as
// an injectable seam: Admission's tests simulate arbitrary free-space
// figures without needing a filesystem actually sized that way.
type statfsFunc func(path string) (availableBytes uint64, err error)

// realStatfs is the production statfsFunc, backed by syscall.Statfs.
//
// What this measures depends entirely on what path resolves to. For a
// staging directory backed by a real network-attached PVC, it is that
// volume's own free space. For a local-path/hostPath-backed volume (a
// common single-node deployment shape - see config/image-service's PVC
// comment), path resolves to a directory on the node's root or a
// node-local disk, so this measures the node filesystem itself; on that
// deployment shape the node filesystem genuinely is the true limit an
// upload can hit, so measuring it here is correct, not a workaround.
func realStatfs(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // both operands come from the kernel's own statfs reply
}

// usedBytesFunc reports the total size of every completed upload
// currently on the staging volume. Admission calls this only when a
// logical quota is configured; Staging.UsedBytes is the production
// implementation.
type usedBytesFunc func() (int64, error)

// Admission enforces the two capacity guards a streamed upload must pass
// before this service reads a single body byte:
//
//   - a physical guard, always active: the staging directory
//     filesystem's currently available space (see realStatfs) must be
//     enough to hold the declared upload length, with headroom.
//   - a logical guard, active only when quotaBytes > 0: the sum of
//     already-completed uploads' sizes plus every currently in-flight
//     upload's declared length must not exceed quotaBytes. This is a
//     policy limit (for example, keeping one tenant's uploads from
//     filling a staging volume other tenants share), independent of how
//     much physical space happens to be free.
//
// Both guards are checked against reserved space, not just a
// point-in-time available/used figure: Reserve tracks every admitted,
// not-yet-finished upload's declared length in an in-memory ledger, so
// two concurrent uploads that would each individually fit are still
// caught if their sum would not. The ledger is process-local and
// unpersisted; that is correct because exactly one image-service
// instance owns a given staging directory (see the package doc comment),
// so there is never a second process's reservations to account for.
type Admission struct {
	stagingDir string
	statfs     statfsFunc
	usedBytes  usedBytesFunc
	quotaBytes int64

	mu       sync.Mutex
	reserved int64
}

// NewAdmission builds an Admission guarding stagingDir. usedBytes is
// consulted only when quotaBytes > 0; quotaBytes <= 0 disables the
// logical quota guard entirely (only the physical guard applies).
func NewAdmission(stagingDir string, usedBytes usedBytesFunc, quotaBytes int64) *Admission {
	return &Admission{
		stagingDir: stagingDir,
		statfs:     realStatfs,
		usedBytes:  usedBytes,
		quotaBytes: quotaBytes,
	}
}

// Reserve admits an upload that declares length bytes. On success it
// records length in the in-flight reservation ledger and returns a
// release func the caller must call exactly once - on completion or on
// failure - to give the reservation back; calling release more than once
// is a safe no-op. On failure it reserves nothing and returns an error
// wrapping ErrInsufficientSpace or ErrQuotaExceeded.
func (a *Admission) Reserve(length int64) (release func(), err error) {
	if length < 0 {
		return nil, fmt.Errorf("declared upload length %d is negative", length)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.quotaBytes > 0 {
		used, err := a.usedBytes()
		if err != nil {
			return nil, fmt.Errorf("check staging quota usage: %w", err)
		}
		if used+a.reserved+length > a.quotaBytes {
			return nil, fmt.Errorf("%w: quota %d bytes, %d already used or reserved, upload declares %d more bytes",
				ErrQuotaExceeded, a.quotaBytes, used+a.reserved, length)
		}
	}

	available, err := a.statfs(a.stagingDir)
	if err != nil {
		return nil, fmt.Errorf("check staging volume space: %w", err)
	}
	// available is a point-in-time figure; every other upload currently
	// admitted (a.reserved) has not necessarily written its bytes yet,
	// so it has to be subtracted here rather than trusted to already be
	// reflected in available.
	if uint64(length)+uint64(a.reserved)+StagingSpaceHeadroom > available {
		return nil, fmt.Errorf("%w: upload declares %d bytes, %d bytes already reserved by other uploads, %d bytes available on the staging volume",
			ErrInsufficientSpace, length, a.reserved, available)
	}

	a.reserved += length
	var once sync.Once
	release = func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.reserved -= length
		})
	}
	return release, nil
}
