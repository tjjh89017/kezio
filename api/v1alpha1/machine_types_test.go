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

package v1alpha1

import "testing"

func TestMachineSpecEffectiveOnline(t *testing.T) {
	if got := (MachineSpec{}).EffectiveOnline(); !got {
		t.Errorf("EffectiveOnline() with nil Online = %v, want true", got)
	}

	trueVal, falseVal := true, false
	if got := (MachineSpec{Online: &trueVal}).EffectiveOnline(); !got {
		t.Errorf("EffectiveOnline() with Online=true = %v, want true", got)
	}
	if got := (MachineSpec{Online: &falseVal}).EffectiveOnline(); got {
		t.Errorf("EffectiveOnline() with Online=false = %v, want false", got)
	}
}

func TestMachineSpecEffectiveAfterDeploy(t *testing.T) {
	cases := map[string]struct {
		afterDeploy string
		want        string
	}{
		"absent falls back to Reboot": {afterDeploy: "", want: AfterDeployReboot},
		"Reboot is kept as-is":        {afterDeploy: AfterDeployReboot, want: AfterDeployReboot},
		"PowerOff is kept as-is":      {afterDeploy: AfterDeployPowerOff, want: AfterDeployPowerOff},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			spec := MachineSpec{AfterDeploy: c.afterDeploy}
			if got := spec.EffectiveAfterDeploy(); got != c.want {
				t.Errorf("EffectiveAfterDeploy() = %q, want %q", got, c.want)
			}
		})
	}
}
