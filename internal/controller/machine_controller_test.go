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
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestShouldProvision(t *testing.T) {
	image1 := keziov1alpha2.NameRef{Name: "image-1"}
	image2 := keziov1alpha2.NameRef{Name: "image-2"}
	data1 := []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: "data-1"}}}
	data2 := []keziov1alpha2.MachineDataImage{
		{ImageRef: keziov1alpha2.NameRef{Name: "data-1"}},
		{ImageRef: keziov1alpha2.NameRef{Name: "data-2"}},
	}

	cases := []struct {
		name    string
		spec    keziov1alpha2.MachineSpec
		lastRun *keziov1alpha2.DeployRun
		want    bool
	}{
		{
			name:    "empty payload, no last run, never triggers",
			spec:    keziov1alpha2.MachineSpec{},
			lastRun: nil,
			want:    false,
		},
		{
			name:    "non-empty payload, no last run, triggers once",
			spec:    keziov1alpha2.MachineSpec{ImageRef: &image1},
			lastRun: nil,
			want:    true,
		},
		{
			name:    "dataImages-only payload, no last run, triggers",
			spec:    keziov1alpha2.MachineSpec{DataImages: data1},
			lastRun: nil,
			want:    true,
		},
		{
			name: "identical subset does not trigger",
			spec: keziov1alpha2.MachineSpec{ImageRef: &image1, DataImages: data1},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{
				ImageRef:   &image1,
				DataImages: data1,
			}},
			want: false,
		},
		{
			name: "imageRef change triggers",
			spec: keziov1alpha2.MachineSpec{ImageRef: &image2, DataImages: data1},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{
				ImageRef:   &image1,
				DataImages: data1,
			}},
			want: true,
		},
		{
			name: "dataImages addition triggers",
			spec: keziov1alpha2.MachineSpec{ImageRef: &image1, DataImages: data2},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{
				ImageRef:   &image1,
				DataImages: data1,
			}},
			want: true,
		},
		{
			name: "dataImages removal triggers",
			spec: keziov1alpha2.MachineSpec{ImageRef: &image1, DataImages: data1},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{
				ImageRef:   &image1,
				DataImages: data2,
			}},
			want: true,
		},
		{
			name: "resolvedDisks-only difference does not trigger",
			spec: keziov1alpha2.MachineSpec{ImageRef: &image1, DataImages: data1},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{
				ImageRef:   &image1,
				DataImages: data1,
				ResolvedDisks: []keziov1alpha2.DeployRunResolvedDisk{
					{ImageRef: image1, TargetDisk: "/dev/sda"},
				},
			}},
			want: false,
		},
		{
			name:    "empty payload never triggers even against a non-empty last run",
			spec:    keziov1alpha2.MachineSpec{},
			lastRun: &keziov1alpha2.DeployRun{Spec: keziov1alpha2.DeployRunSpec{ImageRef: &image1, DataImages: data1}},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha2.Machine{Spec: tc.spec}
			if got := shouldProvision(machine, tc.lastRun); got != tc.want {
				t.Errorf("shouldProvision() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReInspectAcceptable(t *testing.T) {
	image := keziov1alpha2.NameRef{Name: "test-image"}
	emptyPayload := keziov1alpha2.MachineSpec{}
	setPayload := keziov1alpha2.MachineSpec{ImageRef: &image}

	states := []string{
		"",
		keziov1alpha2.MachineStateEnrolling,
		keziov1alpha2.MachineStateInspecting,
		keziov1alpha2.MachineStateAvailable,
		keziov1alpha2.MachineStateProvisioning,
		keziov1alpha2.MachineStateProvisioned,
	}

	for _, state := range states {
		for _, tc := range []struct {
			payloadName string
			spec        keziov1alpha2.MachineSpec
		}{
			{"empty payload", emptyPayload},
			{"set payload", setPayload},
		} {
			want := state == keziov1alpha2.MachineStateAvailable ||
				(state == keziov1alpha2.MachineStateProvisioned && tc.payloadName == "empty payload")
			t.Run(fmt.Sprintf("state=%q/%s", state, tc.payloadName), func(t *testing.T) {
				machine := &keziov1alpha2.Machine{
					Spec:   tc.spec,
					Status: keziov1alpha2.MachineStatus{State: state},
				}
				if got := reInspectAcceptable(machine); got != want {
					t.Errorf("reInspectAcceptable() = %v, want %v", got, want)
				}
			})
		}
	}
}

var _ = Describe("Machine Controller", func() {
	// Every Context below that references the BMC credentials Secret by
	// its literal name "bmc-creds" relies on this shared, idempotently
	// created Secret existing before any reconcile runs the credentials
	// gate. Contexts that need to control the Secret's own lifecycle
	// (naming, deletion, mismatch) create and manage their own Secret
	// instead.
	BeforeEach(func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: "default"},
			Type:       corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("hunter2"),
			},
		}
		if err := k8sClient.Create(context.Background(), secret); err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

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
			By("adding a dataImages entry and confirming a third DeployRun is created")
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			thirdRunPredecessor := machine.Status.LastSuccessfulRunRef.Name
			machine.Spec.DataImages = []keziov1alpha2.MachineDataImage{
				{ImageRef: keziov1alpha2.NameRef{Name: "data-image-1"}},
			}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			reconcileUntilStable(reconciler, name)

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.LastSuccessfulRunRef.Name).NotTo(Equal(thirdRunPredecessor))

			var thirdRun keziov1alpha2.DeployRun
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machine.Status.LastSuccessfulRunRef.Name, Namespace: "default"}, &thirdRun)).To(Succeed())
			Expect(thirdRun.Spec.DataImages).To(HaveLen(1))

			By("removing the dataImages entry and confirming a fourth DeployRun is created")
			fourthRunPredecessor := machine.Status.LastSuccessfulRunRef.Name
			machine.Spec.DataImages = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			reconcileUntilStable(reconciler, name)

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.LastSuccessfulRunRef.Name).NotTo(Equal(fourthRunPredecessor))

			var fourthRun keziov1alpha2.DeployRun
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machine.Status.LastSuccessfulRunRef.Name, Namespace: "default"}, &fourthRun)).To(Succeed())
			Expect(fourthRun.Spec.DataImages).To(BeEmpty())
		})
	})
	Context("When the provisioning trigger's intent subset is unchanged or empty", func() {
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
			machineName = fmt.Sprintf("trigger-%d", time.Now().UnixNano())
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
					BootMACAddress: "aa:bb:cc:dd:ee:0d",
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

		It("does not create a new run when the intent subset is unchanged, and does not create one when spec.imageRef is cleared to empty", func() {
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			reconciler := &MachineReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Deployer: &deployer.FakeDeployer{Client: k8sClient},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			reconcileUntilStable(reconciler, name)

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			firstRunName := machine.Status.LastSuccessfulRunRef.Name

			By("reconciling again with an unchanged spec: no new run")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.LastSuccessfulRunRef.Name).To(Equal(firstRunName))
			Expect(machine.Status.CurrentRunRef.Name).To(Equal(firstRunName))

			By("clearing spec.imageRef to the empty payload: still no new run")
			machine.Spec.ImageRef = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.LastSuccessfulRunRef.Name).To(Equal(firstRunName))
			Expect(machine.Status.CurrentRunRef.Name).To(Equal(firstRunName))
		})

		It("treats a deleted lastSuccessfulRunRef DeployRun as no known run: it redeploys once instead of erroring or wedging", func() {
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			reconciler := &MachineReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Deployer: &deployer.FakeDeployer{Client: k8sClient},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			reconcileUntilStable(reconciler, name)

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			firstRunName := machine.Status.LastSuccessfulRunRef.Name

			By("deleting the run the status still references")
			staleRun := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: firstRunName, Namespace: "default"}}
			Expect(k8sClient.Delete(ctx, staleRun)).To(Succeed())

			By("reconciling: no error, and the machine redeploys exactly once")
			reconcileUntilStable(reconciler, name)

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.LastSuccessfulRunRef.Name).NotTo(Equal(firstRunName))
			secondRunName := machine.Status.LastSuccessfulRunRef.Name

			By("reconciling again: the fresh lastSuccessfulRunRef stops it from repeating")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.LastSuccessfulRunRef.Name).To(Equal(secondRunName))
		})
	})

	Context("When a Deployer step is delayed", func() {
		ctx := context.Background()

		It("sets operationalStatus=delayed without touching state, errorType, or errorCount, and requeues after the fixed interval", func() {
			machineName := fmt.Sprintf("delayed-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:0a",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			origInterval := delayedRequeueInterval
			delayedRequeueInterval = time.Millisecond
			DeferCleanup(func() { delayedRequeueInterval = origInterval })

			origJitter := jitter
			jitter = func(d time.Duration) time.Duration { return d }
			DeferCleanup(func() { jitter = origJitter })

			var calls int
			delayingDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					if calls == 1 {
						return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
					}
					return deployer.Result{Outcome: deployer.Delayed}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: delayingDeployer}

			By("reconciling through the finalizer add, Enrolling, and the failing Inspect call")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(mid.Status.ErrorCount).To(Equal(int32(1)))

			By("reconciling the Delayed outcome")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(delayedRequeueInterval))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDelayed))
			Expect(machine.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorTypeTransient), "delayed must not clear a prior error's errorType")
			Expect(machine.Status.ErrorCount).To(Equal(int32(1)), "delayed must not increase errorCount")
		})

		It("clears delayed back to OK when a subsequent Continuing outcome arrives in the same state", func() {
			machineName := fmt.Sprintf("delayed-clears-continuing-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:0b",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var calls int
			scriptedDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					switch calls {
					case 1:
						return deployer.Result{Outcome: deployer.Delayed}, nil
					case 2:
						return deployer.Result{Outcome: deployer.Continuing}, nil
					default:
						return deployer.Result{Outcome: deployer.Complete}, nil
					}
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: scriptedDeployer}

			By("reconciling through the finalizer add, Enrolling, and the Delayed Inspect call")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDelayed))

			By("reconciling the Continuing outcome that follows")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
		})

		It("clears delayed back to OK and lets the walk proceed once the deployer succeeds", func() {
			machineName := fmt.Sprintf("delayed-clears-success-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:0c",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var calls int
			scriptedDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					if calls == 1 {
						return deployer.Result{Outcome: deployer.Delayed}, nil
					}
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: scriptedDeployer}

			By("reconciling through the finalizer add, Enrolling, and the Delayed Inspect call")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var mid keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &mid)).To(Succeed())
			Expect(mid.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(mid.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDelayed))

			By("reconciling the successful retry that exits Inspecting")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
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

	Context("When the current DeployRun disappears mid-Provisioning", func() {
		ctx := context.Background()

		It("records the failure without changing state, then recovers by creating a fresh run and completing", func() {
			machineName := fmt.Sprintf("current-run-deleted-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "test-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:0e",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			fakeDeployer := &deployer.FakeDeployer{Client: k8sClient}
			// Never completes on its own: keeps the machine parked in
			// Provisioning with a stable currentRunRef until the test
			// deletes that run out from under it.
			fakeDeployer.ProvisionFunc = func(context.Context, *keziov1alpha2.Machine, *keziov1alpha2.DeployRun, bool) (deployer.Result, error) {
				return deployer.Result{Outcome: deployer.Continuing}, nil
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("walking to Provisioning with a currentRunRef set, and no further")
			var machine keziov1alpha2.Machine
			for i := 0; i < 50; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.State == keziov1alpha2.MachineStateProvisioning && machine.Status.CurrentRunRef != nil {
					break
				}
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioning))
			Expect(machine.Status.CurrentRunRef).NotTo(BeNil())
			firstRunName := machine.Status.CurrentRunRef.Name

			By("deleting the current run out from under the machine")
			currentRun := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: firstRunName, Namespace: "default"}}
			Expect(k8sClient.Delete(ctx, currentRun)).To(Succeed())

			By("reconciling: the deletion is reported as a failure, state stays Provisioning")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioning))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(machine.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorTypeRestart))
			Expect(machine.Status.CurrentRunRef).To(BeNil())

			By("recovering: the next reconcile starts a fresh run and it completes normally")
			fakeDeployer.ProvisionFunc = nil
			for i := 0; i < 50; i++ {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if result.RequeueAfter == 0 {
					Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
					if machine.Status.State == keziov1alpha2.MachineStateProvisioned {
						break
					}
				}
			}

			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(machine.Status.CurrentRunRef).NotTo(BeNil())
			Expect(machine.Status.CurrentRunRef.Name).NotTo(Equal(firstRunName))
			Expect(machine.Status.LastSuccessfulRunRef).NotTo(BeNil())
			Expect(machine.Status.LastSuccessfulRunRef.Name).NotTo(Equal(firstRunName))
		})
	})

	Context("Credential lifecycle", func() {
		ctx := context.Background()

		newCredentialsSecret := func(name string) *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Type:       corev1.SecretTypeBasicAuth,
				Data: map[string][]byte{
					"username": []byte("admin"),
					"password": []byte("hunter2"),
				},
			}
		}

		// deleteCredentialsSecret strips MachineCredentialsSecretFinalizer
		// before deleting, standing in for the Machine's onDelete release
		// step: a raw client Delete against a still-finalized Secret only
		// marks it for deletion, which would leave it (and its name)
		// occupied for the next spec that reuses the name.
		deleteCredentialsSecret := func(name string) {
			var current corev1.Secret
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &current); err != nil {
				Expect(errors.IsNotFound(err)).To(BeTrue())
				return
			}
			if len(current.Finalizers) > 0 {
				current.Finalizers = nil
				Expect(k8sClient.Update(ctx, &current)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, &current)).To(Succeed())
		}

		It("records triedCredentials before the attempt and goodCredentials once the deployer answers", func() {
			secretName := fmt.Sprintf("bmc-creds-%d", time.Now().UnixNano())
			secret := newCredentialsSecret(secretName)
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { deleteCredentialsSecret(secretName) })

			machineName := fmt.Sprintf("creds-tried-good-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: secretName},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:10",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			var calls int
			trackingDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(_ context.Context, m *keziov1alpha2.Machine, _ bool) (deployer.Result, error) {
					calls++
					// The attempt must see triedCredentials already recorded for
					// this secret before the deployer is called.
					Expect(m.Status.TriedCredentials.SecretRef).NotTo(BeNil())
					Expect(m.Status.TriedCredentials.SecretRef.Name).To(Equal(secretName))
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: trackingDeployer}

			By("reconciling through the finalizer add, Enrolling, and the Inspect call")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var current corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &current)).To(Succeed())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.TriedCredentials.SecretRef.Name).To(Equal(secretName))
			Expect(machine.Status.TriedCredentials.ResourceVersion).To(Equal(current.ResourceVersion))
			Expect(machine.Status.GoodCredentials.SecretRef.Name).To(Equal(secretName))
			Expect(machine.Status.GoodCredentials.ResourceVersion).To(Equal(current.ResourceVersion))
		})

		It("clears goodCredentials on a resourceVersion mismatch and re-establishes it on the next successful attempt, without ever changing state", func() {
			secretName := fmt.Sprintf("bmc-creds-%d", time.Now().UnixNano())
			secret := newCredentialsSecret(secretName)
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { deleteCredentialsSecret(secretName) })

			machineName := fmt.Sprintf("creds-mismatch-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "test-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: secretName},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:11",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			var provisionCalls int
			var blockProvisioning bool
			fakeDeployer := &deployer.FakeDeployer{Client: k8sClient}
			fakeDeployer.ProvisionFunc = func(pctx context.Context, m *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restart bool) (deployer.Result, error) {
				provisionCalls++
				if blockProvisioning {
					// A Failed outcome - not a transient error - keeps this
					// attempt from recording goodCredentials, so the test can
					// observe the mismatch-cleared state before the next
					// successful attempt re-establishes it.
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "blocked"}, nil
				}
				fakeDeployer.ProvisionFunc = nil
				return (&deployer.FakeDeployer{Client: k8sClient}).Provision(pctx, m, run, restart)
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("walking to Available, which records goodCredentials from the Inspect attempt")
			var machine keziov1alpha2.Machine
			for i := 0; i < 50; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.GoodCredentials.SecretRef != nil {
					break
				}
			}
			Expect(machine.Status.GoodCredentials.SecretRef).NotTo(BeNil())
			goodBeforeUpdate := machine.Status.GoodCredentials.ResourceVersion

			By("walking to a Provisioning attempt with a stable currentRunRef")
			blockProvisioning = true
			for i := 0; i < 50 && provisionCalls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioning))
			stateBeforeMismatch := machine.Status.State

			By("changing the secret so its resourceVersion no longer matches goodCredentials")
			var live corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &live)).To(Succeed())
			live.Data["password"] = []byte("swordfish")
			Expect(k8sClient.Update(ctx, &live)).To(Succeed())
			Expect(live.ResourceVersion).NotTo(Equal(goodBeforeUpdate))

			By("reconciling once more: the mismatch clears goodCredentials without changing state")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(stateBeforeMismatch))
			Expect(machine.Status.GoodCredentials.SecretRef).To(BeNil())
			Expect(machine.Status.TriedCredentials.ResourceVersion).To(Equal(live.ResourceVersion))

			By("letting provisioning succeed: goodCredentials is re-established with the new resourceVersion, state still never skipped a beat")
			blockProvisioning = false
			for i := 0; i < 50; i++ {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if result.RequeueAfter == 0 {
					Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
					if machine.Status.State == keziov1alpha2.MachineStateProvisioned {
						break
					}
				}
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(machine.Status.GoodCredentials.SecretRef).NotTo(BeNil())
			Expect(machine.Status.GoodCredentials.ResourceVersion).To(Equal(live.ResourceVersion))
		})

		It("requeues after credentialsSecretAbsentRequeueInterval, marks delayed, and never escalates to an error when the credentials secret does not exist", func() {
			machineName := fmt.Sprintf("creds-absent-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "no-such-secret"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:12",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			origJitter := jitter
			jitter = func(d time.Duration) time.Duration { return d }
			DeferCleanup(func() { jitter = origJitter })

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling through the finalizer add and the Enrolling to Inspecting transition")
			var result reconcile.Result
			var err error
			for i := 0; i < 10 && result.RequeueAfter != credentialsSecretAbsentRequeueInterval; i++ {
				result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(result.RequeueAfter).To(Equal(credentialsSecretAbsentRequeueInterval))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDelayed))
			Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
			Expect(machine.Status.TriedCredentials.SecretRef).To(BeNil())
			Expect(machine.Status.GoodCredentials.SecretRef).To(BeNil())
		})

		It("does not requeue when credentialsSecretRef.Name is empty, and waits for a spec edit instead", func() {
			machineName := fmt.Sprintf("creds-spec-invalid-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: ""},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:13",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling through the finalizer add and the Enrolling to Inspecting transition")
			var machine keziov1alpha2.Machine
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.State == keziov1alpha2.MachineStateInspecting {
					break
				}
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))

			By("reconciling the blocked Inspecting step: no requeue, no error, no error escalation")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(machine.Status.OperationalStatus).NotTo(Equal(keziov1alpha2.MachineOperationalStatusError))
			Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
			Expect(machine.Status.TriedCredentials.SecretRef).To(BeNil())
		})

		It("claims the credentials secret with a label, a non-controller owner reference, and a finalizer, and picks up a subsequent secret edit", func() {
			secretName := fmt.Sprintf("bmc-creds-%d", time.Now().UnixNano())
			secret := newCredentialsSecret(secretName)
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { deleteCredentialsSecret(secretName) })

			machineName := fmt.Sprintf("creds-claim-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: secretName},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:14",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			// Continuing on the first Inspect call keeps the machine in
			// Inspecting - and the credentials gate live - long enough to
			// observe a second, edited-secret attempt in the same state.
			var calls int
			scriptedDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					calls++
					if calls == 1 {
						return deployer.Result{Outcome: deployer.Continuing}, nil
					}
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: scriptedDeployer}

			By("reconciling through the finalizer add, Enrolling, and the first Inspect call that claims the secret")
			for i := 0; i < 10 && calls < 1; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(calls).To(Equal(1))

			var claimed corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &claimed)).To(Succeed())
			Expect(claimed.Labels).To(HaveKeyWithValue(keziov1alpha2.MachineCredentialsSecretLabel, annotationValueTrue))
			Expect(claimed.Finalizers).To(ContainElement(keziov1alpha2.MachineCredentialsSecretFinalizer))

			var ownerFound bool
			for _, ref := range claimed.OwnerReferences {
				if ref.Kind == "Machine" && ref.Name == machineName {
					ownerFound = true
					Expect(ref.Controller).To(BeNil(), "the BMC credentials secret owner reference must not be a controller reference")
				}
			}
			Expect(ownerFound).To(BeTrue(), "the BMC credentials secret must carry an owner reference to the Machine")

			By("editing the secret and reconciling the next Inspect attempt: the new resourceVersion is picked up")
			claimed.Data["password"] = []byte("new-password")
			Expect(k8sClient.Update(ctx, &claimed)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(calls).To(Equal(2))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.TriedCredentials.ResourceVersion).To(Equal(claimed.ResourceVersion))
		})

		It("releases the credentials secret finalizer when the Machine is deleted, leaving the Secret itself in place", func() {
			secretName := fmt.Sprintf("bmc-creds-%d", time.Now().UnixNano())
			secret := newCredentialsSecret(secretName)
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { deleteCredentialsSecret(secretName) })

			machineName := fmt.Sprintf("creds-release-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: secretName},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:15",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling until the secret is claimed")
			var claimed corev1.Secret
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &claimed)).To(Succeed())
				if len(claimed.Finalizers) > 0 {
					break
				}
			}
			Expect(claimed.Finalizers).To(ContainElement(keziov1alpha2.MachineCredentialsSecretFinalizer))

			By("deleting the Machine and reconciling its delete walk to completion")
			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			var gone bool
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if err := k8sClient.Get(ctx, name, &machine); errors.IsNotFound(err) {
					gone = true
					break
				}
			}
			Expect(gone).To(BeTrue(), "the Machine must be gone once the delete walk finishes")

			var released corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &released)).To(Succeed())
			Expect(released.Finalizers).NotTo(ContainElement(keziov1alpha2.MachineCredentialsSecretFinalizer))
		})
	})

	Context("Machine delete walk", func() {
		ctx := context.Background()

		reconcileUntilGone := func(reconciler *MachineReconciler, name types.NamespacedName, machine *keziov1alpha2.Machine) bool {
			GinkgoHelper()
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if err := k8sClient.Get(ctx, name, machine); errors.IsNotFound(err) {
					return true
				}
			}
			return false
		}

		It("walks deprovision then power-off, in order, before releasing the Machine", func() {
			machineName := fmt.Sprintf("delete-walk-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:20",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			var order []string
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				DeprovisionFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					order = append(order, "deprovision")
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
				PowerOffFunc: func(context.Context, *keziov1alpha2.Machine) (deployer.Result, error) {
					order = append(order, "power-off")
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("reconciling through the finalizer add and into a forward state")
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			By("reconciling the delete walk to completion")
			gone := reconcileUntilGone(reconciler, name, &machine)
			Expect(gone).To(BeTrue(), "the Machine must be gone once the delete walk finishes")
			Expect(order).To(Equal([]string{"deprovision", "power-off"}), "deprovision must run before power-off")
		})

		It("gives up a stage after errorCount exceeds 3, advances anyway, and bumps the give-up metric", func() {
			machineName := fmt.Sprintf("delete-giveup-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:21",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			var deprovisionCalls int
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				// Models a dead BMC: deprovision never succeeds on its own.
				DeprovisionFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					deprovisionCalls++
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "dead bmc"}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			before := testutil.ToFloat64(machineDeleteStageGiveUpTotal.WithLabelValues("deprovision"))

			By("reconciling the delete walk: bounded give-up must still finish it")
			gone := reconcileUntilGone(reconciler, name, &machine)
			Expect(gone).To(BeTrue(), "delete must complete within the bounded give-up even with a dead BMC")
			Expect(deprovisionCalls).To(Equal(4), "the stage must give up on the 4th failure (errorCount > 3)")

			after := testutil.ToFloat64(machineDeleteStageGiveUpTotal.WithLabelValues("deprovision"))
			Expect(after).To(Equal(before + 1))
		})

		It("does not block deletion when the BMC credentials secret is missing", func() {
			machineName := fmt.Sprintf("delete-no-secret-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "no-such-secret-for-delete"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:22",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling on the forward path, which stays blocked without a usable secret")
			for i := 0; i < 5; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDelayed))

			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			By("reconciling the delete walk: it proceeds with empty credentials instead of wedging")
			gone := reconcileUntilGone(reconciler, name, &machine)
			Expect(gone).To(BeTrue(), "deletion must not wedge on a missing BMC credentials secret")
		})
	})

	Context("When the paused annotation is present", func() {
		ctx := context.Background()

		It("returns immediately: no finalizer, no status write, no requeue", func() {
			machineName := fmt.Sprintf("paused-new-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:        machineName,
					Namespace:   "default",
					Annotations: map[string]string{keziov1alpha2.MachineAnnotationPaused: ""},
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:30",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Finalizers).To(BeEmpty(), "paused must return before the finalizer is added")
			Expect(machine.Status.State).To(BeEmpty(), "paused must return before any status write")
		})

		It("blocks the delete walk while paused, and resumes deletion once unpaused", func() {
			machineName := fmt.Sprintf("paused-delete-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:31",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling to add the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Finalizers).To(ContainElement(keziov1alpha2.MachineFinalizer))

			By("pausing, then deleting")
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationPaused: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			By("reconciling: the delete walk must not proceed while paused")
			for i := 0; i < 5; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed(), "the Machine must still exist while paused blocks its delete walk")
			Expect(machine.Status.State).To(BeEmpty(), "paused must block even the delete walk's first status write")

			By("unpausing: deletion proceeds and finishes")
			machine.Annotations = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			var gone bool
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if err := k8sClient.Get(ctx, name, &machine); errors.IsNotFound(err) {
					gone = true
					break
				}
			}
			Expect(gone).To(BeTrue(), "unpausing must let the delete walk finish")
		})
	})

	Context("When the detached annotation is present", func() {
		ctx := context.Background()

		It("sets operationalStatus=detached, freezes state, skips deployer calls, and resumes once removed", func() {
			machineName := fmt.Sprintf("detached-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:32",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var inspectCalls int
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					inspectCalls++
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("reconciling to add the finalizer, then detaching before Enrolling ever runs")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationDetached: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			By("reconciling while detached: state stays frozen and the deployer is never called")
			for i := 0; i < 5; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDetached))
			Expect(machine.Status.State).To(BeEmpty(), "detached must freeze state progress")
			Expect(inspectCalls).To(Equal(0), "detached must never call the deployer")

			By("removing the annotation resumes normal operation")
			machine.Annotations = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			for i := 0; i < 10 && machine.Status.State != keziov1alpha2.MachineStateAvailable; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
			Expect(inspectCalls).To(Equal(1))
		})

		It("skips deprovision and power-off on delete while detached", func() {
			machineName := fmt.Sprintf("detached-delete-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:33",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			var deprovisionCalls, powerOffCalls int
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				DeprovisionFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					deprovisionCalls++
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
				PowerOffFunc: func(context.Context, *keziov1alpha2.Machine) (deployer.Result, error) {
					powerOffCalls++
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("reconciling to add the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())

			By("detaching, then deleting")
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationDetached: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

			var gone bool
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				if err := k8sClient.Get(ctx, name, &machine); errors.IsNotFound(err) {
					gone = true
					break
				}
			}
			Expect(gone).To(BeTrue())
			Expect(deprovisionCalls).To(Equal(0), "detached must skip deprovision on delete")
			Expect(powerOffCalls).To(Equal(0), "detached must skip power-off on delete")
		})
	})

	Context("When a reboot annotation is present", func() {
		ctx := context.Background()

		It("calls Deployer.Reboot with hard=true when any holder asks for hard, and clears only the suffixless annotation", func() {
			machineName := fmt.Sprintf("reboot-multi-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: "default",
					Annotations: map[string]string{
						keziov1alpha2.MachineAnnotationRebootPrefix:        `{"mode":"soft"}`,
						keziov1alpha2.MachineAnnotationRebootPrefix + "-a": `{"mode":"hard"}`,
					},
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:34",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var rebootCalls int
			var lastHard bool
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				RebootFunc: func(_ context.Context, _ *keziov1alpha2.Machine, hard bool) (deployer.Result, error) {
					rebootCalls++
					lastHard = hard
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("reconciling to add the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling the reboot: hard wins, and only the suffixless annotation is cleared")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(rebootCalls).To(Equal(1))
			Expect(lastHard).To(BeTrue())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationRebootPrefix))
			Expect(machine.Annotations).To(HaveKey(keziov1alpha2.MachineAnnotationRebootPrefix + "-a"))
			Expect(machine.Status.State).To(BeEmpty(), "the remaining suffixed hold keeps the machine rebooting instead of progressing")

			By("the suffixed hold keeps triggering Reboot on every subsequent reconcile")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(rebootCalls).To(Equal(2))

			By("releasing the suffixed hold lets the machine resume normal progress")
			machine.Annotations = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			for i := 0; i < 10 && machine.Status.State != keziov1alpha2.MachineStateAvailable; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
		})

		It("treats invalid JSON as a soft reboot request but still honors it, clearing the suffixless annotation after acting", func() {
			machineName := fmt.Sprintf("reboot-invalid-json-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:        machineName,
					Namespace:   "default",
					Annotations: map[string]string{keziov1alpha2.MachineAnnotationRebootPrefix: `{not json`},
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:35",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var rebootCalls int
			var lastHard bool
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				RebootFunc: func(_ context.Context, _ *keziov1alpha2.Machine, hard bool) (deployer.Result, error) {
					rebootCalls++
					lastHard = hard
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			Expect(rebootCalls).To(Equal(1))
			Expect(lastHard).To(BeFalse(), "invalid JSON must be treated as a soft reboot request")

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationRebootPrefix), "the suffixless annotation is cleared once acted on, even though its value was invalid")
		})
	})

	Context("re-inspect annotation", func() {
		reconcileUntilState := func(reconciler *MachineReconciler, name types.NamespacedName, want string) {
			GinkgoHelper()
			var machine keziov1alpha2.Machine
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.State == want {
					return
				}
			}
			Fail(fmt.Sprintf("machine %q never reached state %q, stuck at %q", name.Name, want, machine.Status.State))
		}

		newBareMachine := func(machineName string, annotations map[string]string) types.NamespacedName {
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:        machineName,
					Namespace:   "default",
					Annotations: annotations,
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:40",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})
			return name
		}

		newMachineWithImage := func(machineName, bootMAC string, imageRef keziov1alpha2.NameRef) types.NamespacedName {
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
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
					BootMACAddress: bootMAC,
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})
			return name
		}

		countRunsForMachine := func(machineName string) int {
			GinkgoHelper()
			var runs keziov1alpha2.DeployRunList
			Expect(k8sClient.List(ctx, &runs, client.InNamespace("default"))).To(Succeed())
			count := 0
			for _, run := range runs.Items {
				if run.Spec.MachineRef.Name == machineName {
					count++
				}
			}
			return count
		}

		It("in Available: deletes and recreates MachineHardware and emits an accepted event", func() {
			machineName := fmt.Sprintf("reinspect-available-%d", GinkgoRandomSeed())
			name := newBareMachine(machineName, nil)

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("walking to Available with an empty deploy payload")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateAvailable)

			var firstHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &firstHW)).To(Succeed())
			Expect(firstHW.UID).NotTo(BeEmpty())

			By("setting the re-inspect annotation")
			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationReInspect: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			By("reconciling once: the annotation is consumed and the machine moves to Inspecting")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationReInspect))
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(<-recorder.Events).To(ContainSubstring("ReInspectAccepted"))

			By("MachineHardware was deleted, not merely left in place")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, name, &keziov1alpha2.MachineHardware{}))).To(BeTrue())

			By("walking back to Available recreates MachineHardware with a new UID")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateAvailable)

			var secondHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &secondHW)).To(Succeed())
			Expect(secondHW.UID).NotTo(Equal(firstHW.UID), "re-inspection must recreate MachineHardware, not patch it")
		})

		It("outside Available: consumes the annotation and emits a refused event without touching MachineHardware", func() {
			machineName := fmt.Sprintf("reinspect-refused-%d", GinkgoRandomSeed())
			name := newBareMachine(machineName, map[string]string{keziov1alpha2.MachineAnnotationReInspect: ""})

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("reconciling to add the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling again: state is still not Available, so the annotation is refused")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationReInspect), "the annotation is consumed even on refusal")
			Expect(<-recorder.Events).To(ContainSubstring("ReInspectRefused"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, name, &keziov1alpha2.MachineHardware{}))).To(BeTrue(), "a refused re-inspect must never touch MachineHardware")

			By("the walk still completes normally once the annotation is gone")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateAvailable)
		})

		It("in Provisioned with an empty deploy payload: accepts, walks Inspecting to Available, and stays there", func() {
			machineName := fmt.Sprintf("reinspect-provisioned-empty-%d", GinkgoRandomSeed())
			imageRef := keziov1alpha2.NameRef{Name: "reinspect-image"}
			name := newMachineWithImage(machineName, "aa:bb:cc:dd:ee:50", imageRef)

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("walking to Provisioned with a set deploy payload")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateProvisioned)
			Expect(countRunsForMachine(machineName)).To(Equal(1))

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			firstRunRef := machine.Status.LastSuccessfulRunRef.Name
			Expect(machine.Status.CurrentRunRef.Name).To(Equal(firstRunRef))

			var firstHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &firstHW)).To(Succeed())

			By("clearing the deploy payload and setting the re-inspect annotation")
			machine.Spec.ImageRef = nil
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationReInspect: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			By("reconciling once: the annotation is consumed and the machine moves to Inspecting")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationReInspect))
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateInspecting))
			Expect(<-recorder.Events).To(ContainSubstring("ReInspectAccepted"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, name, &keziov1alpha2.MachineHardware{}))).To(BeTrue())

			By("walking back to Available recreates MachineHardware with a new UID")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateAvailable)

			var secondHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &secondHW)).To(Succeed())
			Expect(secondHW.UID).NotTo(Equal(firstHW.UID), "re-inspection must recreate MachineHardware, not patch it")

			By("the machine stays in Available: an empty payload never re-triggers provisioning")
			for i := 0; i < 5; i++ {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
			}
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(countRunsForMachine(machineName)).To(Equal(1), "no spurious DeployRun on the way back to Available")

			By("run history survives the re-inspection untouched")
			Expect(machine.Status.LastSuccessfulRunRef.Name).To(Equal(firstRunRef))
			Expect(machine.Status.CurrentRunRef.Name).To(Equal(firstRunRef))
		})

		It("in Provisioned with a set deploy payload: refuses re-inspection and leaves state and MachineHardware untouched", func() {
			machineName := fmt.Sprintf("reinspect-provisioned-set-%d", GinkgoRandomSeed())
			imageRef := keziov1alpha2.NameRef{Name: "reinspect-image"}
			name := newMachineWithImage(machineName, "aa:bb:cc:dd:ee:51", imageRef)

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("walking to Provisioned with a set deploy payload")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateProvisioned)

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			firstRunRef := machine.Status.LastSuccessfulRunRef.Name

			var firstHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &firstHW)).To(Succeed())

			By("setting the re-inspect annotation while the deploy payload stays set")
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationReInspect: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			By("reconciling once: the annotation is consumed and refused")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationReInspect))
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateProvisioned))
			Expect(<-recorder.Events).To(ContainSubstring("ReInspectRefused"))

			var secondHW keziov1alpha2.MachineHardware
			Expect(k8sClient.Get(ctx, name, &secondHW)).To(Succeed())
			Expect(secondHW.UID).To(Equal(firstHW.UID), "a refused re-inspect must never touch MachineHardware")
			Expect(machine.Status.LastSuccessfulRunRef.Name).To(Equal(firstRunRef))
			Expect(countRunsForMachine(machineName)).To(Equal(1))
		})
	})

	Context("inspect-disable annotation", func() {
		It("skips Inspecting: no MachineHardware is created, and the walk still reaches Available", func() {
			machineName := fmt.Sprintf("inspect-disable-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:        machineName,
					Namespace:   "default",
					Annotations: map[string]string{keziov1alpha2.MachineAnnotationInspectDisable: annotationValueTrue},
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:41",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			var inspectCalls int
			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					inspectCalls++
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			var machine keziov1alpha2.Machine
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.State == keziov1alpha2.MachineStateAvailable {
					break
				}
			}
			Expect(machine.Status.State).To(Equal(keziov1alpha2.MachineStateAvailable))
			Expect(inspectCalls).To(Equal(0), "inspect-disable must skip the deployer's Inspect step entirely")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, name, &keziov1alpha2.MachineHardware{}))).To(BeTrue())
		})
	})

	Context("status-loss hold", func() {
		reconcileUntilState := func(reconciler *MachineReconciler, name types.NamespacedName, want string) {
			GinkgoHelper()
			var machine keziov1alpha2.Machine
			for i := 0; i < 20; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				if machine.Status.State == want {
					return
				}
			}
			Fail(fmt.Sprintf("machine %q never reached state %q, stuck at %q", name.Name, want, machine.Status.State))
		}

		countRunsForMachine := func(machineName string) int {
			GinkgoHelper()
			var runs keziov1alpha2.DeployRunList
			Expect(k8sClient.List(ctx, &runs, client.InNamespace("default"))).To(Succeed())
			count := 0
			for _, run := range runs.Items {
				if run.Spec.MachineRef.Name == machineName {
					count++
				}
			}
			return count
		}

		It("provisions a freshly created Machine with imageRef already set, without any operator action", func() {
			machineName := fmt.Sprintf("statusloss-fresh-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "fresh-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:60",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("a Machine created with imageRef set is empty status + set imageRef on its first reconcile, exactly like a status-loss shape - it must still provision on its own")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateProvisioned)

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha2.MachineConditionStatusLossHold)).To(BeFalse())
			Expect(countRunsForMachine(machineName)).To(Equal(1))
		})

		It("holds a Machine whose status was reset but whose DeployRuns still exist, until the confirm annotation is set", func() {
			machineName := fmt.Sprintf("statusloss-restored-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "restored-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:61",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})

			recorder := record.NewFakeRecorder(10)
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: recorder}

			By("walking the machine to Provisioned once, the way a real deployment would")
			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateProvisioned)
			Expect(countRunsForMachine(machineName)).To(Equal(1))

			By("simulating a status-only restore: status is wiped back to zero, spec (including the DeployRun-owning history) is untouched")
			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			machine.Status = keziov1alpha2.MachineStatus{}
			Expect(k8sClient.Status().Update(ctx, &machine)).To(Succeed())

			By("reconciling: the machine must enter the hold instead of silently re-enrolling/re-provisioning")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.State).To(BeEmpty(), "a held machine must not advance past its empty state")
			Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha2.MachineConditionStatusLossHold)).To(BeTrue())
			Expect(<-recorder.Events).To(ContainSubstring("StatusLossHold"))

			By("reconciling again while held: no new DeployRun, no requeue, no repeated event")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(countRunsForMachine(machineName)).To(Equal(1), "held must never start a new DeployRun")
			Consistently(recorder.Events).ShouldNot(Receive())

			By("confirming the hold releases it and the machine resumes the normal walk, re-provisioning")
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationConfirmStatusLoss: ""}
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			reconcileUntilState(reconciler, name, keziov1alpha2.MachineStateProvisioned)
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha2.MachineAnnotationConfirmStatusLoss), "the confirm annotation is consumed once acted on")
			Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha2.MachineConditionStatusLossHold)).To(BeFalse())
			Expect(countRunsForMachine(machineName)).To(Equal(2), "confirmation resumes the walk, which re-provisions since no successful run is on record")
		})

	})

	// Reconcile hygiene: the Machine update predicate now ignores
	// status-only self-writes, so every walk step that must run again has
	// to say so with an explicit requeue instead of counting on the watch
	// to fire again on its own. These specs drive Reconcile one call at a
	// time and assert the returned Result at each transition, covering the
	// forward walk, the delete walk, and the detached freeze/resume.
	Context("Reconcile hygiene: explicit requeue drives every self-write walk step", func() {
		ctx := context.Background()

		It("requeues explicitly at every forward-walk transition, and stops requeueing once idle", func() {
			machineName := fmt.Sprintf("hygiene-forward-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			imageRef := keziov1alpha2.NameRef{Name: "test-image"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:70",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
					ImageRef:       &imageRef,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			oneShotDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
				ProvisionFunc: func(context.Context, *keziov1alpha2.Machine, *keziov1alpha2.DeployRun, bool) (deployer.Result, error) {
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: oneShotDeployer}

			step := func(wantState string) {
				GinkgoHelper()
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{Requeue: true}))
				if wantState != "" {
					var machine keziov1alpha2.Machine
					Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
					Expect(machine.Status.State).To(Equal(wantState))
				}
			}

			By("adding the finalizer")
			step("")
			By("empty status -> Enrolling")
			step(keziov1alpha2.MachineStateEnrolling)
			By("Enrolling -> Inspecting")
			step(keziov1alpha2.MachineStateInspecting)
			By("Inspecting Complete -> Available")
			step(keziov1alpha2.MachineStateAvailable)
			By("Available, shouldProvision true -> Provisioning")
			step(keziov1alpha2.MachineStateProvisioning)
			By("Provisioning Complete -> Provisioned")
			step(keziov1alpha2.MachineStateProvisioned)

			By("Provisioned, shouldProvision false -> idle, no requeue")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("requeues explicitly at every delete-walk stage transition", func() {
			machineName := fmt.Sprintf("hygiene-delete-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:71",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			fakeDeployer := &deployer.FakeDeployer{
				Client: k8sClient,
				DeprovisionFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
				PowerOffFunc: func(context.Context, *keziov1alpha2.Machine) (deployer.Result, error) {
					return deployer.Result{Outcome: deployer.Complete}, nil
				},
			}
			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

			By("adding the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			var machine keziov1alpha2.Machine
			step := func(wantState string) {
				GinkgoHelper()
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{Requeue: true}))
				Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
				Expect(machine.Status.State).To(Equal(wantState))
			}

			By("deletionTimestamp set -> entering Deprovisioning")
			step(keziov1alpha2.MachineStateDeprovisioning)
			By("Deprovisioning Complete -> PoweringOff")
			step(keziov1alpha2.MachineStatePoweringOff)

			By("PoweringOff Complete -> release: the Machine is gone")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(HaveOccurred())
		})

		It("requeues explicitly when the detached annotation is cleared, resuming the walk", func() {
			machineName := fmt.Sprintf("hygiene-detached-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:        machineName,
					Namespace:   "default",
					Annotations: map[string]string{keziov1alpha2.MachineAnnotationDetached: ""},
				},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:72",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("adding the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling while detached: freezes with no crash")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusDetached))

			By("clearing the annotation and reconciling: requeues explicitly to resume the walk")
			machine.Annotations = nil
			Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true}))
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
		})
	})

	Context("Server-Side Apply field ownership", func() {
		ctx := context.Background()

		It("stamps status writes with the controller's field owner, with no ownership conflict across repeated reconciles", func() {
			machineName := fmt.Sprintf("ssa-owner-%d", GinkgoRandomSeed())
			name := types.NamespacedName{Name: machineName, Namespace: "default"}
			resource := &keziov1alpha2.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
				Spec: keziov1alpha2.MachineSpec{
					BMC: keziov1alpha2.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:70",
					SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

			reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}

			By("reconciling several status writes")
			for i := 0; i < 10; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			var machine keziov1alpha2.Machine
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())

			var statusManagers []string
			for _, mf := range machine.ManagedFields {
				if mf.Subresource == "status" {
					statusManagers = append(statusManagers, mf.Manager)
				}
			}
			Expect(statusManagers).NotTo(BeEmpty(), "expected at least one status managedFields entry")
			for _, manager := range statusManagers {
				Expect(manager).To(Equal(machineControllerFieldOwner))
			}

			By("reconciling again must not hit a Server-Side Apply ownership conflict")
			for i := 0; i < 5; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})
})
