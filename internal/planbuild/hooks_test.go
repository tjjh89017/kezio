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

package planbuild

import (
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestDeriveBuiltinDefaults(t *testing.T) {
	t.Run("nil osImage yields zero value", func(t *testing.T) {
		got := deriveBuiltinDefaults(nil)
		if got != (builtinDefaults{}) {
			t.Fatalf("deriveBuiltinDefaults(nil) = %+v, want zero value", got)
		}
	})

	t.Run("derives disk, esp partition, and last partition", func(t *testing.T) {
		osImage := &resolvedImage{
			targetDisk: "/dev/sda",
			image: &keziov1alpha2.Image{Spec: keziov1alpha2.ImageSpec{Layout: keziov1alpha2.ImageDiskLayout{
				Slots: []keziov1alpha2.ImageSlot{
					{Number: 1, Role: keziov1alpha2.PartitionRoleESP},
					{Number: 2, Role: keziov1alpha2.PartitionRoleData},
					{Number: 3, Role: keziov1alpha2.PartitionRoleSwap},
				},
			}}},
		}
		got := deriveBuiltinDefaults(osImage)
		want := builtinDefaults{disk: "/dev/sda", espPartition: 1, lastPartition: 3}
		if got != want {
			t.Fatalf("deriveBuiltinDefaults = %+v, want %+v", got, want)
		}
	})

	t.Run("no ESP slot leaves espPartition zero", func(t *testing.T) {
		osImage := &resolvedImage{
			targetDisk: "/dev/sda",
			image: &keziov1alpha2.Image{Spec: keziov1alpha2.ImageSpec{Layout: keziov1alpha2.ImageDiskLayout{
				Slots: []keziov1alpha2.ImageSlot{{Number: 1, Role: keziov1alpha2.PartitionRoleData}},
			}}},
		}
		got := deriveBuiltinDefaults(osImage)
		if got.espPartition != 0 {
			t.Fatalf("espPartition = %d, want 0", got.espPartition)
		}
	})
}

func TestResolveBuiltinParams(t *testing.T) {
	defaultsWithESP := builtinDefaults{disk: "/dev/sda", espPartition: 1, lastPartition: 3}
	data := map[string]any{"machineName": "m1"}

	t.Run("efibootmgr derives disk and part from ESP", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepEfibootmgr, nil, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"disk": "/dev/sda", "part": "1"}
		if len(got) != len(want) || got["disk"] != want["disk"] || got["part"] != want["part"] {
			t.Fatalf("params = %+v, want %+v", got, want)
		}
	})

	t.Run("explicit user param overrides derived default", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepEfibootmgr, map[string]string{"part": "2"}, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["part"] != "2" {
			t.Fatalf("params[part] = %q, want 2 (explicit override)", got["part"])
		}
		if got["disk"] != "/dev/sda" {
			t.Fatalf("params[disk] = %q, want the derived default unaffected", got["disk"])
		}
	})

	t.Run("user param value is templated against data", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepEfibootmgr, map[string]string{"disk": "/dev/{{ .machineName }}"}, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["disk"] != "/dev/m1" {
			t.Fatalf("params[disk] = %q, want /dev/m1", got["disk"])
		}
	})

	t.Run("missing ESP with no override is a ValidationError", func(t *testing.T) {
		_, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepEfibootmgr, nil, builtinDefaults{disk: "/dev/sda"}, data)
		var valErr *ValidationError
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !isValidationError(err, &valErr) {
			t.Fatalf("err = %v (%T), want *ValidationError", err, err)
		}
	})

	t.Run("missing ESP but explicit part override succeeds", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepInstallRemovableFallback, map[string]string{"part": "5"}, builtinDefaults{disk: "/dev/sda"}, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["part"] != "5" {
			t.Fatalf("params[part] = %q, want 5", got["part"])
		}
	})

	t.Run("growLastPartition derives disk and last partition number", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepGrowLastPartition, nil, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["disk"] != "/dev/sda" || got["partition"] != "3" {
			t.Fatalf("params = %+v, want disk=/dev/sda partition=3", got)
		}
	})

	t.Run("mkswap has no params", func(t *testing.T) {
		got, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepMkswap, nil, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("params = %+v, want nil", got)
		}
	})

	t.Run("bad template in user param is a ValidationError", func(t *testing.T) {
		_, err := resolveBuiltinParams(keziov1alpha2.BuiltinStepMkswap, nil, defaultsWithESP, data)
		if err != nil {
			t.Fatalf("unexpected error for mkswap with no params: %v", err)
		}
		_, err = resolveBuiltinParams(keziov1alpha2.BuiltinStepGrowLastPartition, map[string]string{"disk": "{{ .undeclared }}"}, defaultsWithESP, data)
		var valErr *ValidationError
		if !isValidationError(err, &valErr) {
			t.Fatalf("err = %v (%T), want *ValidationError", err, err)
		}
	})
}

// isValidationError reports whether err is a *ValidationError, writing it
// into target on success - a small errors.As wrapper kept local to this
// test file to avoid an extra import in every subtest above.
func isValidationError(err error, target **ValidationError) bool {
	ve, ok := err.(*ValidationError)
	if !ok {
		return false
	}
	*target = ve
	return true
}
