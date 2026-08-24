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

package posthookdefaults

import (
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/posthookvalidate"
)

func TestSpecPassesValidation(t *testing.T) {
	ph := &keziov1alpha2.PostHook{Spec: Spec()}
	if err := posthookvalidate.Validate(ph); err != nil {
		t.Fatalf("Spec() failed posthookvalidate.Validate: %v", err)
	}
}

// TestSpecCompletesTheFallbackPathItPointsNVRAMAt: efibootmgr writes an
// NVRAM entry naming the removable-media fallback path and nothing else,
// so the shipped hook stakes a machine's whole bootability on that path.
// install-removable-fallback is what makes the path bootable, and must
// run before the entry that names it.
func TestSpecCompletesTheFallbackPathItPointsNVRAMAt(t *testing.T) {
	spec := Spec()
	if len(spec.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(spec.Steps))
	}

	wantNames := []string{
		keziov1alpha2.BuiltinStepMkswap,
		keziov1alpha2.BuiltinStepInstallRemovableFallback,
		keziov1alpha2.BuiltinStepEfibootmgr,
	}
	for i, want := range wantNames {
		step := spec.Steps[i]
		if step.Type() != keziov1alpha2.PostHookStepTypeBuiltin {
			t.Fatalf("step[%d]: type = %q, want builtin", i, step.Type())
		}
		if step.Builtin.Name != want {
			t.Errorf("step[%d].builtin.name = %q, want %q", i, step.Builtin.Name, want)
		}
		if step.OSFamily != keziov1alpha2.OSFamilyLinux {
			t.Errorf("step[%d].osFamily = %q, want %q", i, step.OSFamily, keziov1alpha2.OSFamilyLinux)
		}
	}
}
