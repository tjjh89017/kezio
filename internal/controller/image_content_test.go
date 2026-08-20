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

func TestBoundedMessage(t *testing.T) {
	cases := []struct {
		name  string
		items []string
		want  string
	}{
		{
			name:  "empty input",
			items: nil,
			want:  "",
		},
		{
			name:  "under limit joins verbatim",
			items: []string{"a", "b"},
			want:  "a; b",
		},
		{
			name:  "exactly at limit joins verbatim",
			items: []string{"a", "b", "c", "d", "e"},
			want:  "a; b; c; d; e",
		},
		{
			name:  "over limit names first N and counts the rest",
			items: []string{"a", "b", "c", "d", "e", "f", "g"},
			want:  "a; b; c; d; e (+2 more)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundedMessage(tc.items)
			if got != tc.want {
				t.Errorf("boundedMessage(%v) = %q, want %q", tc.items, got, tc.want)
			}
		})
	}
}
