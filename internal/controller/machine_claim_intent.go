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

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// resolveClaimIntent reads the MachineClaim named by machine.spec.claimRef,
// the deployment intent's only home since the claim layer replaced
// Machine.spec's own intent fields. It returns (nil, nil) when the
// Machine carries no claimRef, or when the named claim no longer exists -
// both cases mean the same thing to a caller: this Machine has no intent
// right now, so it is idle. Distinguishing "no claim" from "lost binding"
// (a claimRef whose UID no longer matches) is the claim controller's job,
// not this helper's.
func resolveClaimIntent(ctx context.Context, c client.Client, machine *keziov1alpha3.Machine) (*keziov1alpha3.MachineClaim, error) {
	ref := machine.Spec.ClaimRef
	if ref == nil {
		return nil, nil
	}

	claim := &keziov1alpha3.MachineClaim{}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if claim.UID != ref.UID {
		return nil, nil
	}
	return claim, nil
}

// claimUnresolved reports whether machine carries a claimRef that
// resolveClaimIntent could not resolve to a live claim: the transient
// window between a lost binding (a deleted claim, or one recreated with a
// different UID) and the claim controller clearing it. A nil claimRef is
// not this case - that machine has no intent by construction, not an
// unresolved one.
func claimUnresolved(machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) bool {
	return machine.Spec.ClaimRef != nil && claim == nil
}

// claimUnresolvedMessage renders why claimUnresolved is true, for the
// Progressing condition markDelayedNotReady records.
func claimUnresolvedMessage(machine *keziov1alpha3.Machine) string {
	ref := machine.Spec.ClaimRef
	return fmt.Sprintf("claimRef names MachineClaim %s/%s (uid %s), which does not exist or was recreated with a different uid", ref.Namespace, ref.Name, ref.UID)
}
