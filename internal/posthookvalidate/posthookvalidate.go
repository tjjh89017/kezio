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

// Package posthookvalidate checks a PostHook spec's own invariants beyond
// what the CRD schema's CEL rules enforce: unique param names, an
// OS-restricted builtin declaring osFamily=Linux, and an inline script's
// template placeholders resolving to a declared param or a reserved name.
// Both the PostHook admission webhook and PostHookReconciler call Validate
// so the two apply exactly the same rules; the reconciler layers its own
// reconcile-time-only checks (a configMapRef/secretRef actually resolving)
// on top.
package posthookvalidate

import (
	"fmt"
	"regexp"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// Validate runs every spec-only PostHook check: unique param names, then
// each step's shape and content in order.
func Validate(ph *keziov1alpha2.PostHook) error {
	if err := ValidateParams(ph.Spec.Params); err != nil {
		return err
	}
	declared := DeclaredPlaceholderNames(ph.Spec.Params)
	for i, step := range ph.Spec.Steps {
		if err := ValidateStep(i, step, declared); err != nil {
			return err
		}
	}
	return nil
}

// ValidateParams rejects a duplicate param name. The CRD schema already
// enforces this structurally (spec.params is a listType=map keyed on
// name), so this is defense in depth for a caller that builds a PostHook
// programmatically past that layer (for example a dry-run or a future
// in-process caller).
func ValidateParams(params []keziov1alpha2.PostHookParam) error {
	seen := make(map[string]bool, len(params))
	for i, p := range params {
		if seen[p.Name] {
			return fmt.Errorf("spec.params[%d]: duplicate param name %q", i, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// DeclaredPlaceholderNames returns the set of template placeholder names a
// PostHook's steps may reference: every declared param, plus the reserved
// names the deploy plan builder injects on its own.
func DeclaredPlaceholderNames(params []keziov1alpha2.PostHookParam) map[string]bool {
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

// ValidateStep validates one step at spec.steps[i]: exactly one kind is
// set, a script/chrootScript step has exactly one content source, an
// OS-restricted builtin declares osFamily=Linux, and every template
// placeholder in an inline script content references a declared param or a
// reserved name.
//
// The exactly-one-kind and exactly-one-source rules are also enforced by
// the CRD schema's CEL rules; re-checking them here keeps this function
// usable and testable independent of a running apiserver, and gives a
// field-path-qualified error message for the common case.
func ValidateStep(i int, step keziov1alpha2.PostHookStep, declared map[string]bool) error {
	path := fmt.Sprintf("spec.steps[%d]", i)

	switch step.Type() {
	case keziov1alpha2.PostHookStepTypeUnknown:
		return fmt.Errorf("%s: exactly one of builtin, script, or chrootScript must be set", path)
	case keziov1alpha2.PostHookStepTypeBuiltin:
		if err := validateOSFamilyGating(path, step); err != nil {
			return err
		}
		return validateBuiltinParams(path, *step.Builtin, declared)
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

// builtinAllowedParams names, per builtin, the params.* keys that builtin
// accepts (see internal/agent/deploy's finalize.go, which reads exactly
// these keys off ResolvedHookStep.Params): efibootmgr and
// install-removable-fallback both target one ESP partition ("disk",
// "part"); growLastPartition targets one partition to grow ("disk",
// "partition", "fsType"); mkswap takes none, since it always acts on
// every swap slot across the whole plan.
var builtinAllowedParams = map[string]map[string]bool{
	keziov1alpha2.BuiltinStepEfibootmgr:               {"disk": true, "part": true},
	keziov1alpha2.BuiltinStepInstallRemovableFallback: {"disk": true, "part": true},
	keziov1alpha2.BuiltinStepGrowLastPartition:        {"disk": true, "partition": true, "fsType": true},
	keziov1alpha2.BuiltinStepMkswap:                   {},
}

// validateBuiltinParams rejects a builtin step's params key not in
// builtinAllowedParams[step.Name], and checks every value's template
// placeholders the same way an inline script's content is checked
// (validatePlaceholders). step.Name not appearing in builtinAllowedParams
// at all is not reported here - the CRD schema's own Enum on
// PostHookBuiltinStep.Name already rejects any other value.
func validateBuiltinParams(path string, step keziov1alpha2.PostHookBuiltinStep, declared map[string]bool) error {
	allowed := builtinAllowedParams[step.Name]
	for key, value := range step.Params {
		if !allowed[key] {
			return fmt.Errorf("%s.builtin.params: %q is not a valid parameter for builtin %q", path, key, step.Name)
		}
		if err := validatePlaceholders(fmt.Sprintf("%s.builtin.params[%s]", path, key), value, declared); err != nil {
			return err
		}
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
		// here; PostHookReconciler checks that the reference itself
		// resolves (see internal/controller).
		return nil
	}
	return validatePlaceholders(path+".script", src.Script, declared)
}

// CheckOSFamilyCompatible rejects a step whose explicit osFamily does not
// match imageOSFamily: a step's own OSFamily doc comment says an absent
// value applies regardless, so such a step always passes here too,
// including a chrootScript step - the chroot it mounts is always this
// image's deployed root, so an unset osFamily never conflicts with it.
// Only an explicit mismatch is rejected, on any step kind: an attached
// step whose declared osFamily can never match the image it is checked
// against would simply never run once resolved, which is virtually always
// a configuration mistake worth catching at admission/resolve time rather
// than a hook intentionally covering several OS families in one object.
func CheckOSFamilyCompatible(hook *keziov1alpha2.PostHook, imageOSFamily string) error {
	for i, step := range hook.Spec.Steps {
		if step.OSFamily == "" || step.OSFamily == imageOSFamily {
			continue
		}
		return fmt.Errorf("spec.steps[%d]: osFamily %q is incompatible with the image's osFamily %q", i, step.OSFamily, imageOSFamily)
	}
	return nil
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
