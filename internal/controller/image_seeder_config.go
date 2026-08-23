/*
Copyright 2026.

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

import "time"

// defaultSeederGracePeriod is how long a per-(Image, Site) seeder
// Deployment is kept running after its Site's seed-demand drops to zero,
// before it is actually deleted. The grace period exists so a leeching
// swarm mid-download is never stranded by demand that clears and
// reappears across a short window - see reconcileImageSeeder and the
// legacy defaultSeederGracePeriod this mirrors
// (internal/controller/seeder_deployment.go on the legacy branch).
const defaultSeederGracePeriod = 5 * time.Minute

// ImageSeederConfig configures the Image reconciler's seeder half: one
// seeder Deployment per (Image, Site) mounting every content PVC the
// Image's slots reference, gated by per-Site seed-demand and the Image's
// own readiness. The zero value (Image == "") is not ready(): demand at a
// Site with no seeder image configured is simply not acted on - no
// Deployment is created there - rather than creating a half-configured
// one.
type ImageSeederConfig struct {
	// Image is the seeder Deployment's container image: it ships both
	// ezio and kezio-seeder-register (see docker/seeder), run as separate
	// containers in the same pod. Read from PARTITIONCONTENT_SEEDER_IMAGE.
	Image string
	// GracePeriod overrides defaultSeederGracePeriod when positive. Read
	// from PARTITIONCONTENT_SEEDER_GRACE_PERIOD (a time.ParseDuration
	// string).
	GracePeriod time.Duration
	// Now returns the current time. Defaults to time.Now; tests override
	// it to drive the grace-period countdown without sleeping real time.
	Now func() time.Time
}

// ready reports whether cfg carries enough configuration to create a
// seeder Deployment - see the type doc comment.
func (cfg ImageSeederConfig) ready() bool {
	return cfg.Image != ""
}

// gracePeriod returns cfg.GracePeriod, falling back to
// defaultSeederGracePeriod when unset.
func (cfg ImageSeederConfig) gracePeriod() time.Duration {
	if cfg.GracePeriod > 0 {
		return cfg.GracePeriod
	}
	return defaultSeederGracePeriod
}

// now returns cfg.Now(), falling back to time.Now.
func (cfg ImageSeederConfig) now() time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}
