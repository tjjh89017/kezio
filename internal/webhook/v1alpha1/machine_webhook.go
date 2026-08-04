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
	"reflect"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var machinelog = logf.Log.WithName("machine-resource")

// SetupMachineWebhookWithManager registers the webhook for Machine in the manager.
func SetupMachineWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha1.Machine{}).
		WithValidator(&MachineCustomValidator{}).
		WithDefaulter(&MachineCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-kezio-kojuro-date-v1alpha1-machine,mutating=true,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machines,verbs=create;update,versions=v1alpha1,name=mmachine-v1alpha1.kb.io,admissionReviewVersions=v1

// MachineCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Machine when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type MachineCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &MachineCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Machine.
//
// This is intentionally a no-op: every default value Machine needs today
// (spec.online, spec.afterDeploy) is expressed with a +kubebuilder:default
// marker, applied by the apiserver before this webhook runs. The
// defaulter still gets registered so a future default that a marker
// cannot express has a home without needing new webhook scaffolding.
func (d *MachineCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		return fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Defaulting for Machine", "name", machine.GetName())
	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha1-machine,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machines,verbs=create;update,versions=v1alpha1,name=vmachine-v1alpha1.kb.io,admissionReviewVersions=v1

// MachineCustomValidator struct is responsible for validating the Machine resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type MachineCustomValidator struct{}

var _ webhook.CustomValidator = &MachineCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon creation", "name", machine.GetName())
	return nil, validateMachine(machine)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	machine, ok := newObj.(*keziov1alpha1.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object for the newObj but got %T", newObj)
	}
	machinelog.Info("Validation for Machine upon update", "name", machine.GetName())

	// Skip validation while the object is being deleted: the finalizer
	// removal that completes deletion is itself an update, and rejecting it
	// would deadlock deletion of a Machine that predates this webhook.
	if !machine.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}
	return nil, validateMachine(machine)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon deletion", "name", machine.GetName())
	return nil, nil
}

// diskTarget is one Machine spec entry that resolves to a disk: either the
// OS image (spec.imageRef + spec.targetDisk) or one spec.dataImages entry.
type diskTarget struct {
	label string
	hints *keziov1alpha1.TargetDiskHints
}

// validateMachine runs every admission-time check for a Machine spec that
// this codebase can decide without reaching into the controller's
// hardware-inventory matching (which only runs once a real disk inventory
// exists). These checks catch configurations that are guaranteed invalid
// regardless of what hardware the Machine turns out to have.
func validateMachine(machine *keziov1alpha1.Machine) error {
	if err := validateDistinctTargetDisks(machine); err != nil {
		return err
	}
	return validateNoDuplicateDataImageRefs(machine)
}

// validateDistinctTargetDisks rejects a Machine whose OS image and
// dataImages entries do not all target distinct disks. Two targetDisk
// hint sets are only compared when both are "specific" (at least one
// field set): an absent or all-empty hint does not pin a disk, so it
// cannot be shown to collide with anything at admission time - that is
// left to the controller's runtime hardware match, which rejects an
// ambiguous or non-unique match when it actually resolves disks. But two
// specific hint sets that are byte-identical are guaranteed to resolve to
// the same disk (the controller requires exactly one matching disk per
// hint set), so that case is rejectable early.
func validateDistinctTargetDisks(machine *keziov1alpha1.Machine) error {
	var targets []diskTarget
	if machine.Spec.ImageRef != nil {
		targets = append(targets, diskTarget{label: "spec.targetDisk (OS image)", hints: machine.Spec.TargetDisk})
	}
	for i := range machine.Spec.DataImages {
		targets = append(targets, diskTarget{
			label: fmt.Sprintf("spec.dataImages[%d].targetDisk", i),
			hints: machine.Spec.DataImages[i].TargetDisk,
		})
	}

	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			a, b := targets[i], targets[j]
			if !isSpecificHint(a.hints) || !isSpecificHint(b.hints) {
				continue
			}
			if reflect.DeepEqual(*a.hints, *b.hints) {
				return fmt.Errorf(
					"%s and %s specify byte-identical targetDisk hints, which are guaranteed to resolve to the same disk; each deployed image must target a distinct disk",
					a.label, b.label,
				)
			}
		}
	}
	return nil
}

// isSpecificHint reports whether hints pins to a disk: non-nil with at
// least one field set. A nil or all-zero-value hint matches "any disk"
// and is not specific enough to compare for a guaranteed collision.
func isSpecificHint(hints *keziov1alpha1.TargetDiskHints) bool {
	if hints == nil {
		return false
	}
	return !reflect.DeepEqual(*hints, keziov1alpha1.TargetDiskHints{})
}

// validateNoDuplicateDataImageRefs rejects two spec.dataImages entries
// that reference the same Image (by resolved name and namespace) with
// identical targetDisk hints (including both being unset): that
// configuration deploys the same image to the same place twice, which is
// certainly a mistake regardless of whether the hints are specific enough
// to pin a disk.
func validateNoDuplicateDataImageRefs(machine *keziov1alpha1.Machine) error {
	dataImages := machine.Spec.DataImages
	for i := 0; i < len(dataImages); i++ {
		for j := i + 1; j < len(dataImages); j++ {
			a, b := dataImages[i], dataImages[j]
			sameImage := a.ImageRef.Name == b.ImageRef.Name &&
				keziov1alpha1.ResolveNamespace(a.ImageRef, machine.Namespace) == keziov1alpha1.ResolveNamespace(b.ImageRef, machine.Namespace)
			if sameImage && reflect.DeepEqual(a.TargetDisk, b.TargetDisk) {
				return fmt.Errorf(
					"spec.dataImages[%d] and spec.dataImages[%d] both reference Image %q with identical targetDisk hints; this deploys the same image to the same disk twice",
					i, j, a.ImageRef.Name,
				)
			}
		}
	}
	return nil
}
