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

func TestClaimEzioOverrides_NilWhenEzioUnset(t *testing.T) {
	c := &keziov1alpha3.MachineClaim{}
	if got := claimEzioMaxUploads(c); got != nil {
		t.Errorf("claimEzioMaxUploads = %v, want nil", got)
	}
	if got := claimEzioMaxConnections(c); got != nil {
		t.Errorf("claimEzioMaxConnections = %v, want nil", got)
	}
}

func TestClaimEzioOverrides_PartialOverrideLeavesOtherFieldNil(t *testing.T) {
	// Only maxUploads is set on the claim: maxConnections must still
	// report nil so ResolveMaxConnections falls back to the layer above,
	// rather than the merge accidentally forcing an override.
	c := &keziov1alpha3.MachineClaim{
		Spec: keziov1alpha3.MachineClaimSpec{
			Ezio: &keziov1alpha3.MachineEzioTuning{MaxUploads: int32ptr(42)},
		},
	}
	if got := claimEzioMaxUploads(c); got == nil || *got != 42 {
		t.Errorf("claimEzioMaxUploads = %v, want 42", got)
	}
	if got := claimEzioMaxConnections(c); got != nil {
		t.Errorf("claimEzioMaxConnections = %v, want nil (falls back to the layer above)", got)
	}
}

func TestClaimEzioLaunchOverrides_NilWhenEzioUnset(t *testing.T) {
	c := &keziov1alpha3.MachineClaim{}
	if got := claimEzioCacheSizeMB(c); got != nil {
		t.Errorf("claimEzioCacheSizeMB = %v, want nil", got)
	}
	if got := claimEzioAioThreads(c); got != nil {
		t.Errorf("claimEzioAioThreads = %v, want nil", got)
	}
	if got := claimEzioPort(c); got != nil {
		t.Errorf("claimEzioPort = %v, want nil", got)
	}
}

func TestClaimEzioLaunchOverrides_PassThroughWhenSet(t *testing.T) {
	c := &keziov1alpha3.MachineClaim{
		Spec: keziov1alpha3.MachineClaimSpec{
			Ezio: &keziov1alpha3.MachineEzioTuning{
				CacheSizeMB: int32ptr(2048),
				AioThreads:  int32ptr(8),
				Port:        int32ptr(6890),
			},
		},
	}
	if got := claimEzioCacheSizeMB(c); got == nil || *got != 2048 {
		t.Errorf("claimEzioCacheSizeMB = %v, want 2048", got)
	}
	if got := claimEzioAioThreads(c); got == nil || *got != 8 {
		t.Errorf("claimEzioAioThreads = %v, want 8", got)
	}
	if got := claimEzioPort(c); got == nil || *got != 6890 {
		t.Errorf("claimEzioPort = %v, want 6890", got)
	}
}

// TestLeecherTuningChain_ThreeLayers exercises the full precedence chain
// through seeder.ResolveMaxUploads/ResolveMaxConnections the same way
// Builder.Build wires it, without needing a full envtest Machine/DeployRun
// round trip.
func TestLeecherTuningChain_ThreeLayers(t *testing.T) {
	plain := &keziov1alpha3.MachineClaim{}
	overridden := &keziov1alpha3.MachineClaim{
		Spec: keziov1alpha3.MachineClaimSpec{
			Ezio: &keziov1alpha3.MachineEzioTuning{
				MaxUploads:     int32ptr(5),
				MaxConnections: int32ptr(50),
			},
		},
	}

	cases := []struct {
		name               string
		leecherCfg         LeecherEzioConfig
		claim              *keziov1alpha3.MachineClaim
		wantMaxUploads     int32
		wantMaxConnections int32
	}{
		{
			name:               "built-in default alone",
			leecherCfg:         LeecherEzioConfig{},
			claim:              plain,
			wantMaxUploads:     seeder.DefaultMaxUploads,
			wantMaxConnections: seeder.DefaultMaxConnections,
		},
		{
			name:               "operator cluster default overrides built-in",
			leecherCfg:         LeecherEzioConfig{MaxUploads: 8, MaxConnections: 80},
			claim:              plain,
			wantMaxUploads:     8,
			wantMaxConnections: 80,
		},
		{
			name:               "per-claim override wins over the cluster default",
			leecherCfg:         LeecherEzioConfig{MaxUploads: 8, MaxConnections: 80},
			claim:              overridden,
			wantMaxUploads:     5,
			wantMaxConnections: 50,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotUploads := seeder.ResolveMaxUploads(c.leecherCfg.MaxUploads, claimEzioMaxUploads(c.claim))
			gotConnections := seeder.ResolveMaxConnections(c.leecherCfg.MaxConnections, claimEzioMaxConnections(c.claim))
			if gotUploads != c.wantMaxUploads {
				t.Errorf("MaxUploads = %d, want %d", gotUploads, c.wantMaxUploads)
			}
			if gotConnections != c.wantMaxConnections {
				t.Errorf("MaxConnections = %d, want %d", gotConnections, c.wantMaxConnections)
			}
		})
	}
}
