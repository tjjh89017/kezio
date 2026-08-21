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

// nolint:unused
// log is for logging in this package.
var posthooklog = logf.Log.WithName("posthook-resource")

// SetupPostHookWebhookWithManager registers the webhook for PostHook in the manager.
func SetupPostHookWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&keziov1alpha2.PostHook{}).
		WithValidator(&PostHookCustomValidator{}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-kezio-kojuro-date-v1alpha2-posthook,mutating=false,failurePolicy=fail,sideEffects=None,groups=kezio.kojuro.date,resources=posthooks,verbs=create;update,versions=v1alpha2,name=vposthook-v1alpha2.kb.io,admissionReviewVersions=v1

// PostHookCustomValidator struct is responsible for validating the PostHook resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PostHookCustomValidator struct{}

var _ webhook.CustomValidator = &PostHookCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	posthook, ok := obj.(*keziov1alpha2.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon creation", "name", posthook.GetName())

	return nil, validatePostHook(posthook)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	posthook, ok := newObj.(*keziov1alpha2.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object for the newObj but got %T", newObj)
	}
	posthooklog.Info("Validation for PostHook upon update", "name", posthook.GetName())

	return nil, validatePostHook(posthook)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PostHook.
func (v *PostHookCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	posthook, ok := obj.(*keziov1alpha2.PostHook)
	if !ok {
		return nil, fmt.Errorf("expected a PostHook object but got %T", obj)
	}
	posthooklog.Info("Validation for PostHook upon deletion", "name", posthook.GetName())

	return nil, nil
}

// validatePostHook runs every admission-time check for a PostHook spec,
// shared by create and update.
func validatePostHook(ph *keziov1alpha2.PostHook) error {
	if err := validatePostHookParams(ph.Spec.Params); err != nil {
		return err
	}
	declared := declaredPlaceholderNames(ph.Spec.Params)
	for i, step := range ph.Spec.Steps {
		if err := validatePostHookStep(i, step, declared); err != nil {
			return err
		}
	}
	return nil
}

// validatePostHookParams rejects a duplicate param name. The CRD schema
// already enforces this structurally (spec.params is a listType=map keyed
// on name), so this is defense in depth for a caller that builds a PostHook
// programmatically past that layer (for example a dry-run or a future
// in-process caller).
func validatePostHookParams(params []keziov1alpha2.PostHookParam) error {
	seen := make(map[string]bool, len(params))
	for i, p := range params {
		if seen[p.Name] {
			return fmt.Errorf("spec.params[%d]: duplicate param name %q", i, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// declaredPlaceholderNames returns the set of template placeholder names a
// PostHook's steps may reference: every declared param, plus the reserved
// names the deploy plan builder injects on its own.
func declaredPlaceholderNames(params []keziov1alpha2.PostHookParam) map[string]bool {
	names := map[string]bool{
		keziov1alpha2.PostHookReservedParamMachineName: true,
		keziov1alpha2.PostHookReservedParamImageName:   true,
		keziov1alpha2.PostHookReservedParamTargetDisk:  true,
	}
	for _, p := range params {
		names[p.Name] = true
	}
	return names
}

// osFamilyRestrictedBuiltins names the builtins that only make sense
// against a Linux target: mkswap, efibootmgr, and growLastPartition all
// assume Linux disk/boot tooling. install-removable-fallback is not
// restricted; it only copies a bootloader file onto the ESP.
var osFamilyRestrictedBuiltins = map[string]bool{
	keziov1alpha2.BuiltinStepMkswap:            true,
	keziov1alpha2.BuiltinStepEfibootmgr:        true,
	keziov1alpha2.BuiltinStepGrowLastPartition: true,
}

// validatePostHookStep validates one step at spec.steps[i]: exactly one
// kind is set, a script/chrootScript step has exactly one content source,
// an OS-restricted builtin declares osFamily=Linux, and every template
// placeholder in an inline script content references a declared param or a
// reserved name.
//
// The exactly-one-kind and exactly-one-source rules are also enforced by
// the CRD schema's CEL rules; re-checking them here keeps this function
// usable and testable independent of a running apiserver, and gives a
// field-path-qualified error message for the common case.
func validatePostHookStep(i int, step keziov1alpha2.PostHookStep, declared map[string]bool) error {
	path := fmt.Sprintf("spec.steps[%d]", i)

	switch step.Type() {
	case keziov1alpha2.PostHookStepTypeUnknown:
		return fmt.Errorf("%s: exactly one of builtin, script, or chrootScript must be set", path)
	case keziov1alpha2.PostHookStepTypeBuiltin:
		return validateOSFamilyGating(path, step)
	case keziov1alpha2.PostHookStepTypeScript:
		return validateScriptSource(path+".script", *step.Script, declared)
	case keziov1alpha2.PostHookStepTypeChrootScript:
		return validateScriptSource(path+".chrootScript", *step.ChrootScript, declared)
	}
	return nil
}

// validateOSFamilyGating rejects a builtin step whose Name is in
// osFamilyRestrictedBuiltins unless the step's osFamily is Linux.
func validateOSFamilyGating(path string, step keziov1alpha2.PostHookStep) error {
	if !osFamilyRestrictedBuiltins[step.Builtin.Name] {
		return nil
	}
	if step.OSFamily != keziov1alpha2.OSFamilyLinux {
		return fmt.Errorf(
			"%s.builtin: %q requires osFamily to be set to %q, got %q",
			path, step.Builtin.Name, keziov1alpha2.OSFamilyLinux, step.OSFamily,
		)
	}
	return nil
}

// validateScriptSource checks a script/chrootScript step's content source
// and, for an inline script, its template placeholders.
func validateScriptSource(path string, src keziov1alpha2.PostHookScriptSource, declared map[string]bool) error {
	if src.SourceKind() == keziov1alpha2.PostHookScriptSourceUnknown {
		return fmt.Errorf("%s: exactly one of script, configMapRef, or secretRef must be set", path)
	}
	if src.SourceKind() != keziov1alpha2.PostHookScriptSourceInline {
		// configMapRef/secretRef content lives outside this object and can
		// change independently of it, so its placeholders cannot be checked
		// at admission time.
		return nil
	}
	return validatePlaceholders(path+".script", src.Script, declared)
}

// placeholderPattern matches the flat "{{ .name }}" template placeholder
// form the deploy plan builder resolves a PostHook step's content with
// (see internal/agentserver's template rendering): a single field lookup
// directly off the root data map, with no pipeline, function call, or
// nested path. Anything more elaborate the template engine supports is out
// of scope for this check and passes through unexamined.
var placeholderPattern = regexp.MustCompile(`\{\{\s*\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// validatePlaceholders rejects a script whose content references a
// "{{ .name }}" placeholder that is neither a declared param nor a
// reserved name: such a placeholder can never resolve when the deploy plan
// is built, so rejecting it at admission surfaces the mistake immediately
// instead of failing a later deploy.
func validatePlaceholders(path, content string, declared map[string]bool) error {
	for _, m := range placeholderPattern.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if !declared[name] {
			return fmt.Errorf("%s: placeholder %q does not reference a declared param or a reserved name (machineName, imageName, targetDisk)", path, name)
		}
	}
	return nil
}
