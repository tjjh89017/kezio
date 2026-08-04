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

package controller

import "testing"

// unjitteredDelay reproduces calculateBackoff's exponential growth without
// the jitter subtraction, so tests can check the jittered result against a
// known, deterministic ceiling.
func unjitteredDelay(errorCount int32) float64 {
	exponent := errorCount - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > backoffMaxExponent {
		exponent = backoffMaxExponent
	}
	delay := float64(backoffBaseDelay) * float64(uint64(1)<<uint(exponent))
	if delay <= 0 || delay > float64(backoffMaxDelay) {
		delay = float64(backoffMaxDelay)
	}
	return delay
}

// TestCalculateBackoffBounds checks that every jittered result falls in
// the deterministic [ceiling*(1-jitterFraction), ceiling] window implied
// by errorCount, for a range of small and very large errorCounts.
func TestCalculateBackoffBounds(t *testing.T) {
	for _, errorCount := range []int32{1, 2, 3, 5, 8, 12, 20, 50, 1000, 1 << 30} {
		ceiling := unjitteredDelay(errorCount)
		floor := ceiling * (1 - backoffJitterFraction)

		// Sample repeatedly: jitter is random, so one call could land
		// anywhere in the window; many samples must all stay inside it.
		for i := 0; i < 20; i++ {
			got := calculateBackoff(errorCount)
			if float64(got) < floor-1 || float64(got) > ceiling {
				t.Fatalf("calculateBackoff(%d) = %v, want in [%v, %v]", errorCount, got, floor, ceiling)
			}
		}
	}
}

// TestCalculateBackoffGrows checks that the backoff ceiling grows with
// errorCount up to the cap, so later retries wait longer than earlier
// ones.
func TestCalculateBackoffGrows(t *testing.T) {
	prev := unjitteredDelay(1)
	for errorCount := int32(2); errorCount <= 10; errorCount++ {
		got := unjitteredDelay(errorCount)
		if got < prev {
			t.Errorf("unjitteredDelay(%d) = %v, want >= previous %v", errorCount, got, prev)
		}
		prev = got
	}
	if prev != float64(backoffMaxDelay) {
		t.Errorf("unjitteredDelay(10) = %v, want the cap %v once errorCount grows enough", prev, backoffMaxDelay)
	}
}

// TestCalculateBackoffNonPositiveErrorCount checks that a zero or negative
// errorCount (which should not happen, since recordPhaseError always
// increments before computing backoff) still returns a sane, positive
// delay instead of panicking or returning zero.
func TestCalculateBackoffNonPositiveErrorCount(t *testing.T) {
	for _, errorCount := range []int32{0, -1, -100} {
		if got := calculateBackoff(errorCount); got <= 0 {
			t.Errorf("calculateBackoff(%d) = %v, want > 0", errorCount, got)
		}
	}
}
