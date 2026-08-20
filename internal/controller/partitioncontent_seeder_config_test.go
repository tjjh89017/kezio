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
)

func TestPartitionContentSeederConfigReady(t *testing.T) {
	if (PartitionContentSeederConfig{}).ready() {
		t.Error("zero-value config must not be ready")
	}
	if !(PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"}).ready() {
		t.Error("config with an image must be ready")
	}
}

func TestPartitionContentSeederConfigGracePeriod(t *testing.T) {
	if got := (PartitionContentSeederConfig{}).gracePeriod(); got != defaultSeederGracePeriod {
		t.Errorf("gracePeriod() = %s, want default %s", got, defaultSeederGracePeriod)
	}
	cfg := PartitionContentSeederConfig{GracePeriod: 30 * time.Second}
	if got := cfg.gracePeriod(); got != 30*time.Second {
		t.Errorf("gracePeriod() = %s, want %s", got, 30*time.Second)
	}
}

func TestPartitionContentSeederConfigNow(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg := PartitionContentSeederConfig{Now: func() time.Time { return fixed }}
	if got := cfg.now(); !got.Equal(fixed) {
		t.Errorf("now() = %s, want %s", got, fixed)
	}
	if got := (PartitionContentSeederConfig{}).now(); got.IsZero() {
		t.Error("now() with no override must fall back to a non-zero time.Now()")
	}
}
