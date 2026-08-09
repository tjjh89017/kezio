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

// Package sitederive is the single choke point that resolves a
// Machine's Site: Machine.spec.subnetRef -> Subnet.spec.siteRef ->
// Site. All callers must share this resolution rather than
// recomputing it.
package sitederive

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// ErrSubnetNotFound classifies a Resolve failure caused by
// Machine.spec.subnetRef naming a Subnet that does not exist. A
// user-facing misconfiguration, never retried away.
var ErrSubnetNotFound = errors.New("subnet not found")

// ErrSiteNotFound classifies a Resolve failure caused by a Subnet's
// spec.siteRef naming a Site that does not exist.
var ErrSiteNotFound = errors.New("site not found")

// Resolution is the result of resolving a Machine to its Site, carrying
// both the Subnet and Site objects a caller already paid to fetch.
type Resolution struct {
	// SiteName is the resolved Site's namespace-qualified identity
	// ("namespace/name"), the same format Identity returns. Site has no
	// cluster-uniqueness guarantee on its name alone, so this string is
	// what every consumer must key on instead.
	SiteName string
	Site     *keziov1alpha1.Site
	Subnet   *keziov1alpha1.Subnet
}

// Identity returns site's namespace-qualified identity string, the same
// format Resolve returns as Resolution.SiteName. Exported so a caller
// that already has a *Site in hand computes the exact same string
// without a second, independently-maintained formatting rule.
func Identity(site *keziov1alpha1.Site) string {
	return site.Namespace + "/" + site.Name
}

// Resolve derives machine's Site by following
// Machine.spec.subnetRef -> Subnet.spec.siteRef -> Site. Each hop's
// NameRef namespace defaulting (keziov1alpha1.ResolveNamespace) applies
// against the namespace of the object holding that ref - the Subnet
// resolves in machine's namespace, and the Site resolves in the
// Subnet's own namespace, not machine's.
//
// A Site with no SeederSubnetRef resolves successfully; that is a
// supported topology, not an error.
//
// A dangling subnetRef or siteRef is classified as ErrSubnetNotFound or
// ErrSiteNotFound respectively so a caller can surface it as a
// misconfiguration rather than retry; any other error is transient.
// Resolve never returns an empty SiteName with a nil error.
//
// c is expected to be a cached informer client: Resolve does one read
// of the Subnet and one of the Site per call and relies on that cache
// rather than adding its own.
func Resolve(ctx context.Context, c client.Client, machine *keziov1alpha1.Machine) (Resolution, error) {
	subnetRef := machine.Spec.SubnetRef
	subnetNS := keziov1alpha1.ResolveNamespace(subnetRef, machine.Namespace)
	subnet := &keziov1alpha1.Subnet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: subnetNS, Name: subnetRef.Name}, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (machine %s/%s spec.subnetRef)",
				ErrSubnetNotFound, subnetNS, subnetRef.Name, machine.Namespace, machine.Name)
		}
		return Resolution{}, fmt.Errorf("get subnet %s/%s: %w", subnetNS, subnetRef.Name, err)
	}

	siteRef := subnet.Spec.SiteRef
	siteNS := keziov1alpha1.ResolveNamespace(siteRef, subnet.Namespace)
	site := &keziov1alpha1.Site{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: siteNS, Name: siteRef.Name}, site); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (subnet %s/%s spec.siteRef)",
				ErrSiteNotFound, siteNS, siteRef.Name, subnet.Namespace, subnet.Name)
		}
		return Resolution{}, fmt.Errorf("get site %s/%s: %w", siteNS, siteRef.Name, err)
	}

	return Resolution{SiteName: Identity(site), Site: site, Subnet: subnet}, nil
}
