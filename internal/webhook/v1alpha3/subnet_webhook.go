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

package v1alpha3

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// nolint:unused
// log is for logging in this package.
var subnetlog = logf.Log.WithName("subnet-resource")

// SetupSubnetWebhookWithManager registers the webhook for Subnet in the manager.
func SetupSubnetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha3.Subnet{}).
		WithValidator(&SubnetCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha3-subnet,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=subnets,verbs=create;update,versions=v1alpha3,name=vsubnet-v1alpha3.kb.io,admissionReviewVersions=v1

// SubnetCustomValidator struct is responsible for validating the Subnet resource
// when it is created or updated.
//
// CIDR/IP patterns and the boot-half all-or-nothing/at-least-one-plane
// grouping are CEL rules on the CRD schema and are not repeated here. The
// one rule that needs a cross-object read is spec.siteRef: admission can
// only check that the named Site exists at write time. A Site deleted
// afterward leaves this Subnet's siteRef dangling, and that is the Subnet
// reconciler's problem (a Valid=False condition), not this webhook's -
// admission has no way to observe an event that happens after it runs.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type SubnetCustomValidator struct {
	// Client reads the Site named by spec.siteRef, from the manager's
	// informer cache. SetupSubnetWebhookWithManager always wires this to
	// mgr.GetClient(), so a nil Client is a programming error, not a
	// supported mode: it makes validateSiteRef call a nil interface,
	// which panics and, under this webhook's failurePolicy=fail, fails
	// closed (denies) rather than admitting.
	Client client.Client
}

var _ webhook.CustomValidator = &SubnetCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Subnet.
func (v *SubnetCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	subnet, ok := obj.(*keziov1alpha3.Subnet)
	if !ok {
		return nil, fmt.Errorf("expected a Subnet object but got %T", obj)
	}
	subnetlog.Info("Validation for Subnet upon creation", "name", subnet.GetName())

	return nil, v.validateSiteRef(ctx, subnet)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Subnet.
func (v *SubnetCustomValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	subnet, ok := newObj.(*keziov1alpha3.Subnet)
	if !ok {
		return nil, fmt.Errorf("expected a Subnet object for the newObj but got %T", newObj)
	}
	subnetlog.Info("Validation for Subnet upon update", "name", subnet.GetName())

	return nil, v.validateSiteRef(ctx, subnet)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Subnet.
//
// Deletion needs no cross-object read: the siteRef rule constrains this
// Subnet's own spec against another object's existence, and there is
// nothing left to check once the Subnet is going away.
func (v *SubnetCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	subnet, ok := obj.(*keziov1alpha3.Subnet)
	if !ok {
		return nil, fmt.Errorf("expected a Subnet object but got %T", obj)
	}
	subnetlog.Info("Validation for Subnet upon deletion", "name", subnet.GetName())

	return nil, nil
}

// validateSiteRef denies a spec.siteRef that names a Site which does not
// exist: a dangling required reference is rejected at admission rather
// than discovered later during placement resolution.
func (v *SubnetCustomValidator) validateSiteRef(ctx context.Context, subnet *keziov1alpha3.Subnet) error {
	ref := subnet.Spec.SiteRef

	namespace := ref.Namespace
	if namespace == "" {
		namespace = subnet.GetNamespace()
	}

	site := &keziov1alpha3.Site{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := v.Client.Get(ctx, key, site); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("spec.siteRef names Site %q, which does not exist", ref.Name)
		}
		return fmt.Errorf("looking up Site %q referenced by spec.siteRef: %w", ref.Name, err)
	}

	return nil
}
