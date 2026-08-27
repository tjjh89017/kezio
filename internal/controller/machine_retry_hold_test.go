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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// This suite covers maxRestartFailures's give-up threshold: a Machine
// stuck failing the same restart-classified error must stop retrying and
// hold for an operator instead of re-arming PXE and power-cycling forever.
// Every spec drives the hold through MachineReconciler.Reconcile against a
// bare, claim-less Machine (Inspecting is enough to exercise recordFailure;
// nothing here needs a MachineClaim/Image).
var _ = Describe("Machine restart-failure retry hold", func() {
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
	})

	// newRetryHoldMachine creates a Machine unique to suffix and registers
	// its cleanup. No claim/image: the walk only needs to reach Inspecting
	// for these specs, and FakeDeployer's Inspect is overridden anyway.
	newRetryHoldMachine := func(suffix string) types.NamespacedName {
		GinkgoHelper()
		machineName := fmt.Sprintf("retryhold-%s-%d", suffix, GinkgoRandomSeed())
		resource := &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: fmt.Sprintf("aa:bb:cc:e2:%02x:%02x", GinkgoRandomSeed()%256, GinkgoRandomSeed()%251),
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		name := types.NamespacedName{Name: machineName, Namespace: "default"}
		DeferCleanup(func() {
			var machine keziov1alpha3.Machine
			if err := k8sClient.Get(ctx, name, &machine); err == nil {
				Expect(k8sClient.Delete(ctx, &machine)).To(Succeed())
			}
		})
		return name
	}

	// reachInspecting drives reconciler through the finalizer add and the
	// Enrolling/Inspecting transitions, stopping the instant the deployer's
	// Inspect is actually called for the first time.
	reachInspecting := func(reconciler *MachineReconciler, name types.NamespacedName, calls *int) {
		GinkgoHelper()
		for i := 0; i < 10 && *calls < 1; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(*calls).To(Equal(1), "Inspect was never called")
	}

	It("holds after the 3rd consecutive restart failure, emitting an Event and the RetryHeld condition, and stops requeuing", func() {
		name := newRetryHoldMachine("basic")

		var calls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha3.Machine, bool) (deployer.Result, error) {
				calls++
				return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha3.MachineErrorTypeRestart, ErrorMessage: fmt.Sprintf("boom %d", calls)}, nil
			},
		}
		recorder := record.NewFakeRecorder(10)
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer, Recorder: recorder}

		By("reaching Inspecting: the 1st restart failure requeues normally")
		reachInspecting(reconciler, name, &calls)

		var machine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.RestartCount).To(Equal(int32(1)))
		Expect(machine.Status.ErrorType).To(Equal(keziov1alpha3.MachineErrorTypeRestart))

		By("the 2nd restart failure also requeues normally")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(failedRequeueInterval))
		Expect(calls).To(Equal(2))
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.RestartCount).To(Equal(int32(2)))
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeFalse())

		By("the 3rd restart failure holds instead of requeuing")
		result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		Expect(calls).To(Equal(3))

		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.RestartCount).To(Equal(int32(3)))
		Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusError))
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeTrue())
		Expect(<-recorder.Events).To(ContainSubstring("BootRetryExhausted"))

		By("reconciling again while held: no further Inspect call, no requeue, no repeated event")
		result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		Expect(calls).To(Equal(3), "held must not call the deployer again")
		Consistently(recorder.Events).ShouldNot(Receive())
	})

	It("does not let a Transient failure in between consume one of the three restart attempts", func() {
		name := newRetryHoldMachine("transient-between")

		// Restart, Restart, Transient, Restart: the hold must trigger on
		// the 4th call (3 Restart failures), not the 3rd - the Transient
		// call must leave restartCount unchanged.
		errorTypes := []keziov1alpha3.MachineErrorType{
			keziov1alpha3.MachineErrorTypeRestart,
			keziov1alpha3.MachineErrorTypeRestart,
			keziov1alpha3.MachineErrorTypeTransient,
			keziov1alpha3.MachineErrorTypeRestart,
		}
		var calls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha3.Machine, bool) (deployer.Result, error) {
				calls++
				idx := calls - 1
				if idx >= len(errorTypes) {
					idx = len(errorTypes) - 1
				}
				return deployer.Result{Outcome: deployer.Failed, ErrorType: errorTypes[idx], ErrorMessage: "boom"}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer, Recorder: record.NewFakeRecorder(10)}

		reachInspecting(reconciler, name, &calls)

		for calls < len(errorTypes) {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			if calls < len(errorTypes) {
				Expect(result.RequeueAfter).To(Equal(failedRequeueInterval), fmt.Sprintf("call %d must still be retrying, not held", calls))
			}
		}
		Expect(calls).To(Equal(4), "the hold must trigger on the 4th call: 3 Restart failures plus 1 Transient that must not count")

		var machine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.RestartCount).To(Equal(int32(3)))
		Expect(machine.Status.ErrorCount).To(Equal(int32(4)), "errorCount counts every failure, Transient included")
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeTrue())
	})

	It("clears the hold and retries once the clear-error annotation is set, and consumes the annotation", func() {
		name := newRetryHoldMachine("clear")

		var calls int
		var cleared bool
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha3.Machine, bool) (deployer.Result, error) {
				calls++
				if cleared {
					return deployer.Result{Outcome: deployer.Complete}, nil
				}
				return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha3.MachineErrorTypeRestart, ErrorMessage: "boom"}, nil
			},
		}
		recorder := record.NewFakeRecorder(10)
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer, Recorder: recorder}

		By("driving the machine into the hold")
		reachInspecting(reconciler, name, &calls)
		for calls < maxRestartFailures {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
		}
		var machine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeTrue())
		Expect(<-recorder.Events).To(ContainSubstring("BootRetryExhausted"))

		By("setting the clear-error annotation")
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		machine.Annotations = map[string]string{keziov1alpha3.MachineAnnotationClearError: ""}
		Expect(k8sClient.Update(ctx, &machine)).To(Succeed())
		cleared = true

		By("reconciling once: the annotation is consumed, the error and both counters are cleared, and the walk requeues immediately")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{Requeue: true}))

		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha3.MachineAnnotationClearError), "the annotation is consumed once acted on")
		Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusOK))
		Expect(machine.Status.ErrorType).To(BeEmpty())
		Expect(machine.Status.ErrorMessage).To(BeEmpty())
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
		Expect(machine.Status.RestartCount).To(Equal(int32(0)))
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeFalse())
		Expect(<-recorder.Events).To(ContainSubstring("ErrorCleared"))

		By("the walk actually resumes: Inspect is called again and now succeeds")
		callsBeforeResume := calls
		for i := 0; i < 10; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
			if machine.Status.State == keziov1alpha3.MachineStateAvailable {
				break
			}
		}
		Expect(machine.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))
		Expect(calls).To(BeNumerically(">", callsBeforeResume), "the deployer must be called again after the annotation clears the hold")
	})

	It("honors the clear-error annotation even when the machine is not held", func() {
		name := newRetryHoldMachine("clear-not-held")

		var calls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha3.Machine, bool) (deployer.Result, error) {
				calls++
				if calls <= 1 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha3.MachineErrorTypeTransient, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Complete}, nil
			},
		}
		recorder := record.NewFakeRecorder(10)
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer, Recorder: recorder}

		reachInspecting(reconciler, name, &calls)

		var machine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusError))
		Expect(meta.IsStatusConditionTrue(machine.Status.Conditions, keziov1alpha3.MachineConditionRetryHeld)).To(BeFalse(), "one Transient failure must not hold")

		machine.Annotations = map[string]string{keziov1alpha3.MachineAnnotationClearError: ""}
		Expect(k8sClient.Update(ctx, &machine)).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{Requeue: true}))

		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Annotations).NotTo(HaveKey(keziov1alpha3.MachineAnnotationClearError))
		Expect(machine.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusOK))
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
	})

	It("resets restartCount once a subsequent attempt succeeds", func() {
		name := newRetryHoldMachine("success-resets")

		var calls int
		fakeDeployer := &deployer.FakeDeployer{
			Client: k8sClient,
			InspectFunc: func(context.Context, *keziov1alpha3.Machine, bool) (deployer.Result, error) {
				calls++
				if calls <= 2 {
					return deployer.Result{Outcome: deployer.Failed, ErrorType: keziov1alpha3.MachineErrorTypeRestart, ErrorMessage: "boom"}, nil
				}
				return deployer.Result{Outcome: deployer.Complete}, nil
			},
		}
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: fakeDeployer, Recorder: record.NewFakeRecorder(10)}

		reachInspecting(reconciler, name, &calls)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(2))

		var machine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.RestartCount).To(Equal(int32(2)))

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(3))

		Expect(k8sClient.Get(ctx, name, &machine)).To(Succeed())
		Expect(machine.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))
		Expect(machine.Status.RestartCount).To(Equal(int32(0)))
		Expect(machine.Status.ErrorCount).To(Equal(int32(0)))
	})
})
