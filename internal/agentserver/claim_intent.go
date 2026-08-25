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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// resolveClaimIntent reads the MachineClaim named by machine.spec.claimRef.
// It returns (nil, nil) when the Machine carries no claimRef, or when the
// named claim no longer exists or its UID no longer matches - all cases
// where this Machine has no intent right now.
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
