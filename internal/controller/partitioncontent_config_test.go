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

import "testing"

func TestPartitionContentPublishConfigReady(t *testing.T) {
	cases := []struct {
		name  string
		cfg   PartitionContentPublishConfig
		ready bool
	}{
		{
			name:  "both unset is not ready",
			cfg:   PartitionContentPublishConfig{},
			ready: false,
		},
		{
			name:  "image set alone is not ready - tracker is also mandatory",
			cfg:   PartitionContentPublishConfig{Image: "example.test/kezio-ingest:test"},
			ready: false,
		},
		{
			name:  "tracker set alone is not ready - image is also mandatory",
			cfg:   PartitionContentPublishConfig{TrackerURL: "http://tracker.example.test/announce"},
			ready: false,
		},
		{
			name: "both set is ready",
			cfg: PartitionContentPublishConfig{
				Image:      "example.test/kezio-ingest:test",
				TrackerURL: "http://tracker.example.test/announce",
			},
			ready: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ready(); got != tc.ready {
				t.Errorf("ready() = %v, want %v", got, tc.ready)
			}
		})
	}
}
