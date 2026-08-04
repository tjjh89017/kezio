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

package utils

import "testing"

func TestParseKubectlTopLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantPod string
		wantCPU int64
		wantMem int64
		wantErr bool
	}{
		{
			name:    "typical millicore CPU and mebibyte memory",
			line:    "kezio-ezio-seeder-6f9d8c7b9-abcde 105m 130Mi",
			wantPod: "kezio-ezio-seeder-6f9d8c7b9-abcde",
			wantCPU: 105,
			wantMem: 130 * 1024 * 1024,
		},
		{
			name:    "whole-core CPU and gibibyte memory",
			line:    "kezio-ezio-seeder-abc 2 1Gi",
			wantPod: "kezio-ezio-seeder-abc",
			wantCPU: 2000,
			wantMem: 1024 * 1024 * 1024,
		},
		{
			name:    "tolerates extra surrounding whitespace",
			line:    "  kezio-ezio-seeder-abc   50m   64Mi  ",
			wantPod: "kezio-ezio-seeder-abc",
			wantCPU: 50,
			wantMem: 64 * 1024 * 1024,
		},
		{
			name:    "too few fields is an error",
			line:    "kezio-ezio-seeder-abc 50m",
			wantErr: true,
		},
		{
			name:    "too many fields is an error",
			line:    "kezio-ezio-seeder-abc 50m 64Mi extra",
			wantErr: true,
		},
		{
			name:    "unparseable cpu quantity is an error",
			line:    "kezio-ezio-seeder-abc notacpu 64Mi",
			wantErr: true,
		},
		{
			name:    "unparseable memory quantity is an error",
			line:    "kezio-ezio-seeder-abc 50m notamemory",
			wantErr: true,
		},
		{
			name:    "empty line is an error",
			line:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, cpu, mem, err := ParseKubectlTopLine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseKubectlTopLine(%q) expected an error, got none", tt.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKubectlTopLine(%q) unexpected error: %v", tt.line, err)
			}
			if pod != tt.wantPod || cpu != tt.wantCPU || mem != tt.wantMem {
				t.Errorf("ParseKubectlTopLine(%q) = (%q, %d, %d), want (%q, %d, %d)",
					tt.line, pod, cpu, mem, tt.wantPod, tt.wantCPU, tt.wantMem)
			}
		})
	}
}
