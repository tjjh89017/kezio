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

package kezioctl

import "testing"

func TestDetectFormatFromFilename(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		wantFormat string
		wantOK     bool
	}{
		{"qcow2", "golden.qcow2", "qcow2", true},
		{"qcow2 uppercase extension", "golden.QCOW2", "qcow2", true},
		{"vmdk", "golden.vmdk", "vmdk", true},
		{"raw", "golden.raw", "raw", true},
		{"img alias for raw", "golden.img", "raw", true},
		{"path with directories", "/tmp/images/golden.vmdk", "vmdk", true},
		{"unknown extension", "golden.iso", "", false},
		{"no extension", "golden", "", false},
		{"stdin marker has no extension", "-", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DetectFormatFromFilename(tc.file)
			if ok != tc.wantOK || got != tc.wantFormat {
				t.Errorf("DetectFormatFromFilename(%q) = (%q, %v), want (%q, %v)",
					tc.file, got, ok, tc.wantFormat, tc.wantOK)
			}
		})
	}
}
