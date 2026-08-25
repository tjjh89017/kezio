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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// TestOperationalStatusOKIsOnlyWrittenByMarkOperational guards the whole
// defect class the specs below cover one path at a time. Those specs can
// only catch the reset paths that exist when they are written; a reset
// path added later, spelling the assignment out again instead of calling
// markOperational, would reintroduce a healthy machine displaying a stale
// errorType with no spec failing. This parses machine_controller.go's own
// AST rather than trusting a comment to stay accurate, the same approach
// TestMachineControllerGoSourceDoesNotImportBMC takes.
func TestOperationalStatusOKIsOnlyWrittenByMarkOperational(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report this test file's path")
	}
	sourcePath := filepath.Join(filepath.Dir(thisFile), "machine_controller.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, sourcePath, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", sourcePath, err)
	}

	writesOK := func(stmt *ast.AssignStmt) bool {
		for _, lhs := range stmt.Lhs {
			sel, isSel := lhs.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "OperationalStatus" {
				continue
			}
			for _, rhs := range stmt.Rhs {
				if val, isSel := rhs.(*ast.SelectorExpr); isSel && val.Sel.Name == "MachineOperationalStatusOK" {
					return true
				}
			}
		}
		return false
	}

	var found []string
	for _, decl := range f.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if assign, isAssign := n.(*ast.AssignStmt); isAssign && writesOK(assign) {
				found = append(found, fn.Name.Name)
			}
			return true
		})
	}

	if len(found) != 1 || found[0] != "markOperational" {
		t.Fatalf("operationalStatus is set to OK in %v; every reset path must go through markOperational so errorType/errorMessage are cleared with it", found)
	}
}

// This suite covers one reset path per spec: every place that puts
// operationalStatus back to OK must also drop the errorType/errorMessage
// pair that described the failure it just left. Each spec drives the
// transition (fail, then recover) through MachineReconciler.Reconcile
// rather than calling the reset helper directly - a helper-level test
// cannot catch a reset path that forgets to call the helper, which is the
// defect class here.
var _ = Describe("Machine error detail clearing", func() {
	ctx := context.Background()

	BeforeEach(func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: "default"},
			Type:       corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("hunter2"),
			},
		}
		if err := k8sClient.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		ensureReadyTestImage(ctx, "test-image")
	})

	// newErrorClearMachine creates a Machine whose name and MAC are unique
	// to suffix, and registers its cleanup.
	newErrorClearMachine := func(suffix string, mac byte, imageRef *keziov1alpha2.NameRef) types.NamespacedName {
		GinkgoHelper()
		machineName := fmt.Sprintf("errclear-%s-%d", suffix, GinkgoRandomSeed())
		resource := &keziov1alpha2.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
			Spec: keziov1alpha2.MachineSpec{
				BMC: keziov1alpha2.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: fmt.Sprintf("aa:bb:cc:e1:%02x:%02x", mac, GinkgoRandomSeed()%256),
				SubnetRef:      keziov1alpha2.NameRef{Name: "default"},
				ImageRef:       imageRef,
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		name := types.NamespacedName{Name: machineName, Namespace: "default"}
		DeferCleanup(func() {
			var machine keziov1alpha2.Machine
			if err := k8sClient.Get(ctx, name, &machine); err == nil {
				Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())
			}
		})
		return name
	}

	// reconcileUntil drives Reconcile until cond holds, returning the
	// Machine as it stood when it did.
	reconcileUntil := func(r *MachineReconciler, name types.NamespacedName, description string, cond func(*keziov1alpha2.Machine) bool) *keziov1alpha2.Machine {
		GinkgoHelper()
		machine := &keziov1alpha2.Machine{}
		for range 60 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, machine)).To(Succeed())
			if cond(machine) {
				return machine
			}
		}
		Fail("the machine never reached: " + description)
		return nil
	}

	expectNoErrorDetail := func(machine *keziov1alpha2.Machine) {
		GinkgoHelper()
		Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha2.MachineOperationalStatusOK))
		Expect(machine.Status.ErrorType).To(BeEmpty(), "a machine that is no longer in error must not keep the errorType of the failure it left")
		Expect(machine.Status.ErrorMessage).To(BeEmpty(), "a machine that is no longer in error must not keep the errorMessage of the failure it left")
	}

	inError := func(m *keziov1alpha2.Machine) bool {
		return m.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusError
	}

	It("clears the error detail when a state transition resets the error", func() {
		// setState: Inspecting -> Available after a failed Inspect.
		name := newErrorClearMachine("setstate", 0x01, nil)

		var inspectCalls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
				inspectCalls++
				if inspectCalls == 1 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Complete}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("failing the first Inspect")
		machine := reconcileUntil(reconciler, name, "a recorded Inspect failure", inError)
		Expect(machine.Status.ErrorMessage).To(Equal("boom"))

		By("letting the retry succeed into Available")
		machine = reconcileUntil(reconciler, name, "Available", func(m *keziov1alpha2.Machine) bool {
			return m.Status.State == keziov1alpha2.MachineStateAvailable
		})
		expectNoErrorDetail(machine)
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
	})

	It("clears the error detail when a vanished current run is replaced by a fresh one", func() {
		// startProvisioningRun, reached through recordCurrentRunDeleted -
		// the exact shape that made a healthy machine report a deleted
		// run as its current error.
		imageRef := keziov1alpha2.NameRef{Name: "test-image"}
		name := newErrorClearMachine("runvanished", 0x02, &imageRef)

		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			ProvisionFunc: func(context.Context, *keziov1alpha2.Machine, *keziov1alpha2.DeployRun, bool) (deployer.Result, error) {
				return deployer.Result{Outcome: deployer.Continuing}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("holding a first provisioning run in flight")
		machine := reconcileUntil(reconciler, name, "a run in flight", func(m *keziov1alpha2.Machine) bool {
			return m.Status.CurrentRunRef != nil
		})
		firstRun := machine.Status.CurrentRunRef.Name

		By("deleting that run out from under the machine")
		run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: firstRun, Namespace: "default"}}
		Expect(k8sClient.Delete(ctx, run)).To(Succeed())

		machine = reconcileUntil(reconciler, name, "the vanished run recorded as a failure", inError)
		Expect(machine.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorTypeRestart))
		Expect(machine.Status.ErrorMessage).To(ContainSubstring(firstRun))

		By("starting the replacement run")
		machine = reconcileUntil(reconciler, name, "a replacement run", func(m *keziov1alpha2.Machine) bool {
			return m.Status.CurrentRunRef != nil && m.Status.CurrentRunRef.Name != firstRun
		})
		expectNoErrorDetail(machine)
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
	})

	It("clears the error detail once the provisioning run completes", func() {
		imageRef := keziov1alpha2.NameRef{Name: "test-image"}
		name := newErrorClearMachine("provisioned", 0x03, &imageRef)

		failNextProvision := true
		passthrough := &deployer.FakeDeployer{Client: k8sClient}
		fakeDeployer := &deployer.FakeDeployer{Client: k8sClient}
		fakeDeployer.ProvisionFunc = func(c context.Context, m *keziov1alpha2.Machine, run *keziov1alpha2.DeployRun, restarting bool) (deployer.Result, error) {
			if failNextProvision {
				failNextProvision = false
				return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
			}
			return passthrough.Provision(c, m, run, restarting)
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("failing the first provisioning attempt")
		machine := reconcileUntil(reconciler, name, "a recorded provisioning failure", inError)
		Expect(machine.Status.ErrorMessage).To(Equal("boom"))

		By("retrying the same run to success")
		machine = reconcileUntil(reconciler, name, "Provisioned", func(m *keziov1alpha2.Machine) bool {
			return m.Status.State == keziov1alpha2.MachineStateProvisioned
		})
		expectNoErrorDetail(machine)
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
	})

	It("clears the error detail when a delete stage advances after a failure", func() {
		// advanceDeleteStage: deprovision fails, then succeeds, and the
		// delete walk moves to PoweringOff.
		name := newErrorClearMachine("deletestage", 0x04, nil)

		var deprovisionCalls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			DeprovisionFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
				deprovisionCalls++
				if deprovisionCalls == 1 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Complete}, nil
			},
			// Park the walk at PoweringOff so the status written by the
			// stage advance is still readable.
			PowerOffFunc: func(context.Context, *keziov1alpha2.Machine) (deployer.Result, error) {
				return deployer.Result{Outcome: deployer.Continuing}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("reconciling until the finalizer is in place, then deleting")
		reconcileUntil(reconciler, name, "the finalizer", func(m *keziov1alpha2.Machine) bool {
			return len(m.Finalizers) > 0
		})
		var machine keziov1alpha2.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())

		By("failing the deprovision stage")
		recorded := reconcileUntil(reconciler, name, "a recorded delete stage failure", inError)
		Expect(recorded.Status.ErrorMessage).To(Equal("boom"))

		By("letting the retry advance the walk to PoweringOff")
		advanced := reconcileUntil(reconciler, name, "PoweringOff", func(m *keziov1alpha2.Machine) bool {
			return m.Status.State == keziov1alpha2.MachineStatePoweringOff
		})
		expectNoErrorDetail(advanced)
		Expect(advanced.Status.ErrorCount).To(Equal(int32(0)))

		By("letting the walk finish so the Machine is released")
		fakeDeployer.PowerOffFunc = func(context.Context, *keziov1alpha2.Machine) (deployer.Result, error) {
			return deployer.Result{Outcome: deployer.Complete}, nil
		}
		var gone bool
		for range 20 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			if err := k8sClient.Get(ctx, name, &machine); errors.IsNotFound(err) {
				gone = true
				break
			}
		}
		Expect(gone).To(BeTrue())
	})

	It("clears the error detail when the detached annotation is removed", func() {
		name := newErrorClearMachine("detached", 0x05, nil)

		var inspectCalls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
				inspectCalls++
				if inspectCalls == 1 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Continuing}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("failing the first Inspect")
		machine := reconcileUntil(reconciler, name, "a recorded Inspect failure", inError)
		Expect(machine.Status.ErrorMessage).To(Equal("boom"))

		By("detaching the machine, which freezes the recorded error")
		machine.Annotations = map[string]string{keziov1alpha2.MachineAnnotationDetached: ""}
		Expect(k8sClient.Update(ctx, machine)).To(Succeed())
		machine = reconcileUntil(reconciler, name, "detached", func(m *keziov1alpha2.Machine) bool {
			return m.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusDetached
		})

		By("re-attaching it")
		machine.Annotations = nil
		Expect(k8sClient.Update(ctx, machine)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, name, machine)).To(Succeed())
		expectNoErrorDetail(machine)
	})

	It("clears the error detail when a Restart retry reports progress", func() {
		// clearRetryStatus, restart branch.
		name := newErrorClearMachine("restartretry", 0x06, nil)

		var inspectCalls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
				inspectCalls++
				if inspectCalls == 1 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeRestart, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Continuing}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("failing the first Inspect with a Restart-classified error")
		machine := reconcileUntil(reconciler, name, "a recorded Restart failure", inError)
		Expect(machine.Status.ErrorType).To(Equal(keziov1alpha2.MachineErrorTypeRestart))

		By("letting the restart retry report progress")
		machine = reconcileUntil(reconciler, name, "the cleared error status", func(m *keziov1alpha2.Machine) bool {
			return m.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusOK
		})
		expectNoErrorDetail(machine)
		Expect(machine.Status.ErrorCount).To(Equal(int32(1)),
			"a restart is a discarded attempt, not a success: errorCount stays as backoff evidence")
	})

	It("clears the error detail when a delayed step reports progress after an earlier failure", func() {
		// clearRetryStatus, delayed branch: delayed itself never touches
		// the error detail, so without clearing here the error outlives
		// both the failure and the delay.
		name := newErrorClearMachine("delayedretry", 0x07, nil)

		var inspectCalls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha2.Machine, bool) (deployer.Result, error) {
				inspectCalls++
				switch inspectCalls {
				case 1:
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha2.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				case 2:
					return deployer.Result{Outcome: deployer.Delayed}, nil
				default:
					return deployer.Result{Outcome: deployer.Continuing}, nil
				}
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer}

		By("failing the first Inspect, then delaying the retry")
		reconcileUntil(reconciler, name, "a recorded Inspect failure", inError)
		reconcileUntil(reconciler, name, "delayed", func(m *keziov1alpha2.Machine) bool {
			return m.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusDelayed
		})

		By("letting the next call report progress")
		machine := reconcileUntil(reconciler, name, "the cleared delayed status", func(m *keziov1alpha2.Machine) bool {
			return m.Status.OperationalStatus == keziov1alpha2.MachineOperationalStatusOK
		})
		expectNoErrorDetail(machine)
	})
})
