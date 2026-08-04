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

package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var posthooklog = logf.Log.WithName("posthook-resource")

// SetupPostHookWebhookWithManager registers the webhook for PostHook in the manager.
func SetupPostHookWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha1.PostHook{}).
		WithValidator(&PostHookCustomValidator{}).
		WithDefaulter(&PostHookCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-kezio-kojuro-date-v1alpha1-posthook,mutating=true,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=posthooks,verbs=create;update,versions=v1alpha1,name=mposthook-v1alpha1.kb.io,admissionReviewVersions=v1

// PostHookCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind PostHook when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PostHookCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &PostHookCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind PostHook.
//
// This is intentionally a no-op: PostHook has no field with a
// +kubebuilder:default marker today, so there is nothing to default. The
// defaulter is still registered so a future default has a home without
// needing new webhook scaffolding.
func (d *PostHookCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	postHook, ok := obj.(*keziov1alpha1.PostHook)
	if !ok {
		return fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Defaulting for PostHook", "name", postHook.GetName())
	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha1-posthook,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=posthooks,verbs=create;update,versions=v1alpha1,name=vposthook-v1alpha1.kb.io,admissionReviewVersions=v1

// PostHookCustomValidator struct is responsible for validating the PostHook resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PostHookCustomValidator struct{}

var _ webhook.CustomValidator = &PostHookCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
//
// This is intentionally a no-op today. The one rule worth enforcing
// against a chrootScript step - whether the OS it will run against
// actually supports being chrooted into - cannot be decided from a
// PostHook in isolation: PostHook carries no osFamily field, and a given
// PostHook can be attached to Images and Machines of different
// osFamilies through spec.postHookRefs. That check instead lives on the
// attaching side, in the Image validating webhook
// (validateImageOSFamilyAgainstPostHooks in image_webhook.go), which
// reads both the Image's osFamily and the referenced PostHook's steps.
// The validator is still registered here so any future PostHook-local
// rule (for example on Steps or Params in isolation) has a home without
// needing new webhook scaffolding.
func (v *PostHookCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	postHook, ok := obj.(*keziov1alpha1.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon creation", "name", postHook.GetName())
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	postHook, ok := newObj.(*keziov1alpha1.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object for the newObj but got %T", newObj)
	}
	posthooklog.Info("Validation for PostHook upon update", "name", postHook.GetName())
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	postHook, ok := obj.(*keziov1alpha1.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon deletion", "name", postHook.GetName())
	return nil, nil
}
