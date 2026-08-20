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
	"sync"
	"testing"
)

// fakeStatfs returns a statfsFunc that always reports available.
func fakeStatfs(available uint64) statfsFunc {
	return func(string) (uint64, error) { return available, nil }
}

func noUsedBytes() (int64, error) { return 0, nil }

// A length that fits is admitted; release gives it back so a larger
// reservation that only fits once released then succeeds.
func TestAdmission_Reserve_PhysicalGuard_FitsAndReleases(t *testing.T) {
	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = fakeStatfs(500 << 20) // 500 MiB available

	release, err := a.Reserve(100 << 20)
	if err != nil {
		t.Fatalf("Reserve(100MiB) with 500MiB available: unexpected error %v", err)
	}

	// 100 + 400 + headroom > 500: rejected while the first is still held.
	if _, err := a.Reserve(400 << 20); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("Reserve(400MiB) while 100MiB already reserved: err = %v, want ErrInsufficientSpace", err)
	}

	release()

	// Once released, the same 400MiB reservation fits.
	if _, err := a.Reserve(400 << 20); err != nil {
		t.Fatalf("Reserve(400MiB) after release: unexpected error %v", err)
	}
}

// A length that doesn't fit (with headroom) is rejected, and rejection
// leaves nothing in the reservation ledger.
func TestAdmission_Reserve_PhysicalGuard_TooLarge(t *testing.T) {
	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = fakeStatfs(200 << 20) // 200 MiB available

	if _, err := a.Reserve(300 << 20); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("Reserve(300MiB) with 200MiB available: err = %v, want ErrInsufficientSpace", err)
	}

	// The rejected attempt above must not have reserved anything.
	if _, err := a.Reserve(50 << 20); err != nil {
		t.Fatalf("Reserve(50MiB) after a prior rejection: unexpected error %v", err)
	}
}

// release is idempotent: calling it twice must not double-credit the
// ledger.
func TestAdmission_Reserve_ReleaseIsIdempotent(t *testing.T) {
	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = fakeStatfs(200 << 20)

	release, err := a.Reserve(100 << 20)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	release()
	release() // must be a no-op, not a second refund

	if _, err := a.Reserve(100 << 20); err != nil {
		t.Fatalf("Reserve after single release: unexpected error %v", err)
	}
	// Would wrongly admit if release double-refunded the ledger.
	if _, err := a.Reserve(100 << 20); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("Reserve after the second reservation still held: err = %v, want ErrInsufficientSpace", err)
	}
}

// Many goroutines race to reserve slices of a fixed-size volume; the
// ledger must serialize them so simultaneous admission never exceeds
// statfs minus headroom.
func TestAdmission_Reserve_ConcurrentAdmissionNeverOverbooks(t *testing.T) {
	const available = 100 << 20 // 100 MiB
	const chunk = 10 << 20      // 10 MiB per goroutine
	const attempts = 20         // more attempts than could possibly fit at once

	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = fakeStatfs(available)

	var (
		mu       sync.Mutex
		admitted int64
		peak     int64
		wg       sync.WaitGroup
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := a.Reserve(chunk)
			if err != nil {
				return // rejection is an expected outcome once the volume fills up
			}
			mu.Lock()
			admitted += chunk
			if admitted > peak {
				peak = admitted
			}
			mu.Unlock()

			release()

			mu.Lock()
			admitted -= chunk
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > available-StagingSpaceHeadroom {
		t.Fatalf("peak simultaneously-admitted bytes = %d, exceeds available minus headroom = %d",
			peak, available-StagingSpaceHeadroom)
	}
}

// The logical quota accounts for both already-used bytes and everything
// currently reserved, not just one or the other.
func TestAdmission_Reserve_QuotaGuard(t *testing.T) {
	const quota = 100 << 20                                     // 100 MiB
	usedBytes := func() (int64, error) { return 60 << 20, nil } // 60 MiB already staged

	a := NewAdmission("/staging", usedBytes, quota)
	a.statfs = fakeStatfs(1 << 40) // physical space is never the limiting factor here

	// 60 (used) + 30 (declared) = 90, fits under the 100MiB quota.
	release, err := a.Reserve(30 << 20)
	if err != nil {
		t.Fatalf("Reserve(30MiB) with 60MiB used, 100MiB quota: unexpected error %v", err)
	}

	// 60 (used) + 30 (reserved) + 20 (declared) = 110, over quota.
	if _, err := a.Reserve(20 << 20); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Reserve(20MiB) over quota: err = %v, want ErrQuotaExceeded", err)
	}

	release()

	// Once released, 60 (used) + 20 (declared) = 80, fits again.
	if _, err := a.Reserve(20 << 20); err != nil {
		t.Fatalf("Reserve(20MiB) after release: unexpected error %v", err)
	}
}

// quotaBytes <= 0 disables the logical quota guard entirely.
func TestAdmission_Reserve_QuotaDisabledWhenZero(t *testing.T) {
	usedBytes := func() (int64, error) {
		t.Fatal("usedBytes must not be called when the quota is disabled")
		return 0, nil
	}
	a := NewAdmission("/staging", usedBytes, 0)
	a.statfs = fakeStatfs(1 << 40)

	if _, err := a.Reserve(500 << 20); err != nil {
		t.Fatalf("Reserve with quota disabled: unexpected error %v", err)
	}
}

// A usedBytes failure (for example, an unreadable uploads directory)
// surfaces as an error rather than silently treating usage as zero.
func TestAdmission_Reserve_UsedBytesErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	usedBytes := func() (int64, error) { return 0, wantErr }
	a := NewAdmission("/staging", usedBytes, 100<<20)
	a.statfs = fakeStatfs(1 << 40)

	if _, err := a.Reserve(1 << 20); !errors.Is(err, wantErr) {
		t.Fatalf("Reserve: err = %v, want wrapping %v", err, wantErr)
	}
}

// A statfs failure surfaces as an error rather than silently admitting
// the upload.
func TestAdmission_Reserve_StatfsErrorPropagates(t *testing.T) {
	wantErr := errors.New("statfs boom")
	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = func(string) (uint64, error) { return 0, wantErr }

	if _, err := a.Reserve(1 << 20); !errors.Is(err, wantErr) {
		t.Fatalf("Reserve: err = %v, want wrapping %v", err, wantErr)
	}
}

// A negative declared length is rejected outright: it can never be a
// legitimate Content-Length, and treating it as valid would make the
// ledger's arithmetic meaningless.
func TestAdmission_Reserve_NegativeLength(t *testing.T) {
	a := NewAdmission("/staging", noUsedBytes, 0)
	a.statfs = fakeStatfs(1 << 40)

	if _, err := a.Reserve(-1); err == nil {
		t.Fatal("Reserve(-1): expected an error")
	}
}
