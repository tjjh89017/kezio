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
	"testing"

	"github.com/tjjh89017/kezio/internal/store"
)

// maxNameLength is the Kubernetes Deployment name limit this test
// guards Name against, independent of Name's own implementation.
const maxNameLength = 63

// TestName checks that Name is deterministic, distinguishes two
// different contents, and stays within the Deployment name limit.
func TestName(t *testing.T) {
	first := hashOf(t, 0x1000)
	second := hashOf(t, 0x2000)

	firstName := Name(first)
	secondName := Name(second)
	repeat := Name(hashOf(t, 0x1000))

	if firstName == secondName {
		t.Fatalf("Name() collided across different contents: %q", firstName)
	}
	if firstName != repeat {
		t.Fatalf("Name() not deterministic: %q != %q", firstName, repeat)
	}
	if len(firstName) > maxNameLength || len(secondName) > maxNameLength {
		t.Fatalf("Name() exceeded %d chars: %q, %q", maxNameLength, firstName, secondName)
	}
}

// hashOf builds a distinct store.InfoHash for offset: offset positions
// the single extent, which changes the bencoded info dict and therefore
// the computed hash.
func hashOf(t *testing.T, offset uint64) store.InfoHash {
	t.Helper()
	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 1,
		Extents:     []store.Extent{{Offset: offset, Length: 4096}},
		PieceHashes: []store.PieceHash{{}},
	}
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}
	return hash
}
