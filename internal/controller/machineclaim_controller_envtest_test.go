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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// newAvailableTestMachine creates a Machine named name and drives its
// status straight to Available, bypassing MachineReconciler entirely:
// these tests exercise MachineClaimReconciler, which only ever reads
// spec.claimRef and status.state off a Machine.
func newAvailableTestMachine(ctx context.Context, name string, labels map[string]string) *keziov1alpha3.Machine {
	GinkgoHelper()
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec: keziov1alpha3.MachineSpec{
			BMC: keziov1alpha3.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha3.SecretReference{Name: "bmc-creds"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:30",
			SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
		},
	}
	Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	machine.Status.State = keziov1alpha3.MachineStateAvailable
	Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
	return machine
}

var _ = Describe("MachineClaim Controller", func() {
	ctx := context.Background()

	newReconciler := func() *MachineClaimReconciler {
		return &MachineClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20)}
	}

	newTestClaim := func(name string, spec keziov1alpha3.MachineClaimSpec) *keziov1alpha3.MachineClaim {
		GinkgoHelper()
		claim := &keziov1alpha3.MachineClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		return claim
	}

	It("binds a claim to the machine named by spec.machineName", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		machineName := fmt.Sprintf("claim-byname-machine-%d", GinkgoRandomSeed())
		machine := newAvailableTestMachine(ctx, machineName, nil)
		claim := newTestClaim(fmt.Sprintf("claim-byname-%d", GinkgoRandomSeed()), keziov1alpha3.MachineClaimSpec{
			MachineName: machineName,
			ImageRef:    &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()

		By("adding the finalizer")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("binding")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var bound keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &bound)).To(Succeed())
		Expect(bound.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhaseBound))
		Expect(bound.Status.MachineName).To(Equal(machineName))
		Expect(bound.Status.BoundAt).NotTo(BeNil())
		Expect(apimeta.IsStatusConditionTrue(bound.Status.Conditions, keziov1alpha3.MachineClaimConditionBound)).To(BeTrue())

		var boundMachine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &boundMachine)).To(Succeed())
		Expect(boundMachine.Spec.ClaimRef).NotTo(BeNil())
		Expect(boundMachine.Spec.ClaimRef.Name).To(Equal(claim.Name))
		Expect(boundMachine.Spec.ClaimRef.UID).To(Equal(bound.UID))

		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &bound)).To(Succeed())
		})
	})

	It("binds a claim to a machine matching spec.selector labels", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		suffix := GinkgoRandomSeed()
		matching := newAvailableTestMachine(ctx, fmt.Sprintf("claim-bysel-match-%d", suffix), map[string]string{"role": "leaf"})
		nonMatching := newAvailableTestMachine(ctx, fmt.Sprintf("claim-bysel-nomatch-%d", suffix), map[string]string{"role": "spine"})

		claim := newTestClaim(fmt.Sprintf("claim-bysel-%d", suffix), keziov1alpha3.MachineClaimSpec{
			Selector: &keziov1alpha3.MachineClaimSelector{
				LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "leaf"}},
			},
			ImageRef: &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var bound keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &bound)).To(Succeed())
		Expect(bound.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhaseBound))
		Expect(bound.Status.MachineName).To(Equal(matching.Name))

		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, matching)).To(Succeed())
			Expect(k8sClient.Delete(ctx, nonMatching)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &bound)).To(Succeed())
		})
	})

	It("stays Pending when no machine matches the selector", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		claim := newTestClaim(fmt.Sprintf("claim-nomatch-%d", GinkgoRandomSeed()), keziov1alpha3.MachineClaimSpec{
			Selector: &keziov1alpha3.MachineClaimSelector{
				LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "nonexistent"}},
			},
			ImageRef: &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()
		DeferCleanup(func() {
			var final keziov1alpha3.MachineClaim
			Expect(k8sClient.Get(ctx, key, &final)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &final)).To(Succeed())
		})

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		var pending keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &pending)).To(Succeed())
		Expect(pending.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhasePending))
		cond := apimeta.FindStatusCondition(pending.Status.Conditions, keziov1alpha3.MachineClaimConditionBound)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoMatchingMachine"))
	})

	It("does not steal a machine whose claimRef already names a different claim", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		machineName := fmt.Sprintf("claim-stale-machine-%d", GinkgoRandomSeed())
		machine := newAvailableTestMachine(ctx, machineName, nil)

		// Fabricate a claimRef to an unrelated claim identity - the shape a
		// stale binding takes (the claim that wrote it is gone or was
		// recreated with a different uid), without needing to actually
		// race a delete/recreate.
		machine.Spec.ClaimRef = &keziov1alpha3.MachineClaimReference{
			Name: "some-other-claim", Namespace: "default", UID: "11111111-1111-1111-1111-111111111111",
		}
		Expect(k8sClient.Update(ctx, machine)).To(Succeed())

		claim := newTestClaim(fmt.Sprintf("claim-stale-%d", GinkgoRandomSeed()), keziov1alpha3.MachineClaimSpec{
			MachineName: machineName,
			ImageRef:    &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			var final keziov1alpha3.MachineClaim
			Expect(k8sClient.Get(ctx, key, &final)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &final)).To(Succeed())
		})

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var stillPending keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &stillPending)).To(Succeed())
		Expect(stillPending.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhasePending))

		var untouched keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &untouched)).To(Succeed())
		Expect(untouched.Spec.ClaimRef).NotTo(BeNil())
		Expect(untouched.Spec.ClaimRef.Name).To(Equal("some-other-claim"))
	})

	It("clears status back to Pending when the bound machine's claimRef no longer points at it", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		machineName := fmt.Sprintf("claim-lost-machine-%d", GinkgoRandomSeed())
		machine := newAvailableTestMachine(ctx, machineName, nil)
		claim := newTestClaim(fmt.Sprintf("claim-lost-%d", GinkgoRandomSeed()), keziov1alpha3.MachineClaimSpec{
			MachineName: machineName,
			ImageRef:    &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			var final keziov1alpha3.MachineClaim
			Expect(k8sClient.Get(ctx, key, &final)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &final)).To(Succeed())
		})

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var bound keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &bound)).To(Succeed())
		Expect(bound.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhaseBound))

		By("simulating the machine's claimRef being cleared out from under the claim")
		var tampered keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &tampered)).To(Succeed())
		tampered.Spec.ClaimRef = nil
		Expect(k8sClient.Update(ctx, &tampered)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var lost keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &lost)).To(Succeed())
		Expect(lost.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhasePending))
		Expect(lost.Status.MachineName).To(BeEmpty())
		cond := apimeta.FindStatusCondition(lost.Status.Conditions, keziov1alpha3.MachineClaimConditionBound)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("LostBinding"))
	})

	It("clears claimRef and lets the machine move to Released when a bound claim is deleted", func() {
		ensureReadyTestImage(ctx, "machineclaim-test-image")
		imageRef := keziov1alpha3.NameRef{Name: "machineclaim-test-image"}
		machineName := fmt.Sprintf("claim-release-machine-%d", GinkgoRandomSeed())
		machine := newAvailableTestMachine(ctx, machineName, nil)
		claim := newTestClaim(fmt.Sprintf("claim-release-%d", GinkgoRandomSeed()), keziov1alpha3.MachineClaimSpec{
			MachineName: machineName,
			ImageRef:    &imageRef,
		})
		key := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		reconciler := newReconciler()
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
		})

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var bound keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &bound)).To(Succeed())
		Expect(bound.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhaseBound))

		By("marking the machine Provisioned, the way a real bound deployment would leave it")
		var provisioned keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &provisioned)).To(Succeed())
		provisioned.Status.State = keziov1alpha3.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, &provisioned)).To(Succeed())

		By("deleting the claim")
		Expect(k8sClient.Delete(ctx, &bound)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var gone keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, key, &gone)).To(HaveOccurred())

		var released keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &released)).To(Succeed())
		Expect(released.Spec.ClaimRef).To(BeNil())

		By("reconciling the machine: it moves from Provisioned to Released")
		machineReconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: record.NewFakeRecorder(20)}
		machineKey := types.NamespacedName{Name: machineName, Namespace: "default"}
		_, err = machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey}) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		_, err = machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey})
		Expect(err).NotTo(HaveOccurred())

		var finalMachine keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &finalMachine)).To(Succeed())
		Expect(finalMachine.Status.State).To(Equal(keziov1alpha3.MachineStateReleased))
	})
})
