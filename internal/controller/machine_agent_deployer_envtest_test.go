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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/agentserver"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// TestMachineReconciler_AgentFactory_EnvtestWalk exercises the real,
// registration-driven Deployer (deployer.AgentFactory) end to end
// against a real API server (via envtest): a Machine stalls in
// Inspecting until a simulated kezio-agent registration POST lands
// through internal/agentserver's actual handler, then the same
// reconciler picks the inventory back up and completes to Available.
//
// This test spins up its own envtest.Environment and manager instead of
// reusing suite_test.go's Ginkgo-managed globals (cfg, testEnv): those
// are only initialized inside RunSpecs' BeforeSuite, and a plain
// *testing.T test in the same package has no defined ordering against
// that - see internal/bootserver/index_envtest_test.go for the same
// pattern and the same reasoning.
func TestMachineReconciler_AgentFactory_EnvtestWalk(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" && getFirstFoundEnvTestBinaryDir() == "" {
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
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
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

	if err := agentserver.SetupFieldIndexer(ctx, mgr); err != nil {
		t.Fatalf("agentserver.SetupFieldIndexer: %v", err)
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

	const name = "agent-walk-machine"
	key := types.NamespacedName{Name: name, Namespace: "default"}
	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       newTestMachineSpec(name),
	}
	if err := rawClient.Create(ctx, machine); err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := &MachineReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		DeployerFactory: deployer.NewAgentFactory(mgr.GetClient()).New,
	}

	byReconcilingUntil(t, ctx, r, key, "Inspecting", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateInspecting
	})

	// Reconciling further while no agent has registered must keep the
	// Machine stalled in Inspecting: Inspect polls (RequeueAfter) rather
	// than completing.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile while awaiting agent: %v", err)
	}
	stalled := getMachine(t, ctx, mgr.GetClient(), key)
	if stalled.Status.State != keziov1alpha1.MachineStateInspecting {
		t.Fatalf("state = %q, want still Inspecting while awaiting the agent", stalled.Status.State)
	}
	if stalled.Status.Hardware != nil {
		t.Fatalf("hardware was populated before any agent registered: %+v", stalled.Status.Hardware)
	}

	// Simulate the live-boot agent: mint a token the way
	// internal/bootserver would (a real grub.cfg round trip is out of
	// scope for this test; the point here is the agentserver/deployer
	// integration, which starts from a Machine that already carries a
	// live token, the same as bootserver.Server.rotateToken leaves one),
	// then POST it through agentserver's real handler.
	const token = "test-agent-registration-token-0123456789abcdef"
	stalled.Status.NetBoot = &keziov1alpha1.MachineNetBootStatus{
		TokenHash: bootserver.HashToken(token),
		ExpiresAt: metav1.NewTime(time.Now().Add(30 * time.Minute)),
	}
	if err := mgr.GetClient().Status().Update(ctx, stalled); err != nil {
		t.Fatalf("seeding a live token: %v", err)
	}

	agentSrv := agentserver.New(mgr.GetClient(), agentserver.Config{})
	registerBody, err := json.Marshal(agentapi.RegisterRequest{Hardware: sampleInventory()})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	waitForRegistration(t, agentSrv, token, registerBody)

	byReconcilingUntil(t, ctx, r, key, "Available", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateAvailable
	})
	available := getMachine(t, ctx, mgr.GetClient(), key)
	if available.Status.Hardware == nil || len(available.Status.Hardware.Disks) != 1 {
		t.Fatalf("hardware inventory was not carried into status.hardware: %+v", available.Status.Hardware)
	}
	if available.Status.Hardware.Disks[0].SerialNumber != "AGENT-SERIAL" {
		t.Fatalf("unexpected disk serial: %q", available.Status.Hardware.Disks[0].SerialNumber)
	}

	// Provision/Deprovision are honestly not implemented yet by this
	// Deployer: driving to Provisioning must land in Error, not a
	// fabricated success.
	spec := available.DeepCopy()
	spec.Spec.ImageRef = &keziov1alpha1.NameRef{Name: "does-not-need-to-exist-for-this-assertion"}
	if err := mgr.GetClient().Update(ctx, spec); err != nil {
		t.Fatalf("requesting a deployment: %v", err)
	}
	byReconcilingUntil(t, ctx, r, key, "Error", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateError
	})
	errored := getMachine(t, ctx, mgr.GetClient(), key)
	if errored.Status.ErrorMessage == "" {
		t.Fatalf("expected a non-empty errorMessage once Provision runs against the agent deployer")
	}
}

func sampleInventory() keziov1alpha1.MachineHardwareStatus {
	return keziov1alpha1.MachineHardwareStatus{
		Disks: []keziov1alpha1.MachineHardwareDisk{
			{DeviceName: "/dev/nvme0n1", SerialNumber: "AGENT-SERIAL", SizeBytes: 512 << 30},
		},
		Nics: []keziov1alpha1.MachineHardwareNIC{
			{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:01"},
		},
		MemoryBytes: 16 << 30,
		CPUCount:    8,
	}
}

// byReconcilingUntil reconciles key up to a generous bound of steps
// until check reports true, failing the test if it never does. A Get
// that returns NotFound is tolerated and retried (not fatal): the
// manager's cache populates asynchronously from the raw client Create
// this test uses to seed the Machine (see the call site), so the very
// first steps may race the cache's initial sync.
func byReconcilingUntil(t *testing.T, ctx context.Context, r *MachineReconciler, key types.NamespacedName, wantLabel string, check func(*keziov1alpha1.Machine) bool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			if apierrors.IsConflict(err) {
				// The manager's cache can lag one write behind the API
				// server (for example, right after agentDeployer.Register's
				// own Status().Update); a conflict here just means this
				// reconcile read a stale resourceVersion, which the next
				// iteration's fresh Get resolves.
				time.Sleep(50 * time.Millisecond)
				continue
			}
			t.Fatalf("Reconcile: %v", err)
		}
		m := &keziov1alpha1.Machine{}
		if err := r.Get(ctx, key, m); err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if check(m) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never reached %s within the step bound", wantLabel)
}

func getMachine(t *testing.T, ctx context.Context, c ctrlclient.Client, key types.NamespacedName) *keziov1alpha1.Machine {
	t.Helper()
	m := &keziov1alpha1.Machine{}
	if err := c.Get(ctx, key, m); err != nil {
		t.Fatalf("Get: %v", err)
	}
	return m
}

// waitForRegistration POSTs the registration body through agentSrv's
// real handler, polling because the manager cache backing agentSrv's
// field index needs a moment to observe the token that was just written
// to status.netBoot.
func waitForRegistration(t *testing.T, agentSrv *agentserver.Server, token string, body []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodPost, agentapi.RegisterPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		agentSrv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration never succeeded before the deadline; last status %d body %q", rec.Code, rec.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
