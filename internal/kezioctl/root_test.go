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
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
}

func TestNewRootCmd_HasMachineDeployAndStatusCommands(t *testing.T) {
	root := NewRootCmd()

	for _, use := range [][]string{
		{"machine", "enroll"},
		{"machine", "set-disk"},
		{"deploy"},
		{"status"},
	} {
		if _, _, err := root.Find(use); err != nil {
			t.Errorf("Find(%v) error = %v", use, err)
		}
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

func TestNewRootCmd_ExecuteContextPropagatesCanceledContext(t *testing.T) {
	// Execute() derives its context from signal.NotifyContext and runs the
	// tree via ExecuteContext, so a canceled context must reach a
	// subcommand's RunE through cmd.Context() - that is what lets
	// ImageDelete's --wait cancellation path (see image_delete_test.go)
	// actually be reachable from a Ctrl-C during interactive use.
	root := NewRootCmd()

	var probedCtxWasCanceled bool
	root.AddCommand(&cobra.Command{
		Use: "ctx-probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			probedCtxWasCanceled = cmd.Context().Err() == context.Canceled
			return nil
		},
	})
	root.SetArgs([]string{"ctx-probe"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !probedCtxWasCanceled {
		t.Error("subcommand's cmd.Context() was not the canceled context passed to ExecuteContext")
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
