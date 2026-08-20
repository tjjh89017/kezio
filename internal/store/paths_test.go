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

package store

import (
	"regexp"
	"testing"
)

// partitionContentNamePattern mirrors the PartitionContent webhook's name
// validation (internal/webhook/v1alpha2/partitioncontent_webhook.go): this
// pins that ObjectName's output always satisfies it.
var partitionContentNamePattern = regexp.MustCompile(`^pc-[0-9a-f]{40}$`)

func testInfoHash(fill byte) InfoHash {
	var h InfoHash
	for i := range h {
		h[i] = fill
	}
	return h
}

func TestObjectName_MatchesPartitionContentNamePattern(t *testing.T) {
	name := ObjectName(testInfoHash(0x42))
	if !partitionContentNamePattern.MatchString(name) {
		t.Errorf("ObjectName = %q, does not match %q", name, partitionContentNamePattern.String())
	}
}

func TestPVCName_IsObjectNamePlusContentSuffix(t *testing.T) {
	hash := testInfoHash(0x42)
	object := ObjectName(hash)
	pvc := PVCName(hash)
	if want := object + "-content"; pvc != want {
		t.Errorf("PVCName = %q, want %q", pvc, want)
	}
}

func TestObjectName_SameHashSameName(t *testing.T) {
	h1, h2 := testInfoHash(0x42), testInfoHash(0x42)
	if ObjectName(h1) != ObjectName(h2) {
		t.Fatal("ObjectName differs for equal info hashes")
	}
}

func TestObjectName_DifferentHashDifferentName(t *testing.T) {
	h1 := testInfoHash(0x00)
	h2 := testInfoHash(0x00)
	h2[0] = 0x01
	if ObjectName(h1) == ObjectName(h2) {
		t.Fatal("ObjectName collides for different info hashes")
	}
}
