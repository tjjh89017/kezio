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

package agentserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// resolveHooks builds the ordered agentapi.ResolvedHook list for a
// deploy: imageRefs (the OS Image's own spec.postHookRefs) first, then
// machineRefs (the Machine's own spec.postHookRefs) - see
// keziov1alpha1.ImageSpec.PostHookRefs and
// keziov1alpha1.MachineSpec.PostHookRefs' doc comments, which fix this
// order and document it as part of the API. Every step's script content
// is fetched (configMapRef/secretRef) and templated here, server-side,
// against the merged params (imageParams first, then machineParams
// overriding - same order and precedence as PostHookRefs) plus the
// standard machineName/imageName/targetDisk values, so the agent needs
// no cluster access to run any of it.
//
// A PostHook that does not exist, or a configMapRef/secretRef key that
// does not exist, is reported as an error - not as "not ready yet" the
// way a not-yet-Ready Image is - because postHookRefs is a small,
// operator-authored list rather than something that resolves itself
// given enough time: a dangling ref is a configuration mistake, and
// buildDeployPlan surfaces it as a build error the same way a Ready
// Image missing its own layout ConfigMap is surfaced.
func resolveHooks(
	ctx context.Context,
	c client.Client,
	namespace string,
	imageRefs, machineRefs []keziov1alpha1.NameRef,
	imageParams, machineParams *apiextensionsv1.JSON,
	machineName, imageName, targetDisk string,
) ([]agentapi.ResolvedHook, error) {
	merged, err := mergeParams(imageParams, machineParams)
	if err != nil {
		return nil, fmt.Errorf("merging posthook params: %w", err)
	}
	// Standard values KEZIO injects, per PostHook's templating doc
	// comment. Set after merging so a declared param cannot shadow them.
	merged["machineName"] = machineName
	merged["imageName"] = imageName
	merged["targetDisk"] = targetDisk

	var hooks []agentapi.ResolvedHook
	for _, ref := range imageRefs {
		hook, err := resolveHook(ctx, c, namespace, ref, merged)
		if err != nil {
			return nil, fmt.Errorf("image posthook %q: %w", ref.Name, err)
		}
		hooks = append(hooks, hook)
	}
	for _, ref := range machineRefs {
		hook, err := resolveHook(ctx, c, namespace, ref, merged)
		if err != nil {
			return nil, fmt.Errorf("machine posthook %q: %w", ref.Name, err)
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

// jsonToMap decodes j's raw JSON into a map, treating a nil j or an
// empty Raw (Params never set) as an empty map rather than an error -
// the same "absent means no params" reading ImageSpec.Params and
// MachineSpec.Params's doc comments give the field.
func jsonToMap(j *apiextensionsv1.JSON) (map[string]any, error) {
	m := map[string]any{}
	if j == nil || len(j.Raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return nil, fmt.Errorf("parsing params JSON: %w", err)
	}
	return m, nil
}

// mergeParams merges imageParams and machineParams into a single map,
// imageParams first and machineParams overriding on key collision - the
// order ImageSpec.Params and MachineSpec.Params's doc comments fix.
func mergeParams(imageParams, machineParams *apiextensionsv1.JSON) (map[string]any, error) {
	merged, err := jsonToMap(imageParams)
	if err != nil {
		return nil, fmt.Errorf("image params: %w", err)
	}
	fromMachine, err := jsonToMap(machineParams)
	if err != nil {
		return nil, fmt.Errorf("machine params: %w", err)
	}
	for k, v := range fromMachine {
		merged[k] = v
	}
	return merged, nil
}

// resolveHook fetches ref's PostHook and resolves every one of its steps
// against shared (the deploy's merged params plus standard values),
// filling in each declared param's own default for any name shared does
// not already carry.
func resolveHook(ctx context.Context, c client.Client, defaultNS string, ref keziov1alpha1.NameRef, shared map[string]any) (agentapi.ResolvedHook, error) {
	ns := keziov1alpha1.ResolveNamespace(ref, defaultNS)

	hook := &keziov1alpha1.PostHook{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, hook); err != nil {
		return agentapi.ResolvedHook{}, fmt.Errorf("get posthook %s/%s: %w", ns, ref.Name, err)
	}

	data := hookTemplateData(shared, hook)

	steps := make([]agentapi.ResolvedHookStep, 0, len(hook.Spec.Steps))
	for i, step := range hook.Spec.Steps {
		resolved, err := resolveStep(ctx, c, ns, step, data)
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
func hookTemplateData(shared map[string]any, hook *keziov1alpha1.PostHook) map[string]any {
	data := make(map[string]any, len(shared)+len(hook.Spec.Params))
	for k, v := range shared {
		data[k] = v
	}
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

// resolveStep resolves one PostHookStep into its wire ResolvedHookStep:
// a builtin step carries its name through verbatim; a script or
// chrootScript step has its content fetched (fetchScriptSource) and
// templated (renderTemplate) against data.
func resolveStep(ctx context.Context, c client.Client, ns string, step keziov1alpha1.PostHookStep, data map[string]any) (agentapi.ResolvedHookStep, error) {
	switch step.Type() {
	case keziov1alpha1.PostHookStepTypeBuiltin:
		return agentapi.ResolvedHookStep{
			Type:           agentapi.HookStepTypeBuiltin,
			Builtin:        step.Builtin.Name,
			TimeoutSeconds: step.Builtin.EffectiveTimeoutSeconds(),
		}, nil

	case keziov1alpha1.PostHookStepTypeScript:
		return resolveScriptStep(ctx, c, ns, agentapi.HookStepTypeScript, *step.Script, data)

	case keziov1alpha1.PostHookStepTypeChrootScript:
		return resolveScriptStep(ctx, c, ns, agentapi.HookStepTypeChrootScript, *step.ChrootScript, data)

	default:
		return agentapi.ResolvedHookStep{}, fmt.Errorf("step has no builtin, script, or chrootScript set")
	}
}

// resolveScriptStep fetches source's content, templates it against data,
// and wraps the result as a ResolvedHookStep of the given wire type.
func resolveScriptStep(ctx context.Context, c client.Client, ns string, stepType string, source keziov1alpha1.PostHookScriptSource, data map[string]any) (agentapi.ResolvedHookStep, error) {
	raw, fromSecret, err := fetchScriptSource(ctx, c, ns, source)
	if err != nil {
		return agentapi.ResolvedHookStep{}, err
	}
	content, err := renderTemplate(raw, data)
	if err != nil {
		return agentapi.ResolvedHookStep{}, fmt.Errorf("templating script: %w", err)
	}
	return agentapi.ResolvedHookStep{
		Type:           stepType,
		Content:        content,
		FromSecret:     fromSecret,
		TimeoutSeconds: source.EffectiveTimeoutSeconds(),
	}, nil
}

// fetchScriptSource returns source's raw (untemplated) script content -
// copied verbatim for an inline script, or read from the named
// ConfigMap/Secret key otherwise - and whether it came from a Secret.
func fetchScriptSource(ctx context.Context, c client.Client, ns string, source keziov1alpha1.PostHookScriptSource) (content string, fromSecret bool, err error) {
	switch source.SourceKind() {
	case keziov1alpha1.PostHookScriptSourceInline:
		return source.Script, false, nil

	case keziov1alpha1.PostHookScriptSourceConfigMapRef:
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: source.ConfigMapRef.Name}, cm); err != nil {
			return "", false, fmt.Errorf("get configmap %s/%s: %w", ns, source.ConfigMapRef.Name, err)
		}
		v, ok := cm.Data[source.ConfigMapRef.Key]
		if !ok {
			return "", false, fmt.Errorf("configmap %s/%s: missing key %q", ns, source.ConfigMapRef.Name, source.ConfigMapRef.Key)
		}
		return v, false, nil

	case keziov1alpha1.PostHookScriptSourceSecretRef:
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: source.SecretRef.Name}, secret); err != nil {
			return "", false, fmt.Errorf("get secret %s/%s: %w", ns, source.SecretRef.Name, err)
		}
		v, ok := secret.Data[source.SecretRef.Key]
		if !ok {
			return "", false, fmt.Errorf("secret %s/%s: missing key %q", ns, source.SecretRef.Name, source.SecretRef.Key)
		}
		return string(v), true, nil

	default:
		return "", false, fmt.Errorf("script source has none of script, configMapRef, or secretRef set")
	}
}

// renderTemplate executes raw as a text/template against data, failing
// (rather than silently emitting "<no value>") on any {{ .name }}
// placeholder data does not carry - see PostHook's templating doc
// comment: "unresolved placeholders fail validation, not the
// deployment"; resolveHooks is where that failure surfaces for a plan
// build, since PostHook itself has no admission-time check that can see
// the attaching resource's params.
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
