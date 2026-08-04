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

import "testing"

func TestPostHookBuiltinStepEffectiveTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name string
		step PostHookBuiltinStep
		want int32
	}{
		{
			name: "zero timeout defaults to PostHookDefaultTimeoutSeconds",
			step: PostHookBuiltinStep{Name: BuiltinStepMkswap},
			want: PostHookDefaultTimeoutSeconds,
		},
		{
			name: "explicit timeout is preserved",
			step: PostHookBuiltinStep{Name: BuiltinStepMkswap, TimeoutSeconds: 30},
			want: 30,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.step.EffectiveTimeoutSeconds(); got != tt.want {
				t.Errorf("EffectiveTimeoutSeconds() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPostHookScriptSourceEffectiveTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name   string
		source PostHookScriptSource
		want   int32
	}{
		{
			name:   "zero timeout defaults to PostHookDefaultTimeoutSeconds",
			source: PostHookScriptSource{Script: "echo hi"},
			want:   PostHookDefaultTimeoutSeconds,
		},
		{
			name:   "explicit timeout is preserved",
			source: PostHookScriptSource{Script: "echo hi", TimeoutSeconds: 300},
			want:   300,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.EffectiveTimeoutSeconds(); got != tt.want {
				t.Errorf("EffectiveTimeoutSeconds() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPostHookScriptSourceSourceKind(t *testing.T) {
	tests := []struct {
		name   string
		source PostHookScriptSource
		want   string
	}{
		{
			name:   "inline script",
			source: PostHookScriptSource{Script: "echo hi"},
			want:   PostHookScriptSourceInline,
		},
		{
			name:   "configMap reference",
			source: PostHookScriptSource{ConfigMapRef: &ConfigMapKeyRef{Name: "notify-cmdb", Key: "run.sh"}},
			want:   PostHookScriptSourceConfigMapRef,
		},
		{
			name:   "secret reference",
			source: PostHookScriptSource{SecretRef: &SecretKeyRef{Name: "creds", Key: "token"}},
			want:   PostHookScriptSourceSecretRef,
		},
		{
			name:   "none set",
			source: PostHookScriptSource{},
			want:   PostHookScriptSourceUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.SourceKind(); got != tt.want {
				t.Errorf("SourceKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostHookStepType(t *testing.T) {
	tests := []struct {
		name string
		step PostHookStep
		want string
	}{
		{
			name: "builtin step",
			step: PostHookStep{Builtin: &PostHookBuiltinStep{Name: BuiltinStepMkswap}},
			want: PostHookStepTypeBuiltin,
		},
		{
			name: "script step",
			step: PostHookStep{Script: &PostHookScriptSource{Script: "echo hi"}},
			want: PostHookStepTypeScript,
		},
		{
			name: "chrootScript step",
			step: PostHookStep{ChrootScript: &PostHookScriptSource{Script: "update-initramfs -u -k all"}},
			want: PostHookStepTypeChrootScript,
		},
		{
			name: "none set",
			step: PostHookStep{},
			want: PostHookStepTypeUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.step.Type(); got != tt.want {
				t.Errorf("Type() = %q, want %q", got, tt.want)
			}
		})
	}
}
