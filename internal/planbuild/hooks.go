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
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
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
func (b *Builder) resolveHooks(ctx context.Context, defaultNS string, refs []keziov1alpha2.NameRef, shared map[string]any) ([]agentapi.ResolvedHook, error) {
	hooks := make([]agentapi.ResolvedHook, 0, len(refs))
	for _, ref := range refs {
		hook, err := b.resolveHook(ctx, defaultNS, ref, shared)
		if err != nil {
			return nil, fmt.Errorf("posthook %q: %w", ref.Name, err)
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

// resolveHook fetches ref's PostHook and resolves every one of its steps
// against shared (the deploy's merged params plus reserved values),
// filling in each declared param's own default for any name shared does
// not already carry.
func (b *Builder) resolveHook(ctx context.Context, defaultNS string, ref keziov1alpha2.NameRef, shared map[string]any) (agentapi.ResolvedHook, error) {
	ns := resolveNamespace(ref, defaultNS)

	hook := &keziov1alpha2.PostHook{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, hook); err != nil {
		if apierrors.IsNotFound(err) {
			return agentapi.ResolvedHook{}, &NotReadyError{Reason: fmt.Sprintf("posthook %s/%s not found", ns, ref.Name)}
		}
		return agentapi.ResolvedHook{}, fmt.Errorf("get posthook %s/%s: %w", ns, ref.Name, err)
	}
	if !meta.IsStatusConditionTrue(hook.Status.Conditions, keziov1alpha2.PostHookConditionValid) {
		return agentapi.ResolvedHook{}, &NotReadyError{Reason: fmt.Sprintf("posthook %s/%s is not Valid yet", ns, ref.Name)}
	}

	data := hookTemplateData(shared, hook)

	steps := make([]agentapi.ResolvedHookStep, 0, len(hook.Spec.Steps))
	for i, step := range hook.Spec.Steps {
		resolved, err := b.resolveStep(ctx, ns, step, data)
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
func hookTemplateData(shared map[string]any, hook *keziov1alpha2.PostHook) map[string]any {
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
// builtin step carries its name through verbatim; a script or
// chrootScript step has its content fetched (fetchScriptSource) and
// templated (renderTemplate) against data.
func (b *Builder) resolveStep(ctx context.Context, ns string, step keziov1alpha2.PostHookStep, data map[string]any) (agentapi.ResolvedHookStep, error) {
	switch step.Type() {
	case keziov1alpha2.PostHookStepTypeBuiltin:
		return agentapi.ResolvedHookStep{
			Type:           agentapi.HookStepTypeBuiltin,
			Builtin:        step.Builtin.Name,
			OSFamily:       step.OSFamily,
			TimeoutSeconds: step.Builtin.EffectiveTimeoutSeconds(),
		}, nil

	case keziov1alpha2.PostHookStepTypeScript:
		return b.resolveScriptStep(ctx, ns, agentapi.HookStepTypeScript, step.OSFamily, *step.Script, data)

	case keziov1alpha2.PostHookStepTypeChrootScript:
		return b.resolveScriptStep(ctx, ns, agentapi.HookStepTypeChrootScript, step.OSFamily, *step.ChrootScript, data)

	default:
		return agentapi.ResolvedHookStep{}, fmt.Errorf("step has no builtin, script, or chrootScript set")
	}
}

// resolveScriptStep fetches source's content, templates it against data,
// and wraps the result as a ResolvedHookStep of the given wire type.
func (b *Builder) resolveScriptStep(ctx context.Context, ns, stepType, osFamily string, source keziov1alpha2.PostHookScriptSource, data map[string]any) (agentapi.ResolvedHookStep, error) {
	raw, err := b.fetchScriptSource(ctx, ns, source)
	if err != nil {
		return agentapi.ResolvedHookStep{}, err
	}
	content, err := renderTemplate(raw, data)
	if err != nil {
		return agentapi.ResolvedHookStep{}, &ValidationError{Reason: fmt.Sprintf("templating script: %v", err)}
	}
	return agentapi.ResolvedHookStep{
		Type:           stepType,
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
func (b *Builder) fetchScriptSource(ctx context.Context, ns string, source keziov1alpha2.PostHookScriptSource) (string, error) {
	switch source.SourceKind() {
	case keziov1alpha2.PostHookScriptSourceInline:
		return source.Script, nil

	case keziov1alpha2.PostHookScriptSourceConfigMapRef:
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

	case keziov1alpha2.PostHookScriptSourceSecretRef:
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

// resolveNamespace returns ref's namespace when set, defaultNS otherwise -
// the resolution rule NameRef's doc comment fixes.
func resolveNamespace(ref keziov1alpha2.NameRef, defaultNS string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return defaultNS
}
