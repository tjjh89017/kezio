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

// Package sitederive is the single choke point that resolves a
// Machine's seeder placement facts by following
// Machine.spec.subnetRef -> Subnet. All callers must share this
// resolution rather than recomputing it.
package sitederive

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// ErrSubnetNotFound classifies a Resolve failure caused by
// Machine.spec.subnetRef naming a Subnet that does not exist. A
// user-facing misconfiguration, never retried away.
var ErrSubnetNotFound = errors.New("subnet not found")

// Resolution is the result of resolving a Machine to the seeder
// placement facts that govern it, carrying the Subnet object a caller
// already paid to fetch. SeederNetworkRef and NodeSelector are lifted
// out of Subnet.Spec so a caller does not reach into the Subnet object
// directly: a Site kind does not exist yet, and once one does, a later
// refinement can source Identity/SeederNetworkRef/NodeSelector from the
// Subnet's Site instead without callers changing how they use this
// struct.
type Resolution struct {
	// Identity is the resolved Subnet's namespace-qualified identity
	// ("namespace/name"), the same format Identity returns. Subnet has
	// no cluster-uniqueness guarantee on its name alone, so this string
	// is what every consumer must key seeder placement on instead.
	Identity string
	// SeederNetworkRef names the NetworkAttachmentDefinition seeder pods
	// attach through, taken from Subnet.Spec.SeederNetworkRef. Nil means
	// this Subnet does not host seeders.
	SeederNetworkRef *keziov1alpha2.NameRef
	// NodeSelector constrains seeder pods onto nodes attached to this
	// Subnet's broadcast domain, taken from Subnet.Spec.NodeSelector.
	NodeSelector map[string]string
	Subnet       *keziov1alpha2.Subnet
}

// Identity returns subnet's namespace-qualified identity string, the
// same format Resolve returns as Resolution.Identity. Exported so a
// caller that already has a *Subnet in hand computes the exact same
// string without a second, independently-maintained formatting rule.
func Identity(subnet *keziov1alpha2.Subnet) string {
	return subnet.Namespace + "/" + subnet.Name
}

// Resolve derives machine's seeder placement facts by following
// Machine.spec.subnetRef -> Subnet. subnetRef's NameRef namespace
// defaults to machine's own namespace when empty.
//
// A Subnet with no SeederNetworkRef resolves successfully; that is a
// supported topology (this Subnet does not host seeders), not an
// error.
//
// A dangling subnetRef is classified as ErrSubnetNotFound so a caller
// can surface it as a misconfiguration rather than retry; any other
// error is transient. Resolve never returns an empty Identity with a
// nil error.
//
// c is expected to be a cached informer client: Resolve does one read
// of the Subnet per call and relies on that cache rather than adding
// its own.
func Resolve(ctx context.Context, c client.Client, machine *keziov1alpha2.Machine) (Resolution, error) {
	subnetRef := machine.Spec.SubnetRef
	subnetNS := subnetRef.Namespace
	if subnetNS == "" {
		subnetNS = machine.Namespace
	}
	subnet := &keziov1alpha2.Subnet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: subnetNS, Name: subnetRef.Name}, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (machine %s/%s spec.subnetRef)",
				ErrSubnetNotFound, subnetNS, subnetRef.Name, machine.Namespace, machine.Name)
		}
		return Resolution{}, fmt.Errorf("get subnet %s/%s: %w", subnetNS, subnetRef.Name, err)
	}

	return Resolution{
		Identity:         Identity(subnet),
		SeederNetworkRef: subnet.Spec.SeederNetworkRef,
		NodeSelector:     subnet.Spec.NodeSelector,
		Subnet:           subnet,
	}, nil
}
