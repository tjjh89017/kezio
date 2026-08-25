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

package posthookdefaults

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// TestEnsurer_EnvtestLifecycle exercises Ensurer.Start against a real API
// server (envtest): the first run creates the default PostHook, a second
// run is idempotent, and a user edit to spec.steps is overwritten by a
// third run - the whole point of applying under a forced field manager.
func TestEnsurer_EnvtestLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" && firstEnvtestBinaryDir() == "" {
		t.Skip("no envtest binaries available (run `make setup-envtest`, or set KUBEBUILDER_ASSETS)")
	}

	testScheme := scheme.Scheme
	if err := keziov1alpha3.AddToScheme(testScheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	rawClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	ctx := context.Background()
	const namespace = "default"
	e := &Ensurer{Client: rawClient}

	if err := e.ensure(ctx, namespace); err != nil {
		t.Fatalf("ensure() first run: %v", err)
	}

	var got keziov1alpha3.PostHook
	key := ctrlclient.ObjectKey{Name: DefaultFinalizeHookName, Namespace: namespace}
	if err := rawClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get after first ensure(): %v", err)
	}
	assertShippedSteps(t, got.Spec)

	if err := e.ensure(ctx, namespace); err != nil {
		t.Fatalf("ensure() second run (idempotency): %v", err)
	}
	if err := rawClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get after second ensure(): %v", err)
	}
	assertShippedSteps(t, got.Spec)

	// A user edits the spec directly.
	got.Spec.Steps = []keziov1alpha3.PostHookStep{
		{OSFamily: keziov1alpha3.OSFamilyLinux, Builtin: &keziov1alpha3.PostHookBuiltinStep{Name: keziov1alpha3.BuiltinStepGrowLastPartition}},
	}
	if err := rawClient.Update(ctx, &got); err != nil {
		t.Fatalf("user Update: %v", err)
	}

	if err := e.ensure(ctx, namespace); err != nil {
		t.Fatalf("ensure() third run (restore): %v", err)
	}
	if err := rawClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get after restoring ensure(): %v", err)
	}
	assertShippedSteps(t, got.Spec)
}

func assertShippedSteps(t *testing.T, got keziov1alpha3.PostHookSpec) {
	t.Helper()
	want := Spec()
	if len(got.Steps) != len(want.Steps) {
		t.Fatalf("spec.steps = %+v, want %+v", got.Steps, want.Steps)
	}
	for i := range want.Steps {
		if got.Steps[i].Builtin == nil || want.Steps[i].Builtin == nil {
			t.Fatalf("step[%d]: got %+v, want %+v", i, got.Steps[i], want.Steps[i])
		}
		if got.Steps[i].Builtin.Name != want.Steps[i].Builtin.Name || got.Steps[i].OSFamily != want.Steps[i].OSFamily {
			t.Errorf("step[%d] = %+v, want %+v", i, got.Steps[i], want.Steps[i])
		}
	}
}

// firstEnvtestBinaryDir mirrors internal/controller/suite_test.go's
// getFirstFoundEnvTestBinaryDir (and internal/bootserver's copy of it): it
// locates the envtest binaries `make setup-envtest` downloads, for runs
// (for example from an IDE) that do not go through the Makefile's
// KUBEBUILDER_ASSETS export.
func firstEnvtestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
