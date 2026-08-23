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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// nolint:unused
// log is for logging in this package.
var machinelog = logf.Log.WithName("machine-resource")

// SetupMachineWebhookWithManager registers the webhook for Machine in the manager.
func SetupMachineWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.Machine{}).
		WithValidator(&MachineCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-machine,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=machines,verbs=create;update,versions=v1alpha2,name=vmachine-v1alpha2.kb.io,admissionReviewVersions=v1

// MachineCustomValidator struct is responsible for validating the Machine resource
// when it is created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type MachineCustomValidator struct {
	// Client reads the Subnet named by spec.subnetRef, from the manager's
	// informer cache. SetupMachineWebhookWithManager always wires this to
	// mgr.GetClient(), so a nil Client is a programming error, not a
	// supported mode: it makes validateMachineSubnetRef call a nil
	// interface, which panics and, under this webhook's
	// failurePolicy=fail, fails closed (denies) rather than admitting.
	Client client.Client
}

var _ webhook.CustomValidator = &MachineCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon creation", "name", machine.GetName())

	return nil, v.validateMachine(ctx, machine)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	machine, ok := newObj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object for the newObj but got %T", newObj)
	}
	machinelog.Info("Validation for Machine upon update", "name", machine.GetName())

	// Skip validation while the object is being deleted: finalizer removal
	// completes as an update, and rejecting it would deadlock deletion of
	// a Machine that predates a stricter rule.
	if !machine.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}
	return nil, v.validateMachine(ctx, machine)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Machine.
func (v *MachineCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*keziov1alpha2.Machine)
	if !ok {
		return nil, fmt.Errorf("expected a Machine object but got %T", obj)
	}
	machinelog.Info("Validation for Machine upon deletion", "name", machine.GetName())

	return nil, nil
}

// validateMachine runs every admission-time check for a Machine spec that
// this codebase can decide without reaching into the controller's
// hardware-inventory matching.
func (v *MachineCustomValidator) validateMachine(ctx context.Context, machine *keziov1alpha2.Machine) error {
	if err := validateMachineBMC(machine); err != nil {
		return err
	}
	if err := validateInspectDisable(machine); err != nil {
		return err
	}
	return v.validateSubnetRef(ctx, machine)
}

// validateSubnetRef denies a spec.subnetRef that names a Subnet which does
// not exist, or that names a Subnet with no boot half. A Machine PXE-boots
// from its Subnet; a Subnet with no bootd runs no DHCP/TFTP for it, so a
// Machine attached to a data-plane-only segment can never boot. Rejecting
// this at admission surfaces the mistake immediately, rather than as a
// Machine that silently never registers at deploy time.
func (v *MachineCustomValidator) validateSubnetRef(ctx context.Context, machine *keziov1alpha2.Machine) error {
	ref := machine.Spec.SubnetRef

	namespace := ref.Namespace
	if namespace == "" {
		namespace = machine.GetNamespace()
	}

	subnet := &keziov1alpha2.Subnet{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := v.Client.Get(ctx, key, subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("spec.subnetRef names Subnet %q, which does not exist", ref.Name)
		}
		return fmt.Errorf("looking up Subnet %q referenced by spec.subnetRef: %w", ref.Name, err)
	}

	if !subnet.Spec.HasBootPlane() {
		return fmt.Errorf(
			"spec.subnetRef names Subnet %q, which has no boot half (bootdServerIP/bootdNetworkRef/dhcp): a Machine cannot PXE-boot from a data-plane-only Subnet",
			ref.Name)
	}

	return nil
}

// validateMachineBMC rejects a spec.bmc.address whose scheme has no
// driver registered in internal/bmc's registry: such an address is
// certain to fail at connect time, so it is rejected before it reaches a
// deploy attempt. Once url.Parse succeeds, every error reports address
// through (*url.URL).Redacted(): a Machine author who embeds credentials
// in the address (for example "redfish://user:pass@host/...") must never
// see them echoed back in an admission rejection message.
func validateMachineBMC(machine *keziov1alpha2.Machine) error {
	address := machine.Spec.BMC.Address

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("spec.bmc.address is not a valid URL: %w", err)
	}
	if parsed.Scheme == "" {
		return fmt.Errorf("spec.bmc.address %q has no scheme; a BMC address must be a URL like \"redfish://host/...\" or \"ipmi://host\"", parsed.Redacted())
	}
	if !bmc.IsSchemeRegistered(parsed.Scheme) {
		known := bmc.RegisteredSchemes()
		if len(known) == 0 {
			return fmt.Errorf(
				"spec.bmc.address %q has scheme %q, which has no registered BMC driver (no driver is registered in this binary)",
				parsed.Redacted(), parsed.Scheme,
			)
		}
		return fmt.Errorf(
			"spec.bmc.address %q has scheme %q, which has no registered BMC driver (known: %s)",
			parsed.Redacted(), parsed.Scheme, strings.Join(known, ", "),
		)
	}
	return nil
}

// validateInspectDisable rejects a MachineAnnotationInspectDisable value
// that is not exactly "true", and rejects an empty spec.bootMACAddress
// once the annotation disables inspection: the controller then trusts this
// field instead of discovering it, so it cannot be absent. This is the
// only place bootMACAddress presence is enforced - the CRD schema leaves it
// optional, since it is not required when inspection is enabled.
func validateInspectDisable(machine *keziov1alpha2.Machine) error {
	value, ok := machine.Annotations[keziov1alpha2.MachineAnnotationInspectDisable]
	if !ok {
		return nil
	}
	if value != "true" {
		return fmt.Errorf("annotation %q has value %q, expected exactly \"true\"", keziov1alpha2.MachineAnnotationInspectDisable, value)
	}
	if machine.Spec.BootMACAddress == "" {
		return fmt.Errorf("spec.bootMACAddress is required when annotation %q is \"true\"", keziov1alpha2.MachineAnnotationInspectDisable)
	}
	return nil
}
