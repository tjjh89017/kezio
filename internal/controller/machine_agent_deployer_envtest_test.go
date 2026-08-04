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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
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

// agentWalkEnv bundles what every step of the walk below needs: a real
// envtest API server and manager, the reconciler under test wired to the
// real AgentFactory, a raw (uncached) client for seeding state the
// reconciler itself would never write directly, and an agentserver.Server
// standing in for the real HTTP endpoints a kezio-agent talks to.
type agentWalkEnv struct {
	ctx       context.Context
	r         *MachineReconciler
	rawClient ctrlclient.Client
	mgrClient ctrlclient.Client
	agentSrv  *agentserver.Server
}

// setupAgentWalkEnv starts its own envtest.Environment and manager
// instead of reusing suite_test.go's Ginkgo-managed globals (cfg,
// testEnv): those are only initialized inside RunSpecs' BeforeSuite, and
// a plain *testing.T test in the same package has no defined ordering
// against that - see internal/bootserver/index_envtest_test.go for the
// same pattern and the same reasoning. It registers t.Cleanup for
// everything it starts.
func setupAgentWalkEnv(t *testing.T) agentWalkEnv {
	t.Helper()
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

	r := &MachineReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		DeployerFactory: deployer.NewAgentFactory(mgr.GetClient()).New,
	}

	return agentWalkEnv{
		ctx:       ctx,
		r:         r,
		rawClient: rawClient,
		mgrClient: mgr.GetClient(),
		agentSrv:  agentserver.New(mgr.GetClient(), agentserver.Config{}),
	}
}

// TestMachineReconciler_AgentFactory_EnvtestWalk exercises the real,
// registration-driven Deployer (deployer.AgentFactory) end to end against
// a real API server (via envtest), covering both directions Provision can
// resolve: a Machine that walks a simulated agent's whole-plan progress
// reports through to the terminal success step reaches Provisioned with
// status.provisioning.image set and its target disk unchanged from what
// diskmatch resolved before Provision ever ran; a Machine whose simulated
// agent instead reports the terminal failure step lands in Error with a
// non-empty errorMessage, rather than Provision polling forever.
func TestMachineReconciler_AgentFactory_EnvtestWalk(t *testing.T) {
	env := setupAgentWalkEnv(t)

	const name = "agent-walk-machine"
	key, sessionToken := registerAndReachAvailable(t, env, name)
	resolvedDisk := requestDeploymentAndResolveDisk(t, env, key)
	walkProvisionToSuccess(t, env, key, name, sessionToken, resolvedDisk)

	const failName = "agent-walk-machine-failure"
	failKey, failSessionToken := registerAndReachAvailable(t, env, failName)
	requestDeploymentAndResolveDisk(t, env, failKey)
	walkProvisionToFailure(t, env, failKey, failName, failSessionToken)
}

// registerAndReachAvailable creates a fresh Machine named name, drives it
// to Inspecting, confirms it stalls there (Inspect polling, no fabricated
// hardware) until a simulated kezio-agent registers through
// internal/agentserver's real handler, then drives it on to Available and
// confirms the reported inventory landed on status.hardware. It returns
// the Machine's key and the session token the registration minted, which
// every later poll/progress call for this Machine must present.
func registerAndReachAvailable(t *testing.T, env agentWalkEnv, name string) (types.NamespacedName, string) {
	t.Helper()
	ctx := env.ctx
	key := types.NamespacedName{Name: name, Namespace: "default"}

	// newTestMachineSpec's default BMC address points at a real (but
	// unreachable in this test) redfish endpoint - fine for the Ginkgo
	// suite's FakeFactory-backed tests, which never resolve it, but this
	// walk drives the real AgentFactory, whose Register now actually
	// connects to the configured BMC (see agentDeployer.Register). Point
	// it at controllerTestBMCScheme instead (a fast, in-memory fake - see
	// machine_bmc_testdriver_test.go) and seed the credentials Secret it
	// names, so Register's BMC steps succeed the same way this walk's
	// pre-BMC-wiring behavior did.
	spec := newTestMachineSpec(name)
	bmcSecretName := name + "-bmc"
	spec.BMC.Address = controllerTestBMCScheme + "://" + name
	spec.BMC.CredentialsSecretRef.Name = bmcSecretName
	bmcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: bmcSecretName, Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("test-password"),
		},
	}
	if err := env.rawClient.Create(ctx, bmcSecret); err != nil {
		t.Fatalf("creating BMC credentials secret: %v", err)
	}

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
	if err := env.rawClient.Create(ctx, machine); err != nil {
		t.Fatalf("Create: %v", err)
	}

	byReconcilingUntil(t, ctx, env.r, key, "Inspecting", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateInspecting
	})

	// Reconciling further while no agent has registered must keep the
	// Machine stalled in Inspecting: Inspect polls (RequeueAfter) rather
	// than completing.
	if _, err := env.r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile while awaiting agent: %v", err)
	}
	stalled := getMachine(t, ctx, env.mgrClient, key)
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
	// The reconciler (driven synchronously above, and again inside
	// byReconcilingUntil) writes status concurrently with this seeding
	// write, and the manager's cache can also lag the API server's true
	// resourceVersion; RetryOnConflict re-Gets a fresh copy (bypassing the
	// cache via rawClient) and reapplies the mutation whenever a 409
	// lands, instead of failing the test on the first collision.
	token := "test-agent-registration-token-" + name
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		m := &keziov1alpha1.Machine{}
		if err := env.rawClient.Get(ctx, key, m); err != nil {
			return err
		}
		m.Status.NetBoot = &keziov1alpha1.MachineNetBootStatus{
			TokenHash: bootserver.HashToken(token),
			ExpiresAt: metav1.NewTime(time.Now().Add(30 * time.Minute)),
		}
		return env.rawClient.Status().Update(ctx, m)
	}); err != nil {
		t.Fatalf("seeding a live token: %v", err)
	}

	registerBody, err := json.Marshal(agentapi.RegisterRequest{Hardware: sampleInventory()})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	sessionToken := waitForRegistration(t, env.agentSrv, token, registerBody)

	byReconcilingUntil(t, ctx, env.r, key, "Available", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateAvailable
	})
	available := getMachine(t, ctx, env.mgrClient, key)
	if available.Status.Hardware == nil || len(available.Status.Hardware.Disks) != 1 {
		t.Fatalf("hardware inventory was not carried into status.hardware: %+v", available.Status.Hardware)
	}
	if available.Status.Hardware.Disks[0].SerialNumber != "AGENT-SERIAL" {
		t.Fatalf("unexpected disk serial: %q", available.Status.Hardware.Disks[0].SerialNumber)
	}

	return key, sessionToken
}

// deployedImageName is the (deliberately nonexistent) Image name every
// walk below requests: Provision never reads the Image CR itself - only
// agentserver's GET .../next handler needs a real, Ready Image, and
// nothing in this test ever exercises that endpoint - so a name that
// resolves to nothing is enough to drive Available -> Provisioning.
const deployedImageName = "does-not-need-to-exist-for-this-assertion"

// requestDeploymentAndResolveDisk sets spec.imageRef, driving
// Available -> Provisioning, and reconciles until reconcileProvisioning
// has resolved the OS image's target disk against the single reported
// disk (no targetDisk hints on this spec, one disk in inventory -
// diskmatch.Match's unambiguous default) and recorded it in
// status.provisioning - before Provision is ever called, so Provision
// must echo this same disk back on success rather than re-derive it. It
// returns the resolved disk device path.
func requestDeploymentAndResolveDisk(t *testing.T, env agentWalkEnv, key types.NamespacedName) string {
	t.Helper()
	ctx := env.ctx

	// Same cached-read/direct-write skew as registerAndReachAvailable's
	// token seeding: retry against a fresh Get instead of failing
	// outright on a conflicting concurrent reconciler write.
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		m := &keziov1alpha1.Machine{}
		if err := env.rawClient.Get(ctx, key, m); err != nil {
			return err
		}
		m.Spec.ImageRef = &keziov1alpha1.NameRef{Name: deployedImageName}
		return env.rawClient.Update(ctx, m)
	}); err != nil {
		t.Fatalf("requesting a deployment: %v", err)
	}

	// Provision has nothing to drive itself (see agentDeployer.Provision's
	// doc comment): it only polls MachineConditionProvisioningProgress, so
	// nothing advances past Provisioning until a progress report lands -
	// what the walk*ProvisionTo* helpers do next.
	byReconcilingUntil(t, ctx, env.r, key, "Provisioning", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateProvisioning &&
			m.Status.Provisioning != nil && m.Status.Provisioning.Image != nil &&
			m.Status.Provisioning.Image.TargetDisk != ""
	})
	resolvedDisk := getMachine(t, ctx, env.mgrClient, key).Status.Provisioning.Image.TargetDisk
	if resolvedDisk != "/dev/nvme0n1" {
		t.Fatalf("resolved target disk = %q, want /dev/nvme0n1 (the sole reported disk)", resolvedDisk)
	}

	// A reconcile with no progress report yet must keep the Machine
	// stalled in Provisioning: Provision polls (RequeueAfter) rather than
	// completing or erroring on an agent that has not reported anything.
	if _, err := env.r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile while awaiting deploy progress: %v", err)
	}
	stillProvisioning := getMachine(t, ctx, env.mgrClient, key)
	if stillProvisioning.Status.State != keziov1alpha1.MachineStateProvisioning {
		t.Fatalf("state = %q, want still Provisioning while awaiting agent progress", stillProvisioning.Status.State)
	}

	return resolvedDisk
}

// walkProvisionToSuccess simulates a kezio-agent's whole-plan step
// machine walking every intermediate step - each of which must keep
// Provision polling and the Machine in Provisioning - before the
// terminal DeployStepRebootingToDisk, which must complete the deployment:
// Provisioned, with status.provisioning.image set and its target disk
// unchanged from resolvedDisk (the same consistency
// requestDeploymentAndResolveDisk's doc comment describes).
func walkProvisionToSuccess(t *testing.T, env agentWalkEnv, key types.NamespacedName, name, sessionToken, resolvedDisk string) {
	t.Helper()
	ctx := env.ctx

	for _, step := range []string{
		agentapi.DeployStepPartitioning,
		agentapi.DeployStepWritingContent,
		agentapi.DeployStepRunningPostHook,
		agentapi.DeployStepFinalizing,
	} {
		postProgress(t, env.agentSrv, name, sessionToken, agentapi.ProgressRequest{Step: step})
		if _, err := env.r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile after step %q: %v", step, err)
		}
		mid := getMachine(t, ctx, env.mgrClient, key)
		if mid.Status.State != keziov1alpha1.MachineStateProvisioning {
			t.Fatalf("after step %q: state = %q, want still Provisioning", step, mid.Status.State)
		}
	}

	postProgress(t, env.agentSrv, name, sessionToken, agentapi.ProgressRequest{Step: agentapi.DeployStepRebootingToDisk})
	byReconcilingUntil(t, ctx, env.r, key, "Provisioned", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateProvisioned
	})
	provisioned := getMachine(t, ctx, env.mgrClient, key)
	if provisioned.Status.Provisioning == nil || provisioned.Status.Provisioning.Image == nil {
		t.Fatalf("status.provisioning.image was not set once Provisioned: %+v", provisioned.Status.Provisioning)
	}
	if got := provisioned.Status.Provisioning.Image.TargetDisk; got != resolvedDisk {
		t.Fatalf("Provisioned status.provisioning.image.targetDisk = %q, want it unchanged from the disk diskmatch resolved (%q) - Provision must echo it back, not re-derive it", got, resolvedDisk)
	}
	if provisioned.Status.Provisioning.Image.ImageRef.Name != deployedImageName {
		t.Fatalf("unexpected deployed image ref: %+v", provisioned.Status.Provisioning.Image.ImageRef)
	}
}

// walkProvisionToFailure simulates a kezio-agent reporting the terminal
// failure step and confirms the Machine lands in Error with a non-empty
// errorMessage, rather than Provision polling forever behind a deploy
// that will never report DeployStepRebootingToDisk.
func walkProvisionToFailure(t *testing.T, env agentWalkEnv, key types.NamespacedName, name, sessionToken string) {
	t.Helper()
	ctx := env.ctx

	postProgress(t, env.agentSrv, name, sessionToken, agentapi.ProgressRequest{
		Step:        agentapi.DeployStepFailed,
		StepMessage: "injected: sfdisk exited 1",
	})
	byReconcilingUntil(t, ctx, env.r, key, "Error", func(m *keziov1alpha1.Machine) bool {
		return m.Status.State == keziov1alpha1.MachineStateError
	})
	errored := getMachine(t, ctx, env.mgrClient, key)
	if errored.Status.ErrorMessage == "" {
		t.Fatalf("expected a non-empty errorMessage once the agent reports a failed deploy")
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
// to status.netBoot. It returns the minted session token
// (RegisterResponse.SessionToken) subsequent GET .../next and
// POST .../progress calls must present.
func waitForRegistration(t *testing.T, agentSrv *agentserver.Server, token string, body []byte) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodPost, agentapi.RegisterPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		agentSrv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			var resp agentapi.RegisterResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding RegisterResponse: %v", err)
			}
			return resp.SessionToken
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration never succeeded before the deadline; last status %d body %q", rec.Code, rec.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// postProgress POSTs req through agentSrv's real handleProgress handler,
// the same endpoint internal/agent/deploy.Executor reports to in
// production, standing in for a real kezio-agent executing a deploy
// plan. sessionToken is the value waitForRegistration returned for the
// same machine. It retries on a 401 the same way waitForRegistration
// does: agentSrv reads through the manager's cache
// (agentserver.Server.Client), which can briefly lag the direct write
// that just persisted status.agentSession (or, for the very first call
// after registration, the registration response itself), so a session
// that is genuinely valid at the API server can still 401 for a moment.
func postProgress(t *testing.T, agentSrv *agentserver.Server, machineName, sessionToken string, req agentapi.ProgressRequest) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal progress request: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		httpReq := httptest.NewRequest(http.MethodPost, agentapi.NextPathPrefix+machineName+agentapi.ProgressPathSuffix, bytes.NewReader(body))
		httpReq.Header.Set("Authorization", "Bearer "+sessionToken)
		rec := httptest.NewRecorder()
		agentSrv.Handler().ServeHTTP(rec, httpReq)
		if rec.Code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("posting progress (step %q) never succeeded before the deadline; last status %d body %q", req.Step, rec.Code, rec.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
