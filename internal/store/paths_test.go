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

// dns1123Subdomain mirrors the Kubernetes object name rule every generated
// PartitionContent and PVC name has to satisfy.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func TestContentName_IsPrefixPlusPartitionNumber(t *testing.T) {
	if got, want := ContentName("ubuntu-2404", 3), "ubuntu-2404-p3"; got != want {
		t.Errorf("ContentName = %q, want %q", got, want)
	}
}

func TestContentName_IsAValidObjectName(t *testing.T) {
	name := ContentName("ubuntu-2404", 12)
	if !dns1123Subdomain.MatchString(name) {
		t.Errorf("ContentName = %q, does not match %q", name, dns1123Subdomain.String())
	}
}

func TestContentName_DifferentPartitionsDifferentNames(t *testing.T) {
	if ContentName("golden", 1) == ContentName("golden", 2) {
		t.Fatal("ContentName collides for different partition numbers")
	}
}

func TestPVCName_IsContentNamePlusContentSuffix(t *testing.T) {
	name := ContentName("golden", 1)
	if got, want := PVCName(name), name+"-content"; got != want {
		t.Errorf("PVCName = %q, want %q", got, want)
	}
}

func TestPVCName_FitsTheObjectNameLimitAtMaxContentNameLength(t *testing.T) {
	name := make([]byte, MaxContentNameLength)
	for i := range name {
		name[i] = 'a'
	}
	if got := len(PVCName(string(name))); got > k8sObjectNameMaxLength {
		t.Errorf("PVCName length = %d, over the %d-character object name limit", got, k8sObjectNameMaxLength)
	}
}
