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

package agentserver

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// MachineTokenHashIndexField is the field index name handleRegister
// looks Machines up by, mirroring the shape internal/bootserver's
// MachineBootMACIndexField already established: a presented credential
// (there, a MAC; here, a hashed boot token) resolves to a Machine in
// O(1) instead of a linear List scan over every enrolled Machine on
// every registration attempt.
const MachineTokenHashIndexField = ".status.netBoot.tokenHash"

// IndexMachineTokenHash is the client.IndexerFunc for
// MachineTokenHashIndexField: the current boot token hash, or no keys at
// all when the machine has no live token hash (never minted one, or one
// already consumed by a prior registration).
func IndexMachineTokenHash(obj client.Object) []string {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		return nil
	}
	if machine.Status.NetBoot == nil || machine.Status.NetBoot.TokenHash == "" {
		return nil
	}
	return []string{machine.Status.NetBoot.TokenHash}
}

// MachineSessionTokenHashIndexField is the field index name handleNext
// looks Machines up by: the session credential minted at registration
// resolves directly to the Machine it belongs to, the same shape
// MachineTokenHashIndexField gives the boot token, so GET/POST
// /agent/next needs no machine name in its URL or request body.
const MachineSessionTokenHashIndexField = ".status.agentSession.tokenHash"

// IndexMachineSessionTokenHash is the client.IndexerFunc for
// MachineSessionTokenHashIndexField: the current session token hash, or
// no keys at all when the machine has no live session.
func IndexMachineSessionTokenHash(obj client.Object) []string {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		return nil
	}
	if machine.Status.AgentSession == nil || machine.Status.AgentSession.TokenHash == "" {
		return nil
	}
	return []string{machine.Status.AgentSession.TokenHash}
}

// SetupFieldIndexer registers MachineTokenHashIndexField and
// MachineSessionTokenHashIndexField on mgr's cache. Call it once before
// the manager starts, alongside internal/bootserver's own
// SetupFieldIndexer, and before adding a Server that depends on it.
func SetupFieldIndexer(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &keziov1alpha3.Machine{}, MachineTokenHashIndexField, IndexMachineTokenHash); err != nil {
		return err
	}
	return mgr.GetFieldIndexer().IndexField(ctx, &keziov1alpha3.Machine{}, MachineSessionTokenHashIndexField, IndexMachineSessionTokenHash)
}
