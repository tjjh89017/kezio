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

package planbuild

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"strconv"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/posthookvalidate"
)

// resolveHooks builds the ordered agentapi.ResolvedHook list for refs, in
// the given order, against the shared template data (the deploy's merged
// params plus reserved values). Every step's script content is fetched
// (configMapRef/secretRef) and templated here, server-side, so the agent
// needs no cluster access to run any of it.
//
// A PostHook that does not exist or is not Valid is reported as
// NotReadyError, not a hard failure: it may simply not have caught up
// with the PostHookReconciler yet.
//
// imageOSFamily is the resolved OS image's effective OSFamily, empty for a
// dataImages-only run (no OS image to check compatibility against) -
// resolveHook skips the compatibility check in that case.
func (b *Builder) resolveHooks(ctx context.Context, defaultNS string, refs []keziov1alpha3.NameRef, shared map[string]any, defaults builtinDefaults, imageOSFamily string) ([]agentapi.ResolvedHook, error) {
	hooks := make([]agentapi.ResolvedHook, 0, len(refs))
	for _, ref := range refs {
		hook, err := b.resolveHook(ctx, defaultNS, ref, shared, defaults, imageOSFamily)
		if err != nil {
			return nil, fmt.Errorf("posthook %q: %w", ref.Name, err)
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

// resolveHook fetches ref's PostHook, checks it against imageOSFamily
// (posthookvalidate.CheckOSFamilyCompatible, skipped when imageOSFamily is
// empty), and resolves every one of its steps against shared (the deploy's
// merged params plus reserved values), filling in each declared param's
// own default for any name shared does not already carry. Checking
// compatibility here - the single place both an Image's own postHookRefs
// and a Machine's postHookRefs resolve through - is what makes the rule
// hold regardless of which side referenced the hook.
func (b *Builder) resolveHook(ctx context.Context, defaultNS string, ref keziov1alpha3.NameRef, shared map[string]any, defaults builtinDefaults, imageOSFamily string) (agentapi.ResolvedHook, error) {
	ns := resolveNamespace(ref, defaultNS)

	hook := &keziov1alpha3.PostHook{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, hook); err != nil {
		if apierrors.IsNotFound(err) {
			return agentapi.ResolvedHook{}, &NotReadyError{Reason: fmt.Sprintf("posthook %s/%s not found", ns, ref.Name)}
		}
		return agentapi.ResolvedHook{}, fmt.Errorf("get posthook %s/%s: %w", ns, ref.Name, err)
	}
	if !meta.IsStatusConditionTrue(hook.Status.Conditions, keziov1alpha3.PostHookConditionValid) {
		return agentapi.ResolvedHook{}, &NotReadyError{Reason: fmt.Sprintf("posthook %s/%s is not Valid yet", ns, ref.Name)}
	}
	if imageOSFamily != "" {
		if err := posthookvalidate.CheckOSFamilyCompatible(hook, imageOSFamily); err != nil {
			return agentapi.ResolvedHook{}, &ValidationError{Reason: fmt.Sprintf("posthook %s/%s: %v", ns, ref.Name, err)}
		}
	}

	data := hookTemplateData(shared, hook)

	steps := make([]agentapi.ResolvedHookStep, 0, len(hook.Spec.Steps))
	for i, step := range hook.Spec.Steps {
		resolved, err := b.resolveStep(ctx, ns, step, data, defaults)
		if err != nil {
			return agentapi.ResolvedHook{}, fmt.Errorf("step %d: %w", i, err)
		}
		steps = append(steps, resolved)
	}

	return agentapi.ResolvedHook{Name: hook.Name, Steps: steps}, nil
}

// hookTemplateData returns a fresh map carrying every entry of shared,
// plus hook's own declared params' defaults for any name shared does not
// already set - so one PostHook's own defaults never leak into another
// hook resolved from the same shared map.
func hookTemplateData(shared map[string]any, hook *keziov1alpha3.PostHook) map[string]any {
	data := make(map[string]any, len(shared)+len(hook.Spec.Params))
	maps.Copy(data, shared)
	for _, p := range hook.Spec.Params {
		if _, ok := data[p.Name]; ok {
			continue
		}
		if p.Default != nil {
			data[p.Name] = *p.Default
		}
	}
	return data
}

// resolveStep resolves one PostHookStep into its wire ResolvedHookStep: a
// builtin step carries its name through verbatim; a script step has its
// content fetched (fetchScriptSource) and templated (renderTemplate)
// against data.
func (b *Builder) resolveStep(ctx context.Context, ns string, step keziov1alpha3.PostHookStep, data map[string]any, defaults builtinDefaults) (agentapi.ResolvedHookStep, error) {
	switch step.Type() {
	case keziov1alpha3.PostHookStepTypeBuiltin:
		params, err := resolveBuiltinParams(step.Builtin.Name, step.Builtin.Params, defaults, data)
		if err != nil {
			return agentapi.ResolvedHookStep{}, err
		}
		return agentapi.ResolvedHookStep{
			Type:           agentapi.HookStepTypeBuiltin,
			Builtin:        step.Builtin.Name,
			Params:         params,
			OSFamily:       step.OSFamily,
			TimeoutSeconds: step.Builtin.EffectiveTimeoutSeconds(),
		}, nil

	case keziov1alpha3.PostHookStepTypeScript:
		return b.resolveScriptStep(ctx, ns, step.OSFamily, *step.Script, data)

	default:
		return agentapi.ResolvedHookStep{}, fmt.Errorf("step has neither builtin nor script set")
	}
}

// resolveScriptStep fetches source's content, templates it against data,
// and wraps the result as a script ResolvedHookStep.
func (b *Builder) resolveScriptStep(ctx context.Context, ns, osFamily string, source keziov1alpha3.PostHookScriptSource, data map[string]any) (agentapi.ResolvedHookStep, error) {
	raw, err := b.fetchScriptSource(ctx, ns, source)
	if err != nil {
		return agentapi.ResolvedHookStep{}, err
	}
	content, err := renderTemplate(raw, data)
	if err != nil {
		return agentapi.ResolvedHookStep{}, &ValidationError{Reason: fmt.Sprintf("templating script: %v", err)}
	}
	return agentapi.ResolvedHookStep{
		Type:           agentapi.HookStepTypeScript,
		Content:        content,
		OSFamily:       osFamily,
		TimeoutSeconds: source.EffectiveTimeoutSeconds(),
	}, nil
}

// fetchScriptSource returns source's raw (untemplated) script content -
// copied verbatim for an inline script, or read from the named
// ConfigMap/Secret key otherwise. A missing ConfigMap/Secret or key is
// reported as NotReadyError: PostHookConditionValid already checks this
// resolves (see posthookvalidate's doc comment), so a fetch failure here
// should be transient - the PostHookReconciler will flip Valid=False on
// its own next pass.
func (b *Builder) fetchScriptSource(ctx context.Context, ns string, source keziov1alpha3.PostHookScriptSource) (string, error) {
	switch source.SourceKind() {
	case keziov1alpha3.PostHookScriptSourceInline:
		return source.Script, nil

	case keziov1alpha3.PostHookScriptSourceConfigMapRef:
		cm := &corev1.ConfigMap{}
		if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: source.ConfigMapRef.Name}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return "", &NotReadyError{Reason: fmt.Sprintf("configmap %s/%s not found", ns, source.ConfigMapRef.Name)}
			}
			return "", fmt.Errorf("get configmap %s/%s: %w", ns, source.ConfigMapRef.Name, err)
		}
		v, ok := cm.Data[source.ConfigMapRef.Key]
		if !ok {
			return "", &NotReadyError{Reason: fmt.Sprintf("configmap %s/%s: missing key %q", ns, source.ConfigMapRef.Name, source.ConfigMapRef.Key)}
		}
		return v, nil

	case keziov1alpha3.PostHookScriptSourceSecretRef:
		secret := &corev1.Secret{}
		if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: source.SecretRef.Name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return "", &NotReadyError{Reason: fmt.Sprintf("secret %s/%s not found", ns, source.SecretRef.Name)}
			}
			return "", fmt.Errorf("get secret %s/%s: %w", ns, source.SecretRef.Name, err)
		}
		v, ok := secret.Data[source.SecretRef.Key]
		if !ok {
			return "", &NotReadyError{Reason: fmt.Sprintf("secret %s/%s: missing key %q", ns, source.SecretRef.Name, source.SecretRef.Key)}
		}
		return string(v), nil

	default:
		return "", fmt.Errorf("script source has none of script, configMapRef, or secretRef set")
	}
}

// renderTemplate executes raw as a text/template against data, failing
// (rather than silently emitting "<no value>") on any {{ .name }}
// placeholder data does not carry.
func renderTemplate(raw string, data map[string]any) (string, error) {
	tmpl, err := template.New("posthook").Option("missingkey=error").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// espDependentBuiltins names the builtins that require an ESP partition
// number ("part") and have no runtime fallback for it: unlike "disk" (a
// dataImages-only deploy simply has no OS disk to default against) or
// growLastPartition's "partition" (the agent itself already falls back to
// the plan's last slot when none is given), there is no way to guess
// "which partition is the ESP" without either an esp-role slot on the OS
// image or an explicit user override.
var espDependentBuiltins = map[string]bool{
	keziov1alpha3.BuiltinStepEfibootmgr:               true,
	keziov1alpha3.BuiltinStepInstallRemovableFallback: true,
}

// resolveBuiltinParams computes one builtin step's ResolvedHookStep.Params:
// name's own derived defaults (deriveBuiltinDefaults), then userParams -
// each value templated against data - overriding on key collision. Returns
// a ValidationError when name needs an ESP partition number
// (espDependentBuiltins) and none is available after the merge.
func resolveBuiltinParams(name string, userParams map[string]string, defaults builtinDefaults, data map[string]any) (map[string]string, error) {
	merged := map[string]string{}
	switch name {
	case keziov1alpha3.BuiltinStepEfibootmgr, keziov1alpha3.BuiltinStepInstallRemovableFallback:
		if defaults.disk != "" {
			merged["disk"] = defaults.disk
		}
		if defaults.espPartition > 0 {
			merged["part"] = strconv.Itoa(int(defaults.espPartition))
		}
	case keziov1alpha3.BuiltinStepGrowLastPartition:
		if defaults.disk != "" {
			merged["disk"] = defaults.disk
		}
		if defaults.lastPartition > 0 {
			merged["partition"] = strconv.Itoa(int(defaults.lastPartition))
		}
	}

	for k, v := range userParams {
		rendered, err := renderTemplate(v, data)
		if err != nil {
			return nil, &ValidationError{Reason: fmt.Sprintf("builtin %q param %q: %v", name, k, err)}
		}
		merged[k] = rendered
	}

	if espDependentBuiltins[name] && merged["part"] == "" {
		return nil, &ValidationError{Reason: fmt.Sprintf("builtin %q needs the OS image's ESP partition number but none could be identified: no slot has role %q, and params.part was not set explicitly", name, keziov1alpha3.PartitionRoleESP)}
	}

	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

// resolveNamespace returns ref's namespace when set, defaultNS otherwise -
// the resolution rule NameRef's doc comment fixes.
func resolveNamespace(ref keziov1alpha3.NameRef, defaultNS string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return defaultNS
}
