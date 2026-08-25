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

package bootserver

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// MachineBootMACIndexField is the field index name Server looks Machines
// up by. It is registered on the manager's cache once at startup
// (SetupFieldIndexer) so a grub.cfg request resolves its MAC to a
// Machine in O(1) instead of a linear List scan over every Machine on
// every firmware boot attempt.
const MachineBootMACIndexField = ".spec.bootMACAddress"

// IndexMachineBootMAC is the client.IndexerFunc for
// MachineBootMACIndexField: the normalized (lower-cased) form of
// spec.bootMACAddress, or no keys at all when the field is empty or does
// not parse as a MAC address (the webhook's pattern validation should
// already guarantee it does, but the indexer must not panic on a stored
// object that predates that validation).
func IndexMachineBootMAC(obj client.Object) []string {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		return nil
	}
	mac, ok := NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		return nil
	}
	return []string{mac}
}

// SetupFieldIndexer registers MachineBootMACIndexField on mgr's cache.
// Call it once before the manager starts, alongside every controller's
// own SetupWithManager, and before adding a Server that depends on it.
func SetupFieldIndexer(ctx context.Context, mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(ctx, &keziov1alpha3.Machine{}, MachineBootMACIndexField, IndexMachineBootMAC)
}
