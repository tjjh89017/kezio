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
	"testing"
)

func fakeIngestStatfs(available uint64) statfsFunc {
	return func(string) (uint64, error) { return available, nil }
}

// A partition whose estimated content size comfortably fits the reported
// available space (with headroom) passes.
func TestEnsureScratchSpace_Fits(t *testing.T) {
	if err := ensureScratchSpace(fakeIngestStatfs(500<<20), "/store", 100<<20); err != nil {
		t.Fatalf("ensureScratchSpace: unexpected error %v", err)
	}
}

// A partition whose estimated content size does not fit the reported
// available space (accounting for headroom) is rejected with
// ErrInsufficientScratchSpace.
func TestEnsureScratchSpace_TooLarge(t *testing.T) {
	err := ensureScratchSpace(fakeIngestStatfs(200<<20), "/store", 300<<20)
	if !errors.Is(err, ErrInsufficientScratchSpace) {
		t.Fatalf("ensureScratchSpace: err = %v, want ErrInsufficientScratchSpace", err)
	}
}

// The headroom itself is enforced, not just the raw partition size: a
// partition whose size just barely equals the available space (with
// nothing left for headroom) is still rejected.
func TestEnsureScratchSpace_HeadroomEnforced(t *testing.T) {
	err := ensureScratchSpace(fakeIngestStatfs(100<<20), "/store", 100<<20)
	if !errors.Is(err, ErrInsufficientScratchSpace) {
		t.Fatalf("ensureScratchSpace at exactly available with no headroom left: err = %v, want ErrInsufficientScratchSpace", err)
	}
}

// A nil statfsFunc falls back to the real, syscall-backed implementation
// rather than panicking; it is exercised against the current filesystem
// (which always has at least a little free space) just to confirm it
// runs without error.
func TestEnsureScratchSpace_NilStatfsUsesReal(t *testing.T) {
	if err := ensureScratchSpace(nil, ".", 1); err != nil {
		t.Fatalf("ensureScratchSpace(nil statfs): unexpected error %v", err)
	}
}

// A statfs failure (for example, storeRoot no longer mounted) surfaces
// as an error rather than silently skipping the check.
func TestEnsureScratchSpace_StatfsErrorPropagates(t *testing.T) {
	wantErr := errors.New("statfs boom")
	statfs := func(string) (uint64, error) { return 0, wantErr }

	if _, err := statfs("/store"); err == nil {
		t.Fatal("test setup: fake statfs must itself return an error")
	}
	err := ensureScratchSpace(statfs, "/store", 1<<20)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ensureScratchSpace: err = %v, want wrapping %v", err, wantErr)
	}
}

// A negative partition size is rejected outright rather than silently
// treated as zero.
func TestEnsureScratchSpace_NegativeSize(t *testing.T) {
	if err := ensureScratchSpace(fakeIngestStatfs(1<<40), "/store", -1); err == nil {
		t.Fatal("ensureScratchSpace(-1): expected an error")
	}
}
