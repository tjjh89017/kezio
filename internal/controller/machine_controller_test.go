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

package controller

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// TestMachineControllerGoSourceDoesNotImportBMC guards the fast lane's stub
// boundary: the Machine reconciler must never dial a BMC in this stage, and
// this parses machine_controller.go's own import list rather than trusting
// a comment to stay accurate.
func TestMachineControllerGoSourceDoesNotImportBMC(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report this test file's path")
	}
	sourcePath := filepath.Join(filepath.Dir(thisFile), "machine_controller.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, sourcePath, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", sourcePath, err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, "/internal/bmc") {
			t.Fatalf("machine_controller.go imports %q; this stage's reconciler must never dial a BMC", path)
		}
	}
}

func TestRestartOnFailure(t *testing.T) {
	cases := []struct {
		name   string
		status keziov1alpha2.MachineStatus
		want   bool
	}{
		{"zero value status", keziov1alpha2.MachineStatus{}, false},
		{"OK with stale Restart errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusOK, ErrorType: keziov1alpha2.MachineErrorTypeRestart}, false},
		{"error with Restart errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorTypeRestart}, true},
		{"error with Transient errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorTypeTransient}, false},
		{"error with unrecognized errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorType("bogus")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{Status: tc.status}
			if got := restartOnFailure(machine); got != tc.want {
				t.Errorf("restartOnFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasUnknownErrorType(t *testing.T) {
	cases := []struct {
		name   string
		status keziov1alpha2.MachineStatus
		want   bool
	}{
		{"zero value status", keziov1alpha2.MachineStatus{}, false},
		{"OK with bogus errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusOK, ErrorType: keziov1alpha2.MachineErrorType("bogus")}, false},
		{"error with empty errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError}, false},
		{"error with Transient errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorTypeTransient}, false},
		{"error with Restart errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorTypeRestart}, false},
		{"error with unrecognized errorType", keziov1alpha2.MachineStatus{OperationalStatus: keziov1alpha2.MachineOperationalStatusError, ErrorType: keziov1alpha2.MachineErrorType("bogus")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{Status: tc.status}
			if got := hasUnknownErrorType(machine); got != tc.want {
				t.Errorf("hasUnknownErrorType() = %v, want %v", got, tc.want)
			}
		})
	}
}

var _ = Describe("Machine Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		machine := &keziov1alpha2.Machine{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Machine")
			err := k8sClient.Get(ctx, typeNamespacedName, machine)
			if err != nil && errors.IsNotFound(err) {
				resource := &keziov1alpha2.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: keziov1alpha2.MachineSpec{
						BMC: keziov1alpha2.MachineBMC{
							Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
							CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
						},
						BootMACAddress: "aa:bb:cc:dd:ee:01",
						SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &keziov1alpha2.Machine{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Machine")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &MachineReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Deployer: &deployer.FakeDeployer{Client: k8sClient},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When walking the fake-deployer state machine", func() {
		ctx := context.Background()
		var machineName string

		reconcileUntilStable := func(reconciler *MachineReconciler, name types.NamespacedName) {
			GinkgoHelper()
			for range 50 {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if result.RequeueAfter == 0 {
					var m keziov1alpha2.Machine
					Expect(k8sClient.Get(ctx, name, &m)).To(Succeed())
					if m.Status.State == keziov1alpha2.MachineStateProvisioned {
						return
					}
				}
			}
		}

		BeforeEach(func() {
			machineName = fmt.Sprintf("walk-%d", GinkgoRandomSeed())
			imageRef := keziov1alpha2.NameRef{Name: "test-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: "default",
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:02",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &keziov1alpha2.Machine{}
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			if err := k8sClient.Get(ctx, name, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("walks Enrolling to Provisioned and produces a DeployRun with per-phase timings", func() {
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			reconciler := &MachineReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Deployer: &deployer.FakeDeployer{Client: k8sClient},
			}

			By("reconciling until the finalizer is added")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			By("walking the state machine to completion")
			reconcileUntilStable(reconciler, name)

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(machine.Status.LastSuccessfulRunRef).NotTo(BeNil())
			Expect(machine.Status.CurrentRunRef).NotTo(BeNil())

			var hw keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &hw)).To(Succeed())
			Expect(hw.Spec.Disks).NotTo(BeEmpty())

			var run keziov1alpha2.DeployRun
			runName := types.NamespacedName{Name: machine.Status.LastSuccessfulRunRef.Name, Namespace: "default"}
			Expect(k8sClient.Get(ctx, runName, &run)).To(Succeed())
			Expect(run.Status.Phase).To(Equal(keziov1alpha2.DeployRunPhaseSucceeded))
			Expect(run.Status.PhaseTimings).NotTo(BeEmpty())
			for _, timing := range run.Status.PhaseTimings {
				Expect(timing.StartedAt).NotTo(BeZero())
			}
			Expect(run.Spec.ImageRef).NotTo(BeNil())
			Expect(run.Spec.ImageRef.Name).To(Equal("test-image"))

			By("changing spec.imageRef and confirming a second DeployRun is created")
			newImageRef := keziov1alpha2.NameRef{Name: "other-image"}
			machine.Spec.ImageRef = &newImageRef
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			reconcileUntilStable(reconciler, name)

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.LastSuccessfulRunRef.Name).NotTo(Equal(run.Name))

			var secondRun keziov1alpha2.DeployRun
			secondRunName := types.NamespacedName{Name: machine.Status.LastSuccessfulRunRef.Name, Namespace: "default"}
			Expect(k8sClient.Get(ctx, secondRunName, &secondRun)).To(Succeed())
			Expect(secondRun.Spec.ImageRef.Name).To(Equal("other-image"))
		})
	})

	Context("When spec references name kinds that do not exist yet", func() {
		ctx := context.Background()

		It("still walks Enrolling to Provisioned, proving subnetRef/postHookRefs/imageRef are never resolved", func() {
			machineName := fmt.Sprintf("unresolved-refs-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "no-such-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:04",
					SubnetRef:      keziov1alpha2.NameRef{Name: "no-such-subnet"},
					PostHookRefs:   []keziov1alpha2.NameRef{{Name: "no-such-hook"}},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			reconciler := &MachineReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Deployer: &deployer.FakeDeployer{Client: k8sClient},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			for range 50 {
				result, rerr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(rerr).NotTo(HaveOccurred())
				if result.RequeueAfter == 0 {
					var m keziov1alpha2.Machine
					Expect(k8sClient.Get(ctx, name, &m)).To(Succeed())
					if m.Status.State == keziov1alpha2.MachineStateProvisioned {
						break
					}
				}
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
		})
	})

	Context("When a Deployer step fails", func() {
		ctx := context.Background()

		It("records errorType/errorMessage/errorCount without changing state", func() {
			machineName := fmt.Sprintf("fail-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:03",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			failingDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: failingDeployer}

			var result reconcile.Result
			var err error
			for i := 0; i < 10 && result.RequeueAfter != failedRequeueInterval; i++ {
				result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(result.RequeueAfter).To(Equal(failedRequeueInterval))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(machine.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorTypeTransient))
			Expect(machine.Status.ErrorMessage).To(Equal("boom"))
			Expect(machine.Status.ErrorCount).To(Equal(int32(1)))
		})

		It("passes restartOnFailure=true into the next Inspect call after a MachineErrorTypeRestart failure", func() {
			machineName := fmt.Sprintf("restart-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:05",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var seenRestartOnFailure []bool
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(_ context.Context, _ *keziov1alpha2.Machine, restartOnFailure bool) (deployer.Result, error) {
					seenRestartOnFailure = append(seenRestartOnFailure, restartOnFailure)
					if len(seenRestartOnFailure) == 1 {
						return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeRestart, ErrorMessage: "boom"}, nil
					}
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("reconciling through the finalizer add, Enrolling, and both Inspect calls")
			for i := 0; i < 10 && len(seenRestartOnFailure) < 2; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(seenRestartOnFailure).To(HaveLen(2))
			Expect(seenRestartOnFailure[0]).To(BeFalse(), "the first Inspect call has no prior error, so restartOnFailure must be false")
			Expect(seenRestartOnFailure[1]).To(BeTrue(), "the retry after a MachineErrorTypeRestart failure must ask the deployer to restart")

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
		})

		It("increases errorCount monotonically across N consecutive failures without ever changing state", func() {
			machineName := fmt.Sprintf("monotonic-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:06",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var failCount int
			failingDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					failCount++
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: failingDeployer}

			By("driving through the finalizer add and the Enrolling to Inspecting transition")
			for i := 0; i < 10 && failCount == 0; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			const wantFailures = 5
			for failCount < wantFailures {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())

				var machine keziov1alpha2.Machine
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
				Expect(machine.Status.ErrorCount).To(Equal(int32(failCount)), "errorCount must equal the number of consecutive failures seen so far")
			}
		})

		It("does not reset errorCount or operationalStatus on Continuing/Busy outcomes within the same state", func() {
			machineName := fmt.Sprintf("no-reset-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:07",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			// The sequence deliberately interleaves Continuing/Busy between two
			// Failed outcomes: neither must touch errorCount/operationalStatus,
			// since only a state transition may clear them.
			outcomes := []deployer.Result{
				{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "first failure"},
				{Outcome: deployer.Continuing},
				{Outcome: deployer.Busy, RequeueAfter: time.Millisecond},
				{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "second failure"},
			}
			var idx int
			scriptedDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					result := outcomes[idx]
					idx++
					return result, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: scriptedDeployer}

			By("driving through the finalizer add, the Enrolling to Inspecting transition, and the first failure")
			for i := 0; i < 10 && idx == 0; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(mid.Status.ErrorCount).To(Equal(int32(1)))

			By("reconciling the remaining scripted outcomes: Continuing, Busy, then a second Failed")
			wantErrorCount := []int32{1, 1, 2}
			for _, want := range wantErrorCount {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())

				var machine keziov1alpha2.Machine
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
				Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
				Expect(machine.Status.ErrorCount).To(Equal(want))
			}
		})

		It("resets errorCount and operationalStatus to OK when the machine transitions out of the failed state", func() {
			machineName := fmt.Sprintf("reset-on-exit-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:08",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var calls int
			recoveringDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					if calls == 1 {
						return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
					}
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: recoveringDeployer}

			By("reconciling through the finalizer add, Enrolling, and the failing Inspect call")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(mid.Status.ErrorCount).To(Equal(int32(1)))

			By("reconciling the successful retry that exits Inspecting")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
		})

		It("re-enrolls a machine carrying an unrecognized errorType and walks it back to a good state", func() {
			machineName := fmt.Sprintf("corrupt-errortype-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:09",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var calls int
			recoveringDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					if calls == 1 {
						return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorType("bogus"), ErrorMessage: "corrupt"}, nil
					}
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: recoveringDeployer}

			By("reconciling through the finalizer add, Enrolling, and the Inspect call that records the unknown errorType")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(mid.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorType("bogus")))
			Expect(mid.Status.ErrorCount).To(Equal(int32(1)))

			By("reconciling once more: the unrecognized errorType sends the machine back to Enrolling")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var reenrolled keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &reenrolled)).To(Succeed())
			Expect(reenrolled.Status.State).To(Equal(keziov1alpha2.MachineStateEnrolling))
			Expect(reenrolled.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(reenrolled.Status.ErrorCount).To(Equal(int32(0)))

			By("walking the rest of the way back to a good state")
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
		})
	})
})
