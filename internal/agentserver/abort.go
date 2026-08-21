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

	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// NewAbortDecider builds an AbortDecider backed by c: it asks the agent
// to abort whenever runName is not machineName's live current run -
// unresolvable Machine, no matching Machine, a currentRunRef naming a
// different run, a DeployRun that no longer exists, or one already
// deleting. This is what lets a DeployRun's deletion (GC, an operator, or
// the Machine reconciler starting a fresh run) interrupt an in-progress
// agent session without the delete path itself ever waiting on the
// agent to notice on its own.
//
// machineName alone (no namespace) is what AbortDecider's signature
// carries, so this resolves it by listing every Machine and matching on
// name - the same "assume Machine names are unique cluster-wide"
// simplification internal/agentserver's token-hash lookups already make.
// A List returning more than one match is treated as unresolvable
// (abort), not arbitrarily picked.
func NewAbortDecider(c client.Client) AbortDecider {
	return func(ctx context.Context, machineName, runName, runUID string) bool {
		machine, err := findMachineByName(ctx, c, machineName)
		if err != nil || machine == nil {
			return true
		}
		if machine.Status.CurrentRunRef == nil || machine.Status.CurrentRunRef.Name != runName {
			return true
		}

		ns := machine.Status.CurrentRunRef.Namespace
		if ns == "" {
			ns = machine.Namespace
		}
		var run keziov1alpha2.DeployRun
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: runName}, &run); err != nil {
			return true
		}
		if !run.DeletionTimestamp.IsZero() {
			return true
		}
		return false
	}
}

// findMachineByName returns the single Machine named name, or nil when
// there is none or more than one - see NewAbortDecider's doc comment.
func findMachineByName(ctx context.Context, c client.Client, name string) (*keziov1alpha2.Machine, error) {
	var list keziov1alpha2.MachineList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}

	var found *keziov1alpha2.Machine
	for i := range list.Items {
		if list.Items[i].Name != name {
			continue
		}
		if found != nil {
			return nil, nil
		}
		found = &list.Items[i]
	}
	return found, nil
}
