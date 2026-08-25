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
)

// nolint:unused
// log is for logging in this package.
var deployrunlog = logf.Log.WithName("deployrun-resource")

// SetupDeployRunWebhookWithManager registers the webhook for DeployRun in the manager.
func SetupDeployRunWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha3.DeployRun{}).
		WithValidator(&DeployRunCustomValidator{}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha3-deployrun,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=deployruns,verbs=create;update,versions=v1alpha3,name=vdeployrun-v1alpha3.kb.io,admissionReviewVersions=v1

// DeployRunCustomValidator struct is responsible for validating the DeployRun resource
// when it is created, updated, or deleted.
//
// It is a deliberate no-op: DeployRun's rules (required machineRef, dataImages
// bound, spec immutability) are CEL rules on the CRD schema, not here. This
// type exists only to satisfy the "every kind has a validating webhook" rule.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type DeployRunCustomValidator struct{}

var _ webhook.CustomValidator = &DeployRunCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type DeployRun.
func (v *DeployRunCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	deployrun, ok := obj.(*keziov1alpha3.DeployRun)
	if !ok {
		return nil, fmt.Errorf("expected a DeployRun object but got %T", obj)
	}
	deployrunlog.Info("Validation for DeployRun upon creation", "name", deployrun.GetName())

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type DeployRun.
func (v *DeployRunCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	deployrun, ok := newObj.(*keziov1alpha3.DeployRun)
	if !ok {
		return nil, fmt.Errorf("expected a DeployRun object for the newObj but got %T", newObj)
	}
	deployrunlog.Info("Validation for DeployRun upon update", "name", deployrun.GetName())

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type DeployRun.
func (v *DeployRunCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	deployrun, ok := obj.(*keziov1alpha3.DeployRun)
	if !ok {
		return nil, fmt.Errorf("expected a DeployRun object but got %T", obj)
	}
	deployrunlog.Info("Validation for DeployRun upon deletion", "name", deployrun.GetName())

	return nil, nil
}
