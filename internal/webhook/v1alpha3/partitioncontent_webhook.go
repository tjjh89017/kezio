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
	"regexp"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/store"
)

// partitionContentNamePattern matches a lowercase RFC 1123 subdomain, the
// shape Kubernetes itself requires of an object name. It is restated here
// only so a name that would generate an invalid PVC name is caught with
// the same message as one that is too long (see
// validatePartitionContentName). CRD schema CEL cannot reach
// metadata.name, so both rules live in the webhook instead of the CRD.
var partitionContentNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// nolint:unused
// log is for logging in this package.
var partitioncontentlog = logf.Log.WithName("partitioncontent-resource")

// SetupPartitionContentWebhookWithManager registers the webhook for PartitionContent in the manager.
func SetupPartitionContentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha3.PartitionContent{}).
		WithValidator(&PartitionContentCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha3-partitioncontent,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=partitioncontents,verbs=create;update,versions=v1alpha3,name=vpartitioncontent-v1alpha3.kb.io,admissionReviewVersions=v1

// PartitionContentCustomValidator struct is responsible for validating the PartitionContent resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PartitionContentCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &PartitionContentCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PartitionContent.
func (v *PartitionContentCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	partitioncontent, ok := obj.(*keziov1alpha3.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object but got %T", obj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon creation", "name", partitioncontent.GetName())

	return nil, validatePartitionContentName(partitioncontent)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PartitionContent.
func (v *PartitionContentCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	partitioncontent, ok := newObj.(*keziov1alpha3.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object for the newObj but got %T", newObj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon update", "name", partitioncontent.GetName())

	// Nothing to validate: name is immutable in Kubernetes, so
	// validatePartitionContentName never needs to re-run here, and
	// PartitionContentSpec carries its own "self == oldSelf" CEL rule that
	// enforces spec immutability at the schema level.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PartitionContent.
func (v *PartitionContentCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	partitioncontent, ok := obj.(*keziov1alpha3.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object but got %T", obj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon deletion", "name", partitioncontent.GetName())

	return nil, nil
}

// validatePartitionContentName rejects a name this content's own PVC name
// cannot be derived from. A PartitionContent name is chosen by the user,
// not derived from its bytes, so the only constraint left is that
// store.PVCName(name) - the name plus a fixed suffix - still fits the
// Kubernetes object name limit. Name is immutable in Kubernetes, so this
// check only needs to run on create.
func validatePartitionContentName(pc *keziov1alpha3.PartitionContent) error {
	name := pc.GetName()
	if !partitionContentNamePattern.MatchString(name) {
		return fmt.Errorf("metadata.name %q must match %q", name, partitionContentNamePattern.String())
	}
	if len(name) > store.MaxContentNameLength {
		return fmt.Errorf(
			"metadata.name %q is %d characters, over the %d-character limit: this content's own PVC is named %q, which has to fit the Kubernetes object name limit",
			name, len(name), store.MaxContentNameLength, store.PVCName(name))
	}
	return nil
}
