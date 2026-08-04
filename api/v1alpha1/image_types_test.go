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

func TestImageSpecEffectiveBootable(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name string
		spec ImageSpec
		want bool
	}{
		{
			name: "nil bootable defaults to true",
			spec: ImageSpec{},
			want: true,
		},
		{
			name: "explicit true stays true",
			spec: ImageSpec{Bootable: &trueVal},
			want: true,
		},
		{
			name: "explicit false stays false",
			spec: ImageSpec{Bootable: &falseVal},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.EffectiveBootable(); got != tt.want {
				t.Errorf("EffectiveBootable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestImageSpecEffectiveOSFamily(t *testing.T) {
	tests := []struct {
		name string
		spec ImageSpec
		want string
	}{
		{
			name: "empty osFamily defaults to Linux",
			spec: ImageSpec{},
			want: OSFamilyLinux,
		},
		{
			name: "explicit osFamily is preserved",
			spec: ImageSpec{OSFamily: OSFamilyWindows},
			want: OSFamilyWindows,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.EffectiveOSFamily(); got != tt.want {
				t.Errorf("EffectiveOSFamily() = %q, want %q", got, tt.want)
			}
		})
	}
}
