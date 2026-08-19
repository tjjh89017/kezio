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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// nolint:unused
// log is for logging in this package.
var machinelog = logf.Log.WithName("machine-resource")

// SetupMachineWebhookWithManager registers the webhook for Machine in the manager.
func SetupMachineWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.Machine{}).
		WithValidator(&MachineCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-machine,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machines,verbs=create;update,versions=v1alpha2,name=vmachine-v1alpha2.kb.io,admissionReviewVersions=v1

// MachineCustomValidator struct is responsible for validating the Machine resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type MachineCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &MachineCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon creation", "name", machine.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	machine, ok := newObj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object for the newObj but got %T", newObj)
	}
	machinelog.Info("Validation for Machine upon update", "name", machine.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon deletion", "name", machine.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
