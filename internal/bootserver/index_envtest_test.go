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

package bootserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// TestSetupFieldIndexer_EnvtestLookup exercises SetupFieldIndexer against
// a real API server (envtest) to catch a wiring mistake between
// SetupFieldIndexer and Server that a fake client (which accepts any
// index name/func pair) would not.
func TestSetupFieldIndexer_EnvtestLookup(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" && firstEnvtestBinaryDir() == "" {
		t.Skip("no envtest binaries available (run `make setup-envtest`, or set KUBEBUILDER_ASSETS)")
	}

	testScheme := scheme.Scheme
	if err := keziov1alpha1.AddToScheme(testScheme); err != nil {
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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := SetupFieldIndexer(ctx, mgr); err != nil {
		t.Fatalf("SetupFieldIndexer: %v", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("mgr.Start: %v", err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatalf("cache did not sync")
	}

	rawClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	machine := newTestMachine(keziov1alpha1.MachineStateInspecting)
	machine.Namespace = "default"
	wantState := machine.Status.State
	if err := rawClient.Create(ctx, machine); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The Machine CRD has a status subresource: Create ignores whatever
	// status the object carried and leaves it zero-valued. A separate
	// status update is required to set status.state, the same as any
	// real client (including the Machine reconciler itself) has to do.
	machine.Status.State = wantState
	if err := rawClient.Status().Update(ctx, machine); err != nil {
		t.Fatalf("Status().Update: %v", err)
	}

	s := New(mgr.GetClient(), Config{ServerURL: "http://boot.example.test:8090"})

	waitForServe(t, s, "aa:bb:cc:dd:ee:01")
}

// waitForServe polls the grub.cfg handler until the cache observes the
// Create above and it resolves to a net-boot config.
func waitForServe(t *testing.T, s *Server, mac string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+mac, nil))
		if rec.Body.String() != bootLocalConfig {
			if !containsAll(rec.Body.String(), "kezio.token=") {
				t.Fatalf("resolved machine but response carries no token: %q", rec.Body.String())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine never resolved via the field-indexed lookup before the deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// firstEnvtestBinaryDir mirrors internal/controller/suite_test.go's
// getFirstFoundEnvTestBinaryDir: it locates the envtest binaries
// `make setup-envtest` downloads, for runs (for example from an IDE)
// that do not go through the Makefile's KUBEBUILDER_ASSETS export.
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
