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

package seederdeploy

import "testing"

// TestName checks that Name is deterministic and distinguishes two
// sites of the same Image (which would otherwise collide, since both
// derive from the same Image name).
func TestName(t *testing.T) {
	first := Name("os-image", "site-a")
	second := Name("os-image", "site-b")
	repeat := Name("os-image", "site-a")

	if first == second {
		t.Fatalf("Name() collided across sites: %q", first)
	}
	if first != repeat {
		t.Fatalf("Name() not deterministic: %q != %q", first, repeat)
	}
	if len(first) > maxNameLength || len(second) > maxNameLength {
		t.Fatalf("Name() exceeded %d chars: %q, %q", maxNameLength, first, second)
	}
}
