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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/posthookvalidate"
)

// nolint:unused
// log is for logging in this package.
var imagelog = logf.Log.WithName("image-resource")

// SetupImageWebhookWithManager registers the webhook for Image in the manager.
func SetupImageWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.Image{}).
		WithValidator(&ImageCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-image,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=images,verbs=create;update,versions=v1alpha2,name=vimage-v1alpha2.kb.io,admissionReviewVersions=v1

// ImageCustomValidator struct is responsible for validating the Image resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ImageCustomValidator struct {
	// Client reads the PartitionContent objects referenced by slot
	// contentRefs, from the manager's informer cache. PartitionContentSpec
	// is immutable (its own type-level XValidation rule enforces
	// self == oldSelf), so a cached lastExtentEnd can never be stale
	// relative to the object's current spec: whatever value is observed is
	// the value that will ever exist for that name.
	// SetupImageWebhookWithManager always wires this to mgr.GetClient(), so
	// a nil Client is a programming error, not a supported mode: the first
	// slot with a ContentRef makes validateSlotContentSizes call a nil
	// interface, which panics and, under this webhook's failurePolicy=fail,
	// fails closed (denies) rather than admitting. Images with no
	// ContentRef slots are unaffected. Tests that construct
	// ImageCustomValidator directly with a nil Client must therefore avoid
	// contentRef checks.
	Client client.Client
}

var _ webhook.CustomValidator = &ImageCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Image.
func (v *ImageCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	image, ok := obj.(*keziov1alpha2.Image)
	if !ok {
		return nil, fmt.Errorf("expected a Image object but got %T", obj)
	}
	imagelog.Info("Validation for Image upon creation", "name", image.GetName())

	return v.validateImage(ctx, image)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Image.
func (v *ImageCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	image, ok := newObj.(*keziov1alpha2.Image)
	if !ok {
		return nil, fmt.Errorf("expected a Image object for the newObj but got %T", newObj)
	}
	imagelog.Info("Validation for Image upon update", "name", image.GetName())

	// ImageSpec carries its own "spec is immutable" XValidation rule, so
	// this branch only runs in practice for a dry-run client or if that
	// rule is ever relaxed; the slot/content and postHookRefs rules must
	// hold either way.
	return v.validateImage(ctx, image)
}

// validateImage runs every cross-object Image check: the slot/content size
// rule, then the postHookRefs rule, merging both calls' warnings.
func (v *ImageCustomValidator) validateImage(ctx context.Context, image *keziov1alpha2.Image) (admission.Warnings, error) {
	warnings, err := v.validateSlotContentSizes(ctx, image)
	if err != nil {
		return warnings, err
	}
	hookWarnings, err := v.validatePostHookRefs(ctx, image)
	return append(warnings, hookWarnings...), err
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Image.
func (v *ImageCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	image, ok := obj.(*keziov1alpha2.Image)
	if !ok {
		return nil, fmt.Errorf("expected a Image object but got %T", obj)
	}
	imagelog.Info("Validation for Image upon deletion", "name", image.GetName())

	return nil, nil
}

// validateSlotContentSizes denies a slot whose declared sizeBytes is
// smaller than its referenced PartitionContent's lastExtentEnd: extents
// restore at absolute offsets, so a smaller slot would corrupt silently
// instead of failing loudly. It also denies a slot whose contentRef names
// a namespace other than the Image's own - see the contentRef-namespace
// paragraph below.
//
// A slot with contentRef but no declared sizeBytes cannot be checked here:
// SizeBytes is optional, deploy-time-resolvable metadata, not the
// authoritative layout (sfdiskJSON is), so treating an absent SizeBytes as
// 0 would deny every such slot outright. Such a slot is admitted, with a
// warning naming it.
//
// A referenced PartitionContent that does not exist yet is admitted, with
// a warning naming it: creation order between an Image and the
// PartitionContent objects it references is not guaranteed, and the Image
// reconciler plus the cross-reference contract handle a not-ready
// referent at read time.
//
// A slot's contentRef must stay in the Image's own namespace: a
// PartitionContent's deletion finalizer (imagesReferencing,
// activeDeployRunsReferencing) only lists referencing Images within its own
// namespace, so a cross-namespace contentRef would be a live reference the
// finalizer can never see, letting the content and its PVC free out from
// under it. Denying it here at admission is what makes that namespace-scoped
// List correct by construction, rather than requiring it to search every
// namespace for a capability nothing uses.
func (v *ImageCustomValidator) validateSlotContentSizes(ctx context.Context, image *keziov1alpha2.Image) (admission.Warnings, error) {
	var warnings admission.Warnings
	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef == nil {
			continue
		}

		if slot.ContentRef.Namespace != "" && slot.ContentRef.Namespace != image.GetNamespace() {
			return warnings, fmt.Errorf(
				"slot %d references PartitionContent %q in namespace %q, but a slot's contentRef must stay in the Image's own namespace %q",
				slot.Number, slot.ContentRef.Name, slot.ContentRef.Namespace, image.GetNamespace())
		}

		content := &keziov1alpha2.PartitionContent{}
		key := client.ObjectKey{Namespace: image.GetNamespace(), Name: slot.ContentRef.Name}
		if err := v.Client.Get(ctx, key, content); err != nil {
			if apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf(
					"slot %d references PartitionContent %q, which does not exist yet; its slot size will be checked once the content is created",
					slot.Number, slot.ContentRef.Name))
				continue
			}
			return warnings, fmt.Errorf("looking up PartitionContent %q referenced by slot %d: %w", slot.ContentRef.Name, slot.Number, err)
		}

		if slot.SizeBytes == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"slot %d references PartitionContent %q but declares no sizeBytes; its size cannot be checked against the content's lastExtentEnd",
				slot.Number, slot.ContentRef.Name))
			continue
		}

		if slot.SizeBytes < content.Spec.LastExtentEnd {
			return warnings, fmt.Errorf(
				"slot %d sizeBytes (%d) is smaller than PartitionContent %q lastExtentEnd (%d): extents restore at absolute offsets, so a smaller slot would corrupt silently",
				slot.Number, slot.SizeBytes, slot.ContentRef.Name, content.Spec.LastExtentEnd)
		}
	}
	return warnings, nil
}

// validatePostHookRefs denies a spec.postHookRefs entry naming a namespace
// other than the Image's own (the same rule validateSlotContentSizes gives
// a slot's contentRef), and denies one whose referenced PostHook has a
// step incompatible with this Image's OSFamily
// (posthookvalidate.CheckOSFamilyCompatible).
//
// A referenced PostHook that does not exist yet is admitted, with a
// warning naming it: creation order between an Image and the PostHook
// objects it references is not guaranteed. planbuild's own resolve-time
// check (Builder.resolveHook, which runs the same
// CheckOSFamilyCompatible call) catches a referent that still does not
// exist, or turns out incompatible, by the time a deploy actually resolves
// it.
func (v *ImageCustomValidator) validatePostHookRefs(ctx context.Context, image *keziov1alpha2.Image) (admission.Warnings, error) {
	var warnings admission.Warnings
	osFamily := image.Spec.EffectiveOSFamily()
	for i, ref := range image.Spec.PostHookRefs {
		if ref.Namespace != "" && ref.Namespace != image.GetNamespace() {
			return warnings, fmt.Errorf(
				"spec.postHookRefs[%d] references PostHook %q in namespace %q, but a postHookRefs entry must stay in the Image's own namespace %q",
				i, ref.Name, ref.Namespace, image.GetNamespace())
		}

		hook := &keziov1alpha2.PostHook{}
		key := client.ObjectKey{Namespace: image.GetNamespace(), Name: ref.Name}
		if err := v.Client.Get(ctx, key, hook); err != nil {
			if apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf(
					"spec.postHookRefs[%d] references PostHook %q, which does not exist yet; its osFamily compatibility will be checked once it is created",
					i, ref.Name))
				continue
			}
			return warnings, fmt.Errorf("looking up PostHook %q referenced by spec.postHookRefs[%d]: %w", ref.Name, i, err)
		}

		if err := posthookvalidate.CheckOSFamilyCompatible(hook, osFamily); err != nil {
			return warnings, fmt.Errorf("spec.postHookRefs[%d]: %w", i, err)
		}
	}
	return warnings, nil
}
