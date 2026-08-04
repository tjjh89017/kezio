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

package bootserver

import "testing"

func TestNormalizeMAC(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{name: "lowercase", in: "aa:bb:cc:dd:ee:01", want: "aa:bb:cc:dd:ee:01", wantOK: true},
		{name: "uppercase normalizes to lowercase", in: "AA:BB:CC:DD:EE:01", want: "aa:bb:cc:dd:ee:01", wantOK: true},
		{name: "mixed case normalizes to lowercase", in: "Aa:bB:Cc:dD:eE:01", want: "aa:bb:cc:dd:ee:01", wantOK: true},
		{name: "empty", in: "", want: "", wantOK: false},
		{name: "too short", in: "aa:bb:cc:dd:ee", want: "", wantOK: false},
		{name: "too long", in: "aa:bb:cc:dd:ee:01:02", want: "", wantOK: false},
		{name: "no separators", in: "aabbccddee01", want: "", wantOK: false},
		{name: "wrong separator", in: "aa-bb-cc-dd-ee-01", want: "", wantOK: false},
		{name: "non-hex characters", in: "gg:bb:cc:dd:ee:01", want: "", wantOK: false},
		{name: "path traversal attempt", in: "../../etc/passwd", want: "", wantOK: false},
		{name: "embedded newline", in: "aa:bb:cc:dd:ee:01\nrogue", want: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeMAC(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("NormalizeMAC(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("NormalizeMAC(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
