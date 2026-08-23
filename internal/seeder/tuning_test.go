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

package seeder

import "testing"

func int32ptr(v int32) *int32 { return &v }

func TestResolveMaxUploads(t *testing.T) {
	cases := []struct {
		name           string
		clusterDefault int32
		override       *int32
		want           int32
	}{
		{"built-in default alone", 0, nil, DefaultMaxUploads},
		{"cluster default overrides built-in", 7, nil, 7},
		{"per-machine override wins over cluster default", 7, int32ptr(9), 9},
		{"per-machine override wins over built-in with no cluster default", 0, int32ptr(9), 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveMaxUploads(c.clusterDefault, c.override); got != c.want {
				t.Errorf("ResolveMaxUploads(%d, %v) = %d, want %d", c.clusterDefault, c.override, got, c.want)
			}
		})
	}
}

func TestResolveMaxConnections(t *testing.T) {
	cases := []struct {
		name           string
		clusterDefault int32
		override       *int32
		want           int32
	}{
		{"built-in default alone", 0, nil, DefaultMaxConnections},
		{"cluster default overrides built-in", 20, nil, 20},
		{"per-machine override wins over cluster default", 20, int32ptr(25), 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveMaxConnections(c.clusterDefault, c.override); got != c.want {
				t.Errorf("ResolveMaxConnections(%d, %v) = %d, want %d", c.clusterDefault, c.override, got, c.want)
			}
		})
	}
}

// TestResolveMaxUploadsAndMaxConnectionsAreIndependent proves the two
// resolution chains never bleed into each other: setting one's cluster
// default or override must not move the other's result.
func TestResolveMaxUploadsAndMaxConnectionsAreIndependent(t *testing.T) {
	uploads := ResolveMaxUploads(50, int32ptr(60))
	connections := ResolveMaxConnections(0, nil)
	if uploads != 60 {
		t.Errorf("ResolveMaxUploads = %d, want 60", uploads)
	}
	if connections != DefaultMaxConnections {
		t.Errorf("ResolveMaxConnections = %d, want untouched default %d", connections, DefaultMaxConnections)
	}
}
