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

package bmc

import (
	"slices"
	"testing"
)

func TestRegisterAndIsSchemeRegistered(t *testing.T) {
	scheme := "test-registry-scheme"
	if IsSchemeRegistered(scheme) {
		t.Fatalf("scheme %q reported registered before Register was called", scheme)
	}

	Register(scheme)
	if !IsSchemeRegistered(scheme) {
		t.Fatalf("scheme %q reported unregistered after Register was called", scheme)
	}
	if !IsSchemeRegistered("TEST-REGISTRY-SCHEME") {
		t.Fatalf("IsSchemeRegistered is not case-insensitive")
	}

	if got := RegisteredSchemes(); !slices.Contains(got, scheme) {
		t.Fatalf("RegisteredSchemes() = %v, want it to contain %q", got, scheme)
	}
}

func TestRegisterEmptySchemePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register(\"\") did not panic")
		}
	}()
	Register("")
}

func TestRegisterTwicePanics(t *testing.T) {
	scheme := "test-registry-scheme-dup"
	Register(scheme)
	defer func() {
		if recover() == nil {
			t.Fatal("second Register call for the same scheme did not panic")
		}
	}()
	Register(scheme)
}
