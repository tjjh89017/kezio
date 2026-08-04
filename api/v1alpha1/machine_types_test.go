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

import (
	"testing"

	"k8s.io/utils/ptr"
)

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

func TestMergeEzioTuning(t *testing.T) {
	t.Run("both nil returns nil", func(t *testing.T) {
		if got := MergeEzioTuning(nil, nil); got != nil {
			t.Errorf("MergeEzioTuning(nil, nil) = %+v, want nil", got)
		}
	})

	t.Run("nil base, override only", func(t *testing.T) {
		override := &MachineEzioTuning{MaxConnections: ptr.To(int32(9))}
		got := MergeEzioTuning(nil, override)
		if got == nil || got.MaxConnections == nil || *got.MaxConnections != 9 {
			t.Errorf("MergeEzioTuning(nil, override) = %+v, want MaxConnections=9", got)
		}
	})

	t.Run("base only, nil override", func(t *testing.T) {
		base := &MachineEzioTuning{MaxUploads: ptr.To(int32(4))}
		got := MergeEzioTuning(base, nil)
		if got == nil || got.MaxUploads == nil || *got.MaxUploads != 4 {
			t.Errorf("MergeEzioTuning(base, nil) = %+v, want MaxUploads=4", got)
		}
	})

	t.Run("per-field override wins, unset fields fall back to base", func(t *testing.T) {
		base := &MachineEzioTuning{
			CacheSizeMB:    ptr.To(int32(512)),
			AioThreads:     ptr.To(int32(16)),
			MaxUploads:     ptr.To(int32(3)),
			MaxConnections: ptr.To(int32(10)),
			Port:           ptr.To(int32(50051)),
		}
		override := &MachineEzioTuning{
			MaxConnections: ptr.To(int32(20)),
		}
		got := MergeEzioTuning(base, override)
		if got.CacheSizeMB == nil || *got.CacheSizeMB != 512 {
			t.Errorf("CacheSizeMB = %v, want base's 512 (override left it unset)", got.CacheSizeMB)
		}
		if got.AioThreads == nil || *got.AioThreads != 16 {
			t.Errorf("AioThreads = %v, want base's 16 (override left it unset)", got.AioThreads)
		}
		if got.MaxUploads == nil || *got.MaxUploads != 3 {
			t.Errorf("MaxUploads = %v, want base's 3 (override left it unset)", got.MaxUploads)
		}
		if got.MaxConnections == nil || *got.MaxConnections != 20 {
			t.Errorf("MaxConnections = %v, want override's 20", got.MaxConnections)
		}
		if got.Port == nil || *got.Port != 50051 {
			t.Errorf("Port = %v, want base's 50051 (override left it unset)", got.Port)
		}
	})
}
