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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// Snapshot is the resolved-plan snapshot a DeployRun records at creation
// time (DeployRunSpec.ResolvedDisks/HooksHash), returned by Build
// alongside the DeployPlan it was computed from so a caller can write it
// into a DeployRun before creating it - DeployRunSpec is immutable once
// the object exists (see its own doc comment), so this must happen before
// the create call, not through a later status write.
type Snapshot struct {
	// ResolvedDisks records which disk each image (the OS image, then
	// each dataImages entry, in order) resolved to.
	ResolvedDisks []keziov1alpha3.DeployRunResolvedDisk
	// HooksHash is a content hash of every resolved PostHook step, so the
	// provisioning trigger can detect a hook change without storing the
	// resolved hook content itself.
	HooksHash string
}

// ApplySnapshot writes snap into run.Spec. Call it only before run is
// created - DeployRunSpec's own XValidation rule rejects any change to
// an already-stored value.
func ApplySnapshot(run *keziov1alpha3.DeployRun, snap Snapshot) {
	run.Spec.ResolvedDisks = snap.ResolvedDisks
	run.Spec.HooksHash = snap.HooksHash
}

// hooksHash returns a content hash of hooks: a deterministic JSON
// encoding (agentapi.ResolvedHook's field order is fixed by its struct
// definition, so json.Marshal is already deterministic here) run through
// SHA-256, hex-encoded.
func hooksHash(hooks []agentapi.ResolvedHook) (string, error) {
	raw, err := json.Marshal(hooks)
	if err != nil {
		return "", fmt.Errorf("marshaling resolved hooks: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
