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

// Package sitederive is the single choke point that resolves seeder
// placement facts, by following Machine.spec.subnetRef -> Subnet ->
// Subnet.spec.siteRef -> Site and returning that Site's designated seeding
// Subnet (Resolve). All callers must share this resolution rather than
// recomputing it.
package sitederive

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// ErrSubnetNotFound classifies a Resolve failure caused by
// Machine.spec.subnetRef naming a Subnet that does not exist. A
// user-facing misconfiguration, never retried away.
var ErrSubnetNotFound = errors.New("subnet not found")

// ErrSiteNotFound classifies a Resolve failure caused by a Subnet's
// spec.siteRef naming a Site that does not exist. A user-facing
// misconfiguration, never retried away.
var ErrSiteNotFound = errors.New("site not found")

// ErrSeederSubnetNotFound classifies a Resolve failure caused by a
// Site's spec.seederSubnetRef naming a Subnet that does not exist. A
// user-facing misconfiguration, never retried away.
var ErrSeederSubnetNotFound = errors.New("seeder subnet not found")

// Resolution is the result of resolving a Machine to the seeder
// placement facts that govern it. Since Resolve follows
// Machine.spec.subnetRef -> Subnet -> Subnet.spec.siteRef -> Site, the
// Identity/SeederNetworkRef/NodeSelector/Subnet fields describe the
// Site's designated seeding Subnet, not the Machine's own Subnet: a
// machine's broadcast domain and its seeder's broadcast domain are
// allowed to differ, as long as one Site holds both.
//
// HasSeeder distinguishes two supported outcomes that would otherwise
// look identical: a Site with no SeederSubnetRef runs no seeder at all
// (HasSeeder false, every other field zero/nil), which differs from a
// seeding Subnet that resolved fine but carries no SeederNetworkRef of
// its own (HasSeeder true, SeederNetworkRef nil - the seeder still
// runs, just on the ordinary cluster network).
type Resolution struct {
	// SiteIdentity is the resolved Site's namespace-qualified identity
	// ("namespace/name"). Empty only when Resolve returned an error.
	SiteIdentity string
	// TrackerURL is the resolved Site's tracker URL
	// (Site.status.trackerURL), echoed as-is. Empty when the Site has no
	// tracker (no SeederSubnetRef) or none has been reconciled yet.
	TrackerURL string
	// HasSeeder reports whether the Site designates a seeding Subnet at
	// all (Site.spec.seederSubnetRef is set). False means this Site runs
	// no seeder, and Identity/SeederNetworkRef/NodeSelector/Subnet are
	// all zero/nil.
	HasSeeder bool
	// Identity is the seeding Subnet's namespace-qualified identity
	// ("namespace/name"), the same format Identity returns. Subnet has
	// no cluster-uniqueness guarantee on its name alone, so this string
	// is what every consumer must key seeder placement on instead.
	Identity string
	// SeederNetworkRef names the NetworkAttachmentDefinition seeder pods
	// attach through, taken from the seeding Subnet's
	// Spec.SeederNetworkRef. Nil means the seeder runs on the ordinary
	// cluster network with no Multus attachment.
	SeederNetworkRef *keziov1alpha3.NameRef
	// NodeSelector constrains seeder pods onto nodes attached to the
	// seeding Subnet's broadcast domain, taken from
	// Subnet.Spec.NodeSelector.
	NodeSelector map[string]string
	Subnet       *keziov1alpha3.Subnet
}

// Identity returns subnet's namespace-qualified identity string, the
// same format Resolve returns as Resolution.Identity. Exported so a
// caller that already has a *Subnet in hand computes the exact same
// string without a second, independently-maintained formatting rule.
func Identity(subnet *keziov1alpha3.Subnet) string {
	return subnet.Namespace + "/" + subnet.Name
}

// SiteIdentity returns site's namespace-qualified identity string, the
// same format Resolve returns as Resolution.SiteIdentity.
func SiteIdentity(site *keziov1alpha3.Site) string {
	return site.Namespace + "/" + site.Name
}

// SiteRefIdentity returns the namespace-qualified identity, in the same
// format SiteIdentity returns, of the Site that subnet.Spec.SiteRef
// names - applying the same bare-NameRef defaulting Resolve does: an
// empty Namespace defaults to subnet's own namespace. Callers that need
// to check a siteRef against a specific Site (for example, that it
// points back at the Site declaring it as a seeding Subnet) must compare
// this against SiteIdentity rather than comparing names alone, since
// Site is namespace-scoped and its name carries no cluster-wide
// uniqueness.
func SiteRefIdentity(subnet *keziov1alpha3.Subnet) string {
	ns := subnet.Spec.SiteRef.Namespace
	if ns == "" {
		ns = subnet.Namespace
	}
	return ns + "/" + subnet.Spec.SiteRef.Name
}

// Resolve derives machine's seeder placement facts by following
// Machine.spec.subnetRef -> Subnet -> Subnet.spec.siteRef -> Site, and
// returns the Site's designated seeding Subnet's facts - not the
// Machine's own Subnet's. A bare NameRef's namespace defaults to the
// namespace of the object holding it (subnetRef against machine,
// siteRef against the Subnet, seederSubnetRef against the Site).
//
// A Site with no SeederSubnetRef resolves successfully with HasSeeder
// false: that Site runs no seeder, which is a supported topology, not
// an error. A seeding Subnet with no SeederNetworkRef also resolves
// successfully (HasSeeder true, SeederNetworkRef nil): its seeder still
// runs, just on the ordinary cluster network.
//
// A dangling subnetRef is classified as ErrSubnetNotFound, a dangling
// siteRef as ErrSiteNotFound, and a Site's dangling seederSubnetRef as
// ErrSeederSubnetNotFound, so a caller can surface each as a
// misconfiguration rather than retry; any other error is transient.
// Resolve never returns a non-zero-error Resolution together with a
// nil error.
//
// c is expected to be a cached informer client: Resolve does at most
// three reads per call (Subnet, Site, seeding Subnet) and relies on
// that cache rather than adding its own. Only Get is used, so a
// read-only client.Reader (for example planbuild.Builder's own Client)
// is sufficient.
func Resolve(ctx context.Context, c client.Reader, machine *keziov1alpha3.Machine) (Resolution, error) {
	subnetRef := machine.Spec.SubnetRef
	subnetNS := subnetRef.Namespace
	if subnetNS == "" {
		subnetNS = machine.Namespace
	}
	subnet := &keziov1alpha3.Subnet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: subnetNS, Name: subnetRef.Name}, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (machine %s/%s spec.subnetRef)",
				ErrSubnetNotFound, subnetNS, subnetRef.Name, machine.Namespace, machine.Name)
		}
		return Resolution{}, fmt.Errorf("get subnet %s/%s: %w", subnetNS, subnetRef.Name, err)
	}

	siteRef := subnet.Spec.SiteRef
	siteNS := siteRef.Namespace
	if siteNS == "" {
		siteNS = subnet.Namespace
	}
	site := &keziov1alpha3.Site{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: siteNS, Name: siteRef.Name}, site); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (subnet %s spec.siteRef)",
				ErrSiteNotFound, siteNS, siteRef.Name, Identity(subnet))
		}
		return Resolution{}, fmt.Errorf("get site %s/%s: %w", siteNS, siteRef.Name, err)
	}

	if site.Spec.SeederSubnetRef == nil {
		return Resolution{
			SiteIdentity: SiteIdentity(site),
			TrackerURL:   site.Status.TrackerURL,
			HasSeeder:    false,
		}, nil
	}

	seederRef := *site.Spec.SeederSubnetRef
	seederNS := seederRef.Namespace
	if seederNS == "" {
		seederNS = site.Namespace
	}
	seederSubnet := &keziov1alpha3.Subnet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: seederNS, Name: seederRef.Name}, seederSubnet); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("%w: %s/%s (site %s spec.seederSubnetRef)",
				ErrSeederSubnetNotFound, seederNS, seederRef.Name, SiteIdentity(site))
		}
		return Resolution{}, fmt.Errorf("get seeder subnet %s/%s: %w", seederNS, seederRef.Name, err)
	}

	res := ResolveSubnet(seederSubnet)
	res.SiteIdentity = SiteIdentity(site)
	res.TrackerURL = site.Status.TrackerURL
	return res, nil
}

// ResolveSubnet derives subnet's own seeder placement facts directly, with
// no Machine or Site indirection: Resolve calls this once it has followed
// the full Machine -> Subnet -> Site chain to a seeding Subnet. HasSeeder
// is always true: a Subnet reached here is, by definition, one that hosts
// a seeder.
func ResolveSubnet(subnet *keziov1alpha3.Subnet) Resolution {
	return Resolution{
		HasSeeder:        true,
		Identity:         Identity(subnet),
		SeederNetworkRef: subnet.Spec.SeederNetworkRef,
		NodeSelector:     subnet.Spec.NodeSelector,
		Subnet:           subnet,
	}
}
