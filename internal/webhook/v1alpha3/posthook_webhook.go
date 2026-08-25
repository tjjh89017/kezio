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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/posthookvalidate"
)

// nolint:unused
// log is for logging in this package.
var posthooklog = logf.Log.WithName("posthook-resource")

// SetupPostHookWebhookWithManager registers the webhook for PostHook in the manager.
func SetupPostHookWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha3.PostHook{}).
		WithValidator(&PostHookCustomValidator{}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha3-posthook,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=posthooks,verbs=create;update,versions=v1alpha3,name=vposthook-v1alpha3.kb.io,admissionReviewVersions=v1

// PostHookCustomValidator struct is responsible for validating the PostHook resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PostHookCustomValidator struct{}

var _ webhook.CustomValidator = &PostHookCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	posthook, ok := obj.(*keziov1alpha3.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon creation", "name", posthook.GetName())

	return nil, posthookvalidate.Validate(posthook)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	posthook, ok := newObj.(*keziov1alpha3.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object for the newObj but got %T", newObj)
	}
	posthooklog.Info("Validation for PostHook upon update", "name", posthook.GetName())

	return nil, posthookvalidate.Validate(posthook)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	posthook, ok := obj.(*keziov1alpha3.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon deletion", "name", posthook.GetName())

	return nil, nil
}
