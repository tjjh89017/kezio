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

package main

import "testing"

// TestRedactToken_EmptyIsDistinguishedFromShort pins the bug behind the
// observed crash-loop: an empty token must never render the same as a
// present-but-short one. Before this fix both cases returned
// "<redacted>", so the "booted; ..." log line looked identical whether
// or not kezio.token= was actually present on the cmdline.
func TestRedactToken_EmptyIsDistinguishedFromShort(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "empty token", token: "", want: "<empty>"},
		{name: "short token still redacted, not confused with empty", token: "abc123", want: "<redacted>"},
		{name: "long token shows a prefix/suffix hint", token: "abcdefghijklmnop", want: "abcd...mnop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactToken(tt.token); got != tt.want {
				t.Errorf("redactToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}
