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
	"reflect"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// nolint:unused
// log is for logging in this package.
var machineclaimlog = logf.Log.WithName("machineclaim-resource")

// SetupMachineClaimWebhookWithManager registers the webhook for MachineClaim in the manager.
func SetupMachineClaimWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha3.MachineClaim{}).
		WithValidator(&MachineClaimCustomValidator{}).
		WithDefaulter(&MachineClaimCustomDefaulter{}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/mutate-kezio-kojuro-date-v1alpha3-machineclaim,mutating=true,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machineclaims,verbs=create;update,versions=v1alpha3,name=mmachineclaim-v1alpha3.kb.io,admissionReviewVersions=v1

// MachineClaimCustomDefaulter struct is responsible for setting default
// values on the MachineClaim resource when it is created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type MachineClaimCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &MachineClaimCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the type MachineClaim.
func (d *MachineClaimCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	claim, ok := obj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return fmt.Errorf("expected a MachineClaim object but got %T", obj)
	}
	machineclaimlog.Info("Defaulting for MachineClaim", "name", claim.GetName())

	if claim.Spec.AfterDeploy == "" {
		claim.Spec.AfterDeploy = keziov1alpha3.AfterDeployReboot
	}
	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha3-machineclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machineclaims,verbs=create;update,versions=v1alpha3,name=vmachineclaim-v1alpha3.kb.io,admissionReviewVersions=v1

// MachineClaimCustomValidator struct is responsible for validating the MachineClaim resource
// when it is created or updated.
//
// The machineName/selector mutual-exclusion rule is also a CEL rule on the
// CRD schema; it is repeated here because a webhook rejection carries a
// clearer message than a CEL one. Every other rule below has no schema
// equivalent.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type MachineClaimCustomValidator struct{}

var _ webhook.CustomValidator = &MachineClaimCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type MachineClaim.
func (v *MachineClaimCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	claim, ok := obj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil, fmt.Errorf("expected a MachineClaim object but got %T", obj)
	}
	machineclaimlog.Info("Validation for MachineClaim upon creation", "name", claim.GetName())

	return nil, validateMachineClaimSpec(claim)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type MachineClaim.
func (v *MachineClaimCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldClaim, ok := oldObj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil, fmt.Errorf("expected a MachineClaim object for the oldObj but got %T", oldObj)
	}
	claim, ok := newObj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil, fmt.Errorf("expected a MachineClaim object for the newObj but got %T", newObj)
	}
	machineclaimlog.Info("Validation for MachineClaim upon update", "name", claim.GetName())

	if !claim.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}

	if err := validateMachineClaimSpec(claim); err != nil {
		return nil, err
	}
	return nil, validateMachineClaimBindingImmutable(oldClaim, claim)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type MachineClaim.
func (v *MachineClaimCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	claim, ok := obj.(*keziov1alpha3.MachineClaim)
	if !ok {
		return nil, fmt.Errorf("expected a MachineClaim object but got %T", obj)
	}
	machineclaimlog.Info("Validation for MachineClaim upon deletion", "name", claim.GetName())

	return nil, nil
}

// validateMachineClaimSpec runs every admission-time check for a
// MachineClaim spec that needs no cross-object read.
func validateMachineClaimSpec(claim *keziov1alpha3.MachineClaim) error {
	if claim.Spec.MachineName != "" && claim.Spec.Selector != nil {
		return fmt.Errorf("spec.machineName and spec.selector are mutually exclusive")
	}
	if claim.Spec.ImageRef == nil && len(claim.Spec.DataImages) == 0 {
		return fmt.Errorf("spec must give at least one of imageRef or dataImages: a claim with no intent has nothing to ask for")
	}
	return nil
}

// validateMachineClaimBindingImmutable refuses a change to spec.machineName
// or spec.selector once the claim has bound: moving a bound claim to a
// different machine is not supported, the way PersistentVolumeClaim does
// not support it either. To move, delete the claim and make a new one.
func validateMachineClaimBindingImmutable(oldClaim, claim *keziov1alpha3.MachineClaim) error {
	if oldClaim.Status.Phase != keziov1alpha3.MachineClaimPhaseBound {
		return nil
	}
	if claim.Spec.MachineName != oldClaim.Spec.MachineName {
		return fmt.Errorf("spec.machineName is immutable once the claim is Bound")
	}
	if !machineClaimSelectorEqual(oldClaim.Spec.Selector, claim.Spec.Selector) {
		return fmt.Errorf("spec.selector is immutable once the claim is Bound")
	}
	return nil
}

// machineClaimSelectorEqual reports whether two selectors are deeply
// equal, treating a nil selector and an empty-but-non-nil selector as
// different: a bound claim's selector, once set, keeps the exact shape it
// bound with.
func machineClaimSelectorEqual(a, b *keziov1alpha3.MachineClaimSelector) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}
