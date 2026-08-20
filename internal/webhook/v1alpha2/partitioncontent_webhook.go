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
	"regexp"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// partitionContentNamePattern matches "pc-" followed by a 40-character hex
// BitTorrent v1 info hash. CRD schema CEL cannot reach metadata.name, so
// this rule lives in the webhook instead of the CRD.
var partitionContentNamePattern = regexp.MustCompile(`^pc-[0-9a-f]{40}$`)

// nolint:unused
// log is for logging in this package.
var partitioncontentlog = logf.Log.WithName("partitioncontent-resource")

// SetupPartitionContentWebhookWithManager registers the webhook for PartitionContent in the manager.
func SetupPartitionContentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.PartitionContent{}).
		WithValidator(&PartitionContentCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-partitioncontent,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=partitioncontents,verbs=create;update,versions=v1alpha2,name=vpartitioncontent-v1alpha2.kb.io,admissionReviewVersions=v1

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
	partitioncontent, ok := obj.(*keziov1alpha2.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object but got %T", obj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon creation", "name", partitioncontent.GetName())

	return nil, validatePartitionContentName(partitioncontent)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PartitionContent.
func (v *PartitionContentCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	partitioncontent, ok := newObj.(*keziov1alpha2.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object for the newObj but got %T", newObj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon update", "name", partitioncontent.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PartitionContent.
func (v *PartitionContentCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	partitioncontent, ok := obj.(*keziov1alpha2.PartitionContent)
	if !ok {
		return nil, fmt.Errorf("expected a PartitionContent object but got %T", obj)
	}
	partitioncontentlog.Info("Validation for PartitionContent upon deletion", "name", partitioncontent.GetName())

	return nil, nil
}

// validatePartitionContentName rejects a name that is not "pc-" followed by
// a 40-character hex BitTorrent v1 info hash. Name is immutable in
// Kubernetes, so this check only needs to run on create.
func validatePartitionContentName(pc *keziov1alpha2.PartitionContent) error {
	if !partitionContentNamePattern.MatchString(pc.GetName()) {
		return fmt.Errorf("metadata.name %q must match %q (a BitTorrent v1 info hash prefixed \"pc-\")", pc.GetName(), partitionContentNamePattern.String())
	}
	return nil
}
