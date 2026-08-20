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

func TestNewRootCmd_HasImageCommandTree(t *testing.T) {
	root := NewRootCmd()

	imageCmd, _, err := root.Find([]string{"image"})
	if err != nil || imageCmd.Use != "image" {
		t.Fatalf("Find(image) = (%v, %v), want the image command", imageCmd, err)
	}

	for _, use := range []string{"upload", "list", "delete"} {
		if _, _, err := root.Find([]string{"image", use}); err != nil {
			t.Errorf("Find(image %s) error = %v", use, err)
		}
	}

	// Machine verbs are a later addition; the root command must not
	// advertise a command tree it does not implement.
	if _, _, err := root.Find([]string{"machine"}); err == nil {
		t.Error("expected no machine command yet")
	}
}

func TestRootCmd_Help(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(--help) error = %v", err)
	}
	if !strings.Contains(out.String(), "kezioctl") {
		t.Errorf("help output = %q, want it to mention kezioctl", out.String())
	}
}

func TestImageUploadCmd_RequiresNameAndLayout(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"image", "upload", "some-file", "--server", "http://x", "--token", "t"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error when --name and --layout are missing")
	}
}

func TestImageUploadCmd_RequiresExactlyOneFileArg(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"image", "upload"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error when the file argument is missing")
	}
}

func TestGlobalFlags_ResolveNamespace(t *testing.T) {
	f := &globalFlags{}
	if got := f.resolveNamespace("kube-ns"); got != "kube-ns" {
		t.Errorf("resolveNamespace() = %q, want kube-ns (kubeconfig fallback)", got)
	}

	f.namespace = "explicit-ns"
	if got := f.resolveNamespace("kube-ns"); got != "explicit-ns" {
		t.Errorf("resolveNamespace() = %q, want explicit-ns (flag wins)", got)
	}
}
