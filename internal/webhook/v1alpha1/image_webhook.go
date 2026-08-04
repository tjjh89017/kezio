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
	"bytes"
	"context"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var imagelog = logf.Log.WithName("image-resource")

// SetupImageWebhookWithManager registers the webhook for Image in the manager.
func SetupImageWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha1.Image{}).
		WithValidator(&ImageCustomValidator{Client: mgr.GetClient()}).
		WithDefaulter(&ImageCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-kezio-kojuro-date-v1alpha1-image,mutating=true,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=images,verbs=create;update,versions=v1alpha1,name=mimage-v1alpha1.kb.io,admissionReviewVersions=v1

// ImageCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Image when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type ImageCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &ImageCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Image.
//
// This is intentionally a no-op: every default value Image needs today
// (spec.bootable, spec.osFamily) is expressed with a +kubebuilder:default
// marker, which the apiserver applies before this webhook ever runs. The
// defaulter still gets registered so a future default that a CEL marker
// cannot express (for example one that depends on another field) has a
// home without needing new webhook scaffolding.
func (d *ImageCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	image, ok := obj.(*keziov1alpha1.Image)
	if !ok {
		return fmt.Errorf("expected an Image object but got %T", obj)
	}
	imagelog.Info("Defaulting for Image", "name", image.GetName())
	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha1-image,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=images,verbs=create;update,versions=v1alpha1,name=vimage-v1alpha1.kb.io,admissionReviewVersions=v1

// ImageCustomValidator struct is responsible for validating the Image resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ImageCustomValidator struct {
	client.Client
}

var _ webhook.CustomValidator = &ImageCustomValidator{}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks,verbs=get;list;watch

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Image.
func (v *ImageCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	image, ok := obj.(*keziov1alpha1.Image)
	if !ok {
		return nil, fmt.Errorf("expected an Image object but got %T", obj)
	}
	imagelog.Info("Validation for Image upon creation", "name", image.GetName())
	return nil, validateImage(ctx, v.Client, image)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Image.
//
// Note: most of spec is create-only, enforced at the CRD level by a
// +kubebuilder:validation:XValidation transition rule ("self == oldSelf")
// on each of spec.source, spec.bootable, spec.osFamily, and
// spec.postHookRefs; that is not duplicated here. spec.params is the
// exception: it is schemaless (x-kubernetes-preserve-unknown-fields), and
// CEL has no structural schema to key equality off for a schemaless
// value, so its immutability is enforced here instead, by a direct byte
// comparison of the raw JSON old and new carry (see paramsChanged).
//
// The rules in validateImage below apply identically to create and
// update (an update that leaves spec untouched still needs to pass them,
// since it may be the first update path a previously-invalid object takes
// - for example a status-only patch on an object created before this
// webhook existed).
func (v *ImageCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	image, ok := newObj.(*keziov1alpha1.Image)
	if !ok {
		return nil, fmt.Errorf("expected an Image object for the newObj but got %T", newObj)
	}
	oldImage, ok := oldObj.(*keziov1alpha1.Image)
	if !ok {
		return nil, fmt.Errorf("expected an Image object for the oldObj but got %T", oldObj)
	}
	imagelog.Info("Validation for Image upon update", "name", image.GetName())

	// Skip validation while the object is being deleted: the finalizer
	// removal that completes deletion is itself an update, and rejecting it
	// would deadlock deletion of an Image that predates this webhook.
	if !image.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}

	if paramsChanged(oldImage.Spec.Params, image.Spec.Params) {
		return nil, fmt.Errorf("spec.params is immutable once the Image is created; create a new Image to publish different content")
	}

	return nil, validateImage(ctx, v.Client, image)
}

// paramsChanged reports whether the schemaless spec.params content
// differs between oldParams and newParams. It compares the raw JSON
// bytes each carries, treating a nil pointer, a pointer to an empty Raw,
// and a pointer to a literal "null" as the same absent value: apiserver
// admission and defaulting round-trips can turn one of these into another
// (see ImageSpec.Params's doc comment) without the content actually
// changing.
func paramsChanged(oldParams, newParams *apiextensionsv1.JSON) bool {
	return string(rawParams(oldParams)) != string(rawParams(newParams))
}

// rawParams returns params's raw JSON bytes, normalizing a nil pointer or
// a literal JSON "null" to an empty byte slice.
func rawParams(params *apiextensionsv1.JSON) []byte {
	if params == nil {
		return nil
	}
	raw := bytes.TrimSpace(params.Raw)
	if string(raw) == "null" {
		return nil
	}
	return raw
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Image.
func (v *ImageCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	image, ok := obj.(*keziov1alpha1.Image)
	if !ok {
		return nil, fmt.Errorf("expected an Image object but got %T", obj)
	}
	imagelog.Info("Validation for Image upon deletion", "name", image.GetName())
	return nil, nil
}

// validateImage runs every admission-time check for an Image spec.
func validateImage(ctx context.Context, cl client.Client, image *keziov1alpha1.Image) error {
	if err := validateChecksum(image.Spec.Source.Checksum); err != nil {
		return fmt.Errorf("spec.source.checksum: %w", err)
	}
	return validateImageOSFamilyAgainstPostHooks(ctx, cl, image)
}

// checksumDigestLen maps a supported checksum algorithm to its expected
// hex digest length.
var checksumDigestLen = map[string]int{
	"md5":    32,
	"sha1":   40,
	"sha256": 64,
	"sha512": 128,
}

// validateChecksum checks that checksum, when set, has the form
// "<algorithm>:<hex digest>" for a supported algorithm, with a digest of
// the length that algorithm produces. An empty checksum is valid: it is an
// optional field.
func validateChecksum(checksum string) error {
	if checksum == "" {
		return nil
	}

	algo, digest, found := strings.Cut(checksum, ":")
	if !found {
		return fmt.Errorf("must have the form \"<algorithm>:<hex digest>\", got %q", checksum)
	}

	wantLen, ok := checksumDigestLen[algo]
	if !ok {
		return fmt.Errorf("unsupported checksum algorithm %q (supported: md5, sha1, sha256, sha512)", algo)
	}

	if len(digest) != wantLen {
		return fmt.Errorf("%s digest must be %d hex characters, got %d in %q", algo, wantLen, len(digest), checksum)
	}
	if !isHexString(digest) {
		return fmt.Errorf("%s digest must be hexadecimal, got %q", algo, digest)
	}

	return nil
}

// isHexString reports whether s contains only hexadecimal digit characters.
func isHexString(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateImageOSFamilyAgainstPostHooks rejects an Image whose osFamily is
// not Linux but which attaches (through spec.postHookRefs) a PostHook
// containing a chrootScript step. chrootScript relies on the agent
// mounting the deployed root file system and chrooting into it to run the
// script; the builtin chroot handling kezio ships only knows how to
// prepare a Linux root (mounting /proc, /sys, /dev, and the ESP under a
// Linux-style fstab path), so a chrootScript step attached to a
// non-Linux-target Image can never run as intended. This check needs a
// cross-object read (Image does not carry the PostHook's steps), which is
// why it lives in the Image validator rather than as a CRD-level CEL rule.
//
// A missing PostHookRefs entry is not an error here: nothing in this
// resource's scope guarantees creation order between an Image and the
// PostHooks it references, and rejecting on a dangling ref is out of
// scope for this check.
func validateImageOSFamilyAgainstPostHooks(ctx context.Context, cl client.Client, image *keziov1alpha1.Image) error {
	if image.Spec.EffectiveOSFamily() == keziov1alpha1.OSFamilyLinux {
		return nil
	}

	for i, ref := range image.Spec.PostHookRefs {
		var postHook keziov1alpha1.PostHook
		ns := keziov1alpha1.ResolveNamespace(ref, image.Namespace)
		err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &postHook)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get spec.postHookRefs[%d] %q: %w", i, ref.Name, err)
		}

		for j, step := range postHook.Spec.Steps {
			if step.ChrootScript != nil {
				return fmt.Errorf(
					"spec.postHookRefs[%d] %q step %d is a chrootScript step, which requires chrooting into a Linux deployed root; spec.osFamily is %q",
					i, ref.Name, j, image.Spec.EffectiveOSFamily(),
				)
			}
		}
	}

	return nil
}
