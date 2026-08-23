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

import (
	"testing"
	"time"

	"github.com/tjjh89017/kezio/internal/seeder"
)

func TestImageSeederConfigReady(t *testing.T) {
	if (ImageSeederConfig{}).ready() {
		t.Errorf("ready() = true for the zero value, want false")
	}
	if !(ImageSeederConfig{Image: "example.test/kezio-seeder:test"}).ready() {
		t.Errorf("ready() = false with Image set, want true")
	}
}

func TestImageSeederConfigGracePeriod(t *testing.T) {
	if got := (ImageSeederConfig{}).gracePeriod(); got != defaultSeederGracePeriod {
		t.Errorf("gracePeriod() = %v, want default %v", got, defaultSeederGracePeriod)
	}
	cfg := ImageSeederConfig{GracePeriod: 30 * time.Second}
	if got := cfg.gracePeriod(); got != 30*time.Second {
		t.Errorf("gracePeriod() = %v, want %v", got, 30*time.Second)
	}
}

func TestImageSeederConfigNow(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := ImageSeederConfig{Now: func() time.Time { return fixed }}
	if got := cfg.now(); !got.Equal(fixed) {
		t.Errorf("now() = %v, want %v", got, fixed)
	}
	if got := (ImageSeederConfig{}).now(); got.IsZero() {
		t.Errorf("now() = zero value, want time.Now()")
	}
}

func TestImageSeederConfigMaxUploadsAndMaxConnections(t *testing.T) {
	if got := (ImageSeederConfig{}).maxUploads(); got != seeder.DefaultMaxUploads {
		t.Errorf("maxUploads() = %d, want built-in default %d", got, seeder.DefaultMaxUploads)
	}
	if got := (ImageSeederConfig{}).maxConnections(); got != seeder.DefaultMaxConnections {
		t.Errorf("maxConnections() = %d, want built-in default %d", got, seeder.DefaultMaxConnections)
	}

	cfg := ImageSeederConfig{MaxUploads: 12, MaxConnections: 40}
	if got := cfg.maxUploads(); got != 12 {
		t.Errorf("maxUploads() = %d, want cluster-wide default 12", got)
	}
	if got := cfg.maxConnections(); got != 40 {
		t.Errorf("maxConnections() = %d, want cluster-wide default 40", got)
	}
}
