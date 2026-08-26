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

package kezioctl

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd_PrintsVersion(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v1.2.3"

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(version) error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "v1.2.3" {
		t.Errorf("version output = %q, want %q", got, "v1.2.3")
	}
}
