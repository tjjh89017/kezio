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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/seeder"
)

func int32ptr(v int32) *int32 { return &v }

func TestMachineEzioOverrides_NilWhenEzioUnset(t *testing.T) {
	m := &keziov1alpha3.Machine{}
	if got := machineEzioMaxUploads(m); got != nil {
		t.Errorf("machineEzioMaxUploads = %v, want nil", got)
	}
	if got := machineEzioMaxConnections(m); got != nil {
		t.Errorf("machineEzioMaxConnections = %v, want nil", got)
	}
}

func TestMachineEzioOverrides_PartialOverrideLeavesOtherFieldNil(t *testing.T) {
	// Only maxUploads is set on the Machine: maxConnections must still
	// report nil so ResolveMaxConnections falls back to the layer above,
	// rather than the merge accidentally forcing an override.
	m := &keziov1alpha3.Machine{
		Spec: keziov1alpha3.MachineSpec{
			Ezio: &keziov1alpha3.MachineEzioTuning{MaxUploads: int32ptr(42)},
		},
	}
	if got := machineEzioMaxUploads(m); got == nil || *got != 42 {
		t.Errorf("machineEzioMaxUploads = %v, want 42", got)
	}
	if got := machineEzioMaxConnections(m); got != nil {
		t.Errorf("machineEzioMaxConnections = %v, want nil (falls back to the layer above)", got)
	}
}

// TestLeecherTuningChain_ThreeLayers exercises the full precedence chain
// through seeder.ResolveMaxUploads/ResolveMaxConnections the same way
// Builder.Build wires it, without needing a full envtest Machine/DeployRun
// round trip.
func TestLeecherTuningChain_ThreeLayers(t *testing.T) {
	plain := &keziov1alpha3.Machine{}
	overridden := &keziov1alpha3.Machine{
		Spec: keziov1alpha3.MachineSpec{
			Ezio: &keziov1alpha3.MachineEzioTuning{
				MaxUploads:     int32ptr(5),
				MaxConnections: int32ptr(50),
			},
		},
	}

	cases := []struct {
		name               string
		leecherCfg         LeecherEzioConfig
		machine            *keziov1alpha3.Machine
		wantMaxUploads     int32
		wantMaxConnections int32
	}{
		{
			name:               "built-in default alone",
			leecherCfg:         LeecherEzioConfig{},
			machine:            plain,
			wantMaxUploads:     seeder.DefaultMaxUploads,
			wantMaxConnections: seeder.DefaultMaxConnections,
		},
		{
			name:               "operator cluster default overrides built-in",
			leecherCfg:         LeecherEzioConfig{MaxUploads: 8, MaxConnections: 80},
			machine:            plain,
			wantMaxUploads:     8,
			wantMaxConnections: 80,
		},
		{
			name:               "per-Machine override wins over the cluster default",
			leecherCfg:         LeecherEzioConfig{MaxUploads: 8, MaxConnections: 80},
			machine:            overridden,
			wantMaxUploads:     5,
			wantMaxConnections: 50,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotUploads := seeder.ResolveMaxUploads(c.leecherCfg.MaxUploads, machineEzioMaxUploads(c.machine))
			gotConnections := seeder.ResolveMaxConnections(c.leecherCfg.MaxConnections, machineEzioMaxConnections(c.machine))
			if gotUploads != c.wantMaxUploads {
				t.Errorf("MaxUploads = %d, want %d", gotUploads, c.wantMaxUploads)
			}
			if gotConnections != c.wantMaxConnections {
				t.Errorf("MaxConnections = %d, want %d", gotConnections, c.wantMaxConnections)
			}
		})
	}
}
