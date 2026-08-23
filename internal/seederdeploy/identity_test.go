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

import (
	"strings"
	"testing"
)

// TestName checks that Name is deterministic, distinguishes two Sites of
// the same Image, distinguishes two Images at the same Site, and stays
// within the Deployment name limit even for a long Image name.
func TestName(t *testing.T) {
	sameImageDifferentSite := Name("os-image", "ns/site-a")
	sameImageOtherSite := Name("os-image", "ns/site-b")
	otherImageSameSite := Name("other-image", "ns/site-a")
	repeat := Name("os-image", "ns/site-a")

	if sameImageDifferentSite == sameImageOtherSite {
		t.Fatalf("Name() collided across two Sites of the same Image: %q", sameImageDifferentSite)
	}
	if sameImageDifferentSite == otherImageSameSite {
		t.Fatalf("Name() collided across two Images at the same Site: %q", sameImageDifferentSite)
	}
	if sameImageDifferentSite != repeat {
		t.Fatalf("Name() not deterministic: %q != %q", sameImageDifferentSite, repeat)
	}
	if len(sameImageDifferentSite) > maxNameLength {
		t.Fatalf("Name() exceeded %d chars: %q", maxNameLength, sameImageDifferentSite)
	}
}

// TestNameTruncatesLongImageName checks that an Image name long enough to
// overflow the limit on its own still produces a name within it.
func TestNameTruncatesLongImageName(t *testing.T) {
	longImageName := strings.Repeat("a", 200)
	name := Name(longImageName, "ns/site-a")
	if len(name) > maxNameLength {
		t.Fatalf("Name() exceeded %d chars: %q", maxNameLength, name)
	}
	if !strings.HasPrefix(name, namePrefix) {
		t.Fatalf("Name() lost its prefix: %q", name)
	}
}
