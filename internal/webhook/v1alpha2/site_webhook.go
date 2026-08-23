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

package v1alpha2

import (
	"context"
	"fmt"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// nolint:unused
// log is for logging in this package.
var sitelog = logf.Log.WithName("site-resource")

// SetupSiteWebhookWithManager registers the webhook for Site in the manager.
func SetupSiteWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.Site{}).
		WithValidator(&SiteCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-site,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=sites,verbs=create;update,versions=v1alpha2,name=vsite-v1alpha2.kb.io,admissionReviewVersions=v1

// SiteCustomValidator struct is responsible for validating the Site resource
// when it is created or updated.
//
// The tracker.ip/tracker.externalURL mutual-exclusivity rule is a CEL rule
// on SiteTracker and needs no cross-object read, so it is not repeated
// here. Everything below needs one because it depends on another object's
// spec.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type SiteCustomValidator struct {
	// Client reads the Subnet named by spec.seederSubnetRef, from the
	// manager's informer cache. SetupSiteWebhookWithManager always wires
	// this to mgr.GetClient(), so a nil Client is a programming error, not
	// a supported mode: a Site with SeederSubnetRef set makes
	// validateSeederSubnetRef call a nil interface, which panics and,
	// under this webhook's failurePolicy=fail, fails closed (denies)
	// rather than admitting. A Site with no SeederSubnetRef is unaffected.
	// Tests that construct SiteCustomValidator directly with a nil Client
	// must therefore avoid the seederSubnetRef checks.
	Client client.Client
}

var _ webhook.CustomValidator = &SiteCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Site.
func (v *SiteCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	site, ok := obj.(*keziov1alpha2.Site)
	if !ok {
		return nil, fmt.Errorf("expected a Site object but got %T", obj)
	}
	sitelog.Info("Validation for Site upon creation", "name", site.GetName())

	return nil, v.validateSite(ctx, site)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Site.
func (v *SiteCustomValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	site, ok := newObj.(*keziov1alpha2.Site)
	if !ok {
		return nil, fmt.Errorf("expected a Site object for the newObj but got %T", newObj)
	}
	sitelog.Info("Validation for Site upon update", "name", site.GetName())

	return nil, v.validateSite(ctx, site)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Site.
//
// Deletion needs no cross-object read: every rule below constrains a
// Site's own spec against another object's spec, and there is no such
// combination left to check once the Site is going away.
func (v *SiteCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	site, ok := obj.(*keziov1alpha2.Site)
	if !ok {
		return nil, fmt.Errorf("expected a Site object but got %T", obj)
	}
	sitelog.Info("Validation for Site upon deletion", "name", site.GetName())

	return nil, nil
}

// validateSite runs every cross-object Site check: the seederSubnetRef
// back-reference rule, then the tracker-versus-seeding rule, then the
// externalURL shape rule.
func (v *SiteCustomValidator) validateSite(ctx context.Context, site *keziov1alpha2.Site) error {
	if err := v.validateSeederSubnetRef(ctx, site); err != nil {
		return err
	}
	if err := validateTrackerMatchesSeeding(site); err != nil {
		return err
	}
	return validateTrackerExternalURL(site)
}

// validateSeederSubnetRef denies a spec.seederSubnetRef that names a
// Subnet which does not exist, or whose own spec.siteRef does not point
// back at this Site. Either case would produce a seeder no machine at
// this Site is guaranteed to be able to reach, since routability is
// promised only within one Site.
func (v *SiteCustomValidator) validateSeederSubnetRef(ctx context.Context, site *keziov1alpha2.Site) error {
	ref := site.Spec.SeederSubnetRef
	if ref == nil {
		return nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = site.GetNamespace()
	}

	subnet := &keziov1alpha2.Subnet{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := v.Client.Get(ctx, key, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf(
				"spec.seederSubnetRef names Subnet %q, which does not exist",
				ref.Name)
		}
		return fmt.Errorf("looking up Subnet %q referenced by spec.seederSubnetRef: %w", ref.Name, err)
	}

	// Site is namespace-scoped and its name carries no cluster-wide
	// uniqueness, so the back-reference must resolve to this Site's full
	// namespace-qualified identity, not just its name - otherwise a
	// same-named Site in another namespace would satisfy this check.
	gotIdentity := sitederive.SiteRefIdentity(subnet)
	wantIdentity := sitederive.SiteIdentity(site)
	if gotIdentity != wantIdentity {
		return fmt.Errorf(
			"spec.seederSubnetRef names Subnet %q, but that Subnet's spec.siteRef names %q, not this Site (%q): a Site cannot designate a Subnet belonging to another Site as its seeding Subnet, because routability is not guaranteed across that line",
			ref.Name, gotIdentity, wantIdentity)
	}

	return nil
}

// validateTrackerExternalURL denies a tracker.externalURL that cannot
// possibly work as a BitTorrent announce URL: without this check, a
// typo surfaces much later as a leecher timing out with nothing to
// point at, exactly the failure the other tracker rules exist to avoid.
// It must parse as an absolute URL with a scheme BitTorrent announce
// supports (http and https, which is what this repo's own tracker
// Deployment serves and what ezio/libtorrent announce over, plus udp,
// the other scheme libtorrent's tracker client and opentracker - this
// repo's tracker image - both speak) and a host. Reachability is
// deliberately not checked here: admission is not the place for a
// network probe.
func validateTrackerExternalURL(site *keziov1alpha2.Site) error {
	raw := site.Spec.Tracker.ExternalURL
	if raw == "" {
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("spec.tracker.externalURL %q does not parse as a URL: %w", raw, err)
	}

	switch u.Scheme {
	case "http", "https", "udp":
	case "":
		return fmt.Errorf(
			"spec.tracker.externalURL %q has no scheme: a BitTorrent announce URL must be absolute (http, https, or udp)",
			raw)
	default:
		return fmt.Errorf(
			"spec.tracker.externalURL %q has scheme %q, but BitTorrent announce only supports http, https, or udp",
			raw, u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("spec.tracker.externalURL %q has no host", raw)
	}

	return nil
}

// validateTrackerMatchesSeeding enforces the "at least one" half of the
// tracker rule: a seeding Site (SeederSubnetRef set) must carry exactly
// one of Tracker.IP or Tracker.ExternalURL, and a non-seeding Site must
// carry neither. The "not both" half is already a CEL rule on
// SiteTracker and is not repeated here.
func validateTrackerMatchesSeeding(site *keziov1alpha2.Site) error {
	hasTracker := site.Spec.Tracker.IP != "" || site.Spec.Tracker.ExternalURL != ""

	if site.Spec.SeederSubnetRef != nil {
		if !hasTracker {
			return fmt.Errorf(
				"spec.seederSubnetRef names Subnet %q, but neither tracker.ip nor tracker.externalURL is set: a seeding Site with no reachable tracker cannot converge",
				site.Spec.SeederSubnetRef.Name)
		}
		return nil
	}

	if hasTracker {
		return fmt.Errorf(
			"spec.seederSubnetRef is unset, but a tracker is configured: a tracker with no seeder to announce for is a configuration mistake")
	}

	return nil
}
