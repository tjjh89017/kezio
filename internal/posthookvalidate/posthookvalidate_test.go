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

package posthookvalidate

import (
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func validBuiltinStep() keziov1alpha2.PostHookStep {
	return keziov1alpha2.PostHookStep{
		Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepInstallRemovableFallback},
	}
}

func TestValidateParams(t *testing.T) {
	tests := []struct {
		name    string
		params  []keziov1alpha2.PostHookParam
		wantErr string
	}{
		{
			name:   "no params",
			params: nil,
		},
		{
			name: "unique names",
			params: []keziov1alpha2.PostHookParam{
				{Name: "a"}, {Name: "b"},
			},
		},
		{
			name: "duplicate name",
			params: []keziov1alpha2.PostHookParam{
				{Name: "a"}, {Name: "a"},
			},
			wantErr: `spec.params[1]: duplicate param name "a"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateParams(tt.params)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateStep(t *testing.T) {
	declared := map[string]bool{
		"foo":         true,
		"machineName": true,
		"imageName":   true,
		"targetDisk":  true,
	}

	tests := []struct {
		name    string
		step    keziov1alpha2.PostHookStep
		wantErr string
	}{
		{
			name:    "no kind set",
			step:    keziov1alpha2.PostHookStep{},
			wantErr: "spec.steps[0]: exactly one of builtin or script must be set",
		},
		{
			name: "builtin only, unrestricted",
			step: validBuiltinStep(),
		},
		{
			name: "restricted builtin without osFamily",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepMkswap},
			},
			wantErr: `spec.steps[0].builtin: "mkswap" requires osFamily to be set to "Linux", got ""`,
		},
		{
			name: "restricted builtin with wrong osFamily",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyWindows,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepEfibootmgr},
			},
			wantErr: `spec.steps[0].builtin: "efibootmgr" requires osFamily to be set to "Linux", got "Windows"`,
		},
		{
			name: "restricted builtin with osFamily linux",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin:  &keziov1alpha2.PostHookBuiltinStep{Name: keziov1alpha2.BuiltinStepGrowLastPartition},
			},
		},
		{
			name: "script with declared placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .foo }}"},
			},
		},
		{
			name: "script with reserved placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .machineName }}"},
			},
		},
		{
			name: "script with undeclared placeholder",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{Script: "echo {{ .bogus }}"},
			},
			wantErr: `spec.steps[0].script.script: placeholder "bogus" does not reference a declared param or a reserved name (machineName, imageName, targetDisk)`,
		},
		{
			name: "builtin with allowed param",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepInstallRemovableFallback,
					Params: map[string]string{"disk": "/dev/sda", "part": "1"},
				},
			},
		},
		{
			name: "builtin with unknown param key",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepInstallRemovableFallback,
					Params: map[string]string{"bogus": "x"},
				},
			},
			wantErr: `spec.steps[0].builtin.params: "bogus" is not a valid parameter for builtin "install-removable-fallback"`,
		},
		{
			name: "mkswap rejects any param",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepMkswap,
					Params: map[string]string{"disk": "/dev/sda"},
				},
			},
			wantErr: `spec.steps[0].builtin.params: "disk" is not a valid parameter for builtin "mkswap"`,
		},
		{
			name: "builtin param with declared placeholder",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepInstallRemovableFallback,
					Params: map[string]string{"disk": "{{ .foo }}"},
				},
			},
		},
		{
			name: "builtin param with undeclared placeholder",
			step: keziov1alpha2.PostHookStep{
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepInstallRemovableFallback,
					Params: map[string]string{"disk": "{{ .bogus }}"},
				},
			},
			wantErr: `spec.steps[0].builtin.params[disk]: placeholder "bogus" does not reference a declared param or a reserved name (machineName, imageName, targetDisk)`,
		},
		{
			name: "growLastPartition allows disk, partition, fsType",
			step: keziov1alpha2.PostHookStep{
				OSFamily: keziov1alpha2.OSFamilyLinux,
				Builtin: &keziov1alpha2.PostHookBuiltinStep{
					Name:   keziov1alpha2.BuiltinStepGrowLastPartition,
					Params: map[string]string{"disk": "/dev/sda", "partition": "3", "fsType": "ext4"},
				},
			},
		},
		{
			name: "script sourced from configMapRef skips placeholder checks",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{ConfigMapRef: &keziov1alpha2.ConfigMapKeyRef{Name: "cm", Key: "run.sh"}},
			},
		},
		{
			name: "script with no source set",
			step: keziov1alpha2.PostHookStep{
				Script: &keziov1alpha2.PostHookScriptSource{},
			},
			wantErr: "spec.steps[0].script: exactly one of script, configMapRef, or secretRef must be set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStep(0, tt.step, declared)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeclaredPlaceholderNames(t *testing.T) {
	names := DeclaredPlaceholderNames([]keziov1alpha2.PostHookParam{{Name: "foo"}})
	for _, want := range []string{"foo", "machineName", "imageName", "targetDisk"} {
		if !names[want] {
			t.Errorf("expected %q to be a declared placeholder name", want)
		}
	}
	if names["bogus"] {
		t.Error("expected bogus to not be declared")
	}
}

func TestCheckOSFamilyCompatible(t *testing.T) {
	tests := []struct {
		name    string
		steps   []keziov1alpha2.PostHookStep
		image   string
		wantErr string
	}{
		{
			name:  "no steps",
			steps: nil,
			image: keziov1alpha2.OSFamilyLinux,
		},
		{
			name: "step with no osFamily applies regardless",
			steps: []keziov1alpha2.PostHookStep{
				{Script: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"}},
			},
			image: keziov1alpha2.OSFamilyWindows,
		},
		{
			name: "matching explicit osFamily",
			steps: []keziov1alpha2.PostHookStep{
				{OSFamily: keziov1alpha2.OSFamilyLinux, Script: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"}},
			},
			image: keziov1alpha2.OSFamilyLinux,
		},
		{
			name: "mismatched explicit osFamily on a script step",
			steps: []keziov1alpha2.PostHookStep{
				{OSFamily: keziov1alpha2.OSFamilyWindows, Script: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"}},
			},
			image:   keziov1alpha2.OSFamilyLinux,
			wantErr: `spec.steps[0]: osFamily "Windows" is incompatible with the image's osFamily "Linux"`,
		},
		{
			name: "script step with no osFamily applies regardless, no error",
			steps: []keziov1alpha2.PostHookStep{
				{Script: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"}},
			},
			image: keziov1alpha2.OSFamilyWindows,
		},
		{
			name: "the offending step is named when it is not the first one",
			steps: []keziov1alpha2.PostHookStep{
				{OSFamily: keziov1alpha2.OSFamilyLinux, Script: &keziov1alpha2.PostHookScriptSource{Script: "echo hi"}},
				{OSFamily: keziov1alpha2.OSFamilyWindows, Script: &keziov1alpha2.PostHookScriptSource{Script: "echo bye"}},
			},
			image:   keziov1alpha2.OSFamilyLinux,
			wantErr: `spec.steps[1]: osFamily "Windows" is incompatible with the image's osFamily "Linux"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := &keziov1alpha2.PostHook{Spec: keziov1alpha2.PostHookSpec{Steps: tt.steps}}
			err := CheckOSFamilyCompatible(hook, tt.image)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := &keziov1alpha2.PostHook{
		Spec: keziov1alpha2.PostHookSpec{
			Steps: []keziov1alpha2.PostHookStep{validBuiltinStep()},
		},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	invalid := &keziov1alpha2.PostHook{
		Spec: keziov1alpha2.PostHookSpec{
			Steps: []keziov1alpha2.PostHookStep{{}},
		},
	}
	if err := Validate(invalid); err == nil {
		t.Fatal("expected an error for a step with no kind set")
	}
}

// TestValidateAcceptsUnreferencedParam pins the unreferenced-param policy
// PostHookParam's doc comment states: a declared param no step's content
// references is accepted, not rejected - it may back a builtin step's
// Params value, or simply document an input a future step or attaching
// resource can rely on.
func TestValidateAcceptsUnreferencedParam(t *testing.T) {
	ph := &keziov1alpha2.PostHook{
		Spec: keziov1alpha2.PostHookSpec{
			Params: []keziov1alpha2.PostHookParam{{Name: "unused"}},
			Steps:  []keziov1alpha2.PostHookStep{validBuiltinStep()},
		},
	}
	if err := Validate(ph); err != nil {
		t.Fatalf("unexpected error for a declared param no step references: %v", err)
	}
}
