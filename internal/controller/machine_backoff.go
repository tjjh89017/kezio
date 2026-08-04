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

import (
	"math/rand"
	"time"
)

const (
	// backoffBaseDelay is the requeue delay after the first consecutive
	// error.
	backoffBaseDelay = 2 * time.Second
	// backoffMaxDelay caps the requeue delay, so a machine stuck in Error
	// still gets retried at a bounded interval.
	backoffMaxDelay = 5 * time.Minute
	// backoffMaxExponent bounds how far errorCount grows the exponent,
	// keeping the shift below backoffBaseDelay's overflow point long
	// before backoffMaxDelay clamps the result anyway.
	backoffMaxExponent = 16
	// backoffJitterFraction is the maximum fraction of the computed delay
	// that jitter subtracts, spreading out retries from machines that
	// failed at the same time.
	backoffJitterFraction = 0.5
)

// calculateBackoff returns how long to wait before retrying a failed
// phase, given the machine's consecutive error count (errorCount >= 1 for
// any call after a recorded failure). The delay grows exponentially with
// errorCount, is capped at backoffMaxDelay, and has jitter subtracted so
// that many machines failing together do not all retry in lockstep.
func calculateBackoff(errorCount int32) time.Duration {
	exponent := errorCount - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > backoffMaxExponent {
		exponent = backoffMaxExponent
	}

	delay := backoffBaseDelay * time.Duration(uint64(1)<<uint(exponent))
	if delay <= 0 || delay > backoffMaxDelay {
		delay = backoffMaxDelay
	}

	jitter := time.Duration(rand.Float64() * backoffJitterFraction * float64(delay)) //nolint:gosec // jitter, not a security-sensitive value
	return delay - jitter
}
