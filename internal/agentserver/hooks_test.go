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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// rawParams builds an *apiextensionsv1.JSON from a JSON object literal,
// the same shape a Machine or Image's schemaless spec.params field
// carries.
func rawParams(jsonObj string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(jsonObj)}
}

func strPtr(s string) *string { return &s }

func TestResolveHooks_ScriptStepFetchesAndTemplatesConfigMapContent(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "notify", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Params: []keziov1alpha1.PostHookParam{
				{Name: "kernelArg", Default: strPtr("quiet")},
			},
			Steps: []keziov1alpha1.PostHookStep{
				{Script: &keziov1alpha1.PostHookScriptSource{
					ConfigMapRef:   &keziov1alpha1.ConfigMapKeyRef{Name: "notify-cm", Key: "run.sh"},
					TimeoutSeconds: 45,
				}},
			},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "notify-cm", Namespace: "default"},
		Data:       map[string]string{"run.sh": "echo {{ .machineName }} {{ .kernelArg }}\n"},
	}

	c := newPlanTestClient(t, hook, cm)

	hooks, err := resolveHooks(context.Background(), c, "default",
		nil, []keziov1alpha1.NameRef{{Name: "notify"}},
		nil, nil,
		"node-01", "", "")
	if err != nil {
		t.Fatalf("resolveHooks: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("len(hooks) = %d, want 1", len(hooks))
	}
	if hooks[0].Name != "notify" {
		t.Errorf("hooks[0].Name = %q, want notify", hooks[0].Name)
	}
	if len(hooks[0].Steps) != 1 {
		t.Fatalf("len(hooks[0].Steps) = %d, want 1", len(hooks[0].Steps))
	}
	step := hooks[0].Steps[0]
	if step.Type != agentapi.HookStepTypeScript {
		t.Errorf("Type = %q, want %q", step.Type, agentapi.HookStepTypeScript)
	}
	if want := "echo node-01 quiet\n"; step.Content != want {
		t.Errorf("Content = %q, want %q", step.Content, want)
	}
	if step.FromSecret {
		t.Error("FromSecret = true, want false for a configMapRef source")
	}
	if step.TimeoutSeconds != 45 {
		t.Errorf("TimeoutSeconds = %d, want 45", step.TimeoutSeconds)
	}
}

func TestResolveHooks_SecretSourcedContentFetchedAndFlagged(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "join-monitoring", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{ChrootScript: &keziov1alpha1.PostHookScriptSource{
					SecretRef: &keziov1alpha1.SecretKeyRef{Name: "monitor-token", Key: "join.sh"},
				}},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "monitor-token", Namespace: "default"},
		Data:       map[string][]byte{"join.sh": []byte("curl -H 'token: s3cr3t' https://monitor.example/join\n")},
	}

	c := newPlanTestClient(t, hook, secret)

	hooks, err := resolveHooks(context.Background(), c, "default",
		[]keziov1alpha1.NameRef{{Name: "join-monitoring"}}, nil,
		nil, nil,
		"node-01", "os-image", "/dev/sda")
	if err != nil {
		t.Fatalf("resolveHooks: %v", err)
	}
	step := hooks[0].Steps[0]
	if step.Type != agentapi.HookStepTypeChrootScript {
		t.Errorf("Type = %q, want %q", step.Type, agentapi.HookStepTypeChrootScript)
	}
	if !step.FromSecret {
		t.Error("FromSecret = false, want true for a secretRef source")
	}
	if !strings.Contains(step.Content, "s3cr3t") {
		t.Errorf("Content = %q, want the fetched secret content", step.Content)
	}
}

func TestResolveHooks_BuiltinStepCarriesNameAndTimeout(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "kezio-default-finalize", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{Builtin: &keziov1alpha1.PostHookBuiltinStep{Name: keziov1alpha1.BuiltinStepEfibootmgr}},
			},
		},
	}
	c := newPlanTestClient(t, hook)

	hooks, err := resolveHooks(context.Background(), c, "default",
		[]keziov1alpha1.NameRef{{Name: "kezio-default-finalize"}}, nil,
		nil, nil,
		"node-01", "os-image", "/dev/sda")
	if err != nil {
		t.Fatalf("resolveHooks: %v", err)
	}
	step := hooks[0].Steps[0]
	if step.Type != agentapi.HookStepTypeBuiltin {
		t.Errorf("Type = %q, want %q", step.Type, agentapi.HookStepTypeBuiltin)
	}
	if step.Builtin != keziov1alpha1.BuiltinStepEfibootmgr {
		t.Errorf("Builtin = %q, want %q", step.Builtin, keziov1alpha1.BuiltinStepEfibootmgr)
	}
	if step.TimeoutSeconds != keziov1alpha1.PostHookDefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want the default %d", step.TimeoutSeconds, keziov1alpha1.PostHookDefaultTimeoutSeconds)
	}
}

func TestResolveHooks_ParamMergePrecedenceMachineOverridesImage(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "regen-initramfs", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Params: []keziov1alpha1.PostHookParam{
				{Name: "kernelArg", Default: strPtr("fallback")},
				{Name: "imageOnly", Default: strPtr("fallback")},
			},
			Steps: []keziov1alpha1.PostHookStep{
				{Script: &keziov1alpha1.PostHookScriptSource{
					Script: "{{ .kernelArg }} {{ .imageOnly }}",
				}},
			},
		},
	}
	c := newPlanTestClient(t, hook)

	imageParams := rawParams(`{"kernelArg":"from-image","imageOnly":"image-value"}`)
	machineParams := rawParams(`{"kernelArg":"from-machine"}`)

	hooks, err := resolveHooks(context.Background(), c, "default",
		[]keziov1alpha1.NameRef{{Name: "regen-initramfs"}}, nil,
		imageParams, machineParams,
		"node-01", "os-image", "/dev/sda")
	if err != nil {
		t.Fatalf("resolveHooks: %v", err)
	}
	got := hooks[0].Steps[0].Content
	if want := "from-machine image-value"; got != want {
		t.Errorf("Content = %q, want %q (machine params override image params on collision, image params otherwise preserved)", got, want)
	}
}

func TestResolveHooks_ImageHooksThenMachineHooksOrder(t *testing.T) {
	imageHook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "image-hook", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{{Script: &keziov1alpha1.PostHookScriptSource{Script: "from-image"}}},
		},
	}
	machineHook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-hook", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{{Script: &keziov1alpha1.PostHookScriptSource{Script: "from-machine"}}},
		},
	}
	c := newPlanTestClient(t, imageHook, machineHook)

	hooks, err := resolveHooks(context.Background(), c, "default",
		[]keziov1alpha1.NameRef{{Name: "image-hook"}}, []keziov1alpha1.NameRef{{Name: "machine-hook"}},
		nil, nil,
		"node-01", "os-image", "/dev/sda")
	if err != nil {
		t.Fatalf("resolveHooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("len(hooks) = %d, want 2", len(hooks))
	}
	if hooks[0].Name != "image-hook" || hooks[1].Name != "machine-hook" {
		t.Errorf("hooks order = [%s, %s], want [image-hook, machine-hook] (Image.spec.postHookRefs runs before Machine.spec.postHookRefs)", hooks[0].Name, hooks[1].Name)
	}
}

func TestResolveHooks_MissingPostHookIsAnError(t *testing.T) {
	c := newPlanTestClient(t)

	_, err := resolveHooks(context.Background(), c, "default",
		nil, []keziov1alpha1.NameRef{{Name: "does-not-exist"}},
		nil, nil,
		"node-01", "", "")
	if err == nil {
		t.Fatal("resolveHooks returned no error for a dangling postHookRefs entry")
	}
}

func TestResolveHooks_MissingSecretKeyIsAnError(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-secret", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{Script: &keziov1alpha1.PostHookScriptSource{SecretRef: &keziov1alpha1.SecretKeyRef{Name: "creds", Key: "missing-key"}}},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("x")},
	}
	c := newPlanTestClient(t, hook, secret)

	_, err := resolveHooks(context.Background(), c, "default",
		nil, []keziov1alpha1.NameRef{{Name: "bad-secret"}},
		nil, nil,
		"node-01", "", "")
	if err == nil {
		t.Fatal("resolveHooks returned no error for a secretRef key that does not exist")
	}
}

func TestResolveHooks_UnresolvedPlaceholderIsAnError(t *testing.T) {
	hook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "needs-param", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{Script: &keziov1alpha1.PostHookScriptSource{Script: "{{ .undeclaredParam }}"}},
			},
		},
	}
	c := newPlanTestClient(t, hook)

	_, err := resolveHooks(context.Background(), c, "default",
		nil, []keziov1alpha1.NameRef{{Name: "needs-param"}},
		nil, nil,
		"node-01", "", "")
	if err == nil {
		t.Fatal("resolveHooks returned no error for a template placeholder with no value and no default")
	}
}

// TestBuildDeployPlan_HooksResolvedFromImageAndMachine exercises hook
// resolution end to end through buildDeployPlan: the OS Image's own
// postHookRefs first, then the Machine's, using the Image's targetDisk
// and name as the standard template values.
func TestBuildDeployPlan_HooksResolvedFromImageAndMachine(t *testing.T) {
	storeRoot := t.TempDir()

	imageHook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "image-hook", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{Script: &keziov1alpha1.PostHookScriptSource{Script: "disk={{ .targetDisk }} image={{ .imageName }}"}},
			},
		},
	}
	machineHook := &keziov1alpha1.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-hook", Namespace: "default"},
		Spec: keziov1alpha1.PostHookSpec{
			Steps: []keziov1alpha1.PostHookStep{
				{ChrootScript: &keziov1alpha1.PostHookScriptSource{Script: "machine={{ .machineName }}"}},
			},
		},
	}

	image, cm := readyImage("os-image", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
	})
	image.Spec.PostHookRefs = []keziov1alpha1.NameRef{{Name: "image-hook"}}

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			ImageRef:     &keziov1alpha1.NameRef{Name: "os-image"},
			PostHookRefs: []keziov1alpha1.NameRef{{Name: "machine-hook"}},
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, imageHook, machineHook, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("buildDeployPlan returned a nil plan; want a ready plan")
	}
	if len(plan.Hooks) != 2 {
		t.Fatalf("len(plan.Hooks) = %d, want 2", len(plan.Hooks))
	}
	if plan.Hooks[0].Name != "image-hook" || plan.Hooks[1].Name != "machine-hook" {
		t.Fatalf("hooks order = [%s, %s], want [image-hook, machine-hook]", plan.Hooks[0].Name, plan.Hooks[1].Name)
	}
	if want := "disk=/dev/nvme0n1 image=os-image"; plan.Hooks[0].Steps[0].Content != want {
		t.Errorf("image-hook content = %q, want %q", plan.Hooks[0].Steps[0].Content, want)
	}
	if want := "machine=node-01"; plan.Hooks[1].Steps[0].Content != want {
		t.Errorf("machine-hook content = %q, want %q", plan.Hooks[1].Steps[0].Content, want)
	}
}
