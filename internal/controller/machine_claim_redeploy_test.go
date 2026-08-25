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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// Both this claim-generation-change watch (mapClaimToMachine) and the
// intent-comparison the redeploy trigger itself is built on
// (shouldProvision/intentSubsetEqual) already need every other guarantee
// this package's other controller tests exercise; this file only covers
// the piece specific to the claim layer: a bound MachineClaim's intent
// edit must actually reach the bound Machine's provisioning trigger, and
// re-observing unchanged intent must not.
var _ = Describe("Machine reconciliation driven by a bound MachineClaim's intent", func() {
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
		if err := k8sClient.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	newTestMachine := func(name string) *keziov1alpha3.Machine {
		machine := &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:50",
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		return machine
	}

	// deployRunsFor lists the DeployRuns owned by machineName, in
	// creation order.
	deployRunsFor := func(machineName string) []keziov1alpha3.DeployRun {
		var runs keziov1alpha3.DeployRunList
		Expect(k8sClient.List(ctx, &runs, client.InNamespace("default"))).To(Succeed())
		var mine []keziov1alpha3.DeployRun
		for _, run := range runs.Items {
			if run.Spec.MachineRef.Name == machineName {
				mine = append(mine, run)
			}
		}
		return mine
	}

	It("starts a new DeployRun when the bound claim's dataImages change, and not on an identical re-apply", func() {
		suffix := GinkgoRandomSeed()
		machineName := fmt.Sprintf("claim-redeploy-machine-%d", suffix)
		machine := newTestMachine(machineName)
		machineKey := types.NamespacedName{Name: machineName, Namespace: "default"}
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

		machineReconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: record.NewFakeRecorder(20)}

		By("driving the machine to Available")
		for range 10 {
			_, err := machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey})
			Expect(err).NotTo(HaveOccurred())
		}
		var available keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, machineKey, &available)).To(Succeed())
		Expect(available.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))

		By("binding a dataImages-only claim to it")
		claim := &keziov1alpha3.MachineClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-redeploy-%d", suffix), Namespace: "default"},
			Spec: keziov1alpha3.MachineClaimSpec{
				MachineName: machineName,
				DataImages:  []keziov1alpha3.MachineDataImage{{ImageRef: keziov1alpha3.NameRef{Name: "data-v1"}}},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		claimKey := types.NamespacedName{Name: claim.Name, Namespace: "default"}
		DeferCleanup(func() {
			var final keziov1alpha3.MachineClaim
			if err := k8sClient.Get(ctx, claimKey, &final); err == nil {
				Expect(k8sClient.Delete(ctx, &final)).To(Succeed())
			}
		})

		claimReconciler := &MachineClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20)}
		_, err := claimReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey}) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		_, err = claimReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey}) // binds
		Expect(err).NotTo(HaveOccurred())

		var bound keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, claimKey, &bound)).To(Succeed())
		Expect(bound.Status.Phase).To(Equal(keziov1alpha3.MachineClaimPhaseBound))

		By("driving the machine through Provisioning to Provisioned on the first dataImages intent")
		for range 15 {
			_, err := machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey})
			Expect(err).NotTo(HaveOccurred())
		}
		var provisioned keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, machineKey, &provisioned)).To(Succeed())
		Expect(provisioned.Status.State).To(Equal(keziov1alpha3.MachineStateProvisioned))
		Expect(deployRunsFor(machineName)).To(HaveLen(1), "the first dataImages intent must produce exactly one DeployRun")

		By("mapClaimToMachine resolving the bound claim to its machine, the mapping SetupWithManager's watch installs")
		Expect(k8sClient.Get(ctx, claimKey, &bound)).To(Succeed())
		requests := machineReconciler.mapClaimToMachine(ctx, &bound)
		Expect(requests).To(ConsistOf(reconcile.Request{NamespacedName: machineKey}))

		By("re-applying the identical claim spec: no new DeployRun, matching the request a no-op patch produces")
		unchanged := bound.DeepCopy()
		Expect(k8sClient.Update(ctx, unchanged)).To(Succeed())
		_, err = machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(deployRunsFor(machineName)).To(HaveLen(1), "an identical claim re-apply must not start a second DeployRun")

		By("changing the claim's dataImages: the provisioning trigger fires again")
		var toChange keziov1alpha3.MachineClaim
		Expect(k8sClient.Get(ctx, claimKey, &toChange)).To(Succeed())
		toChange.Spec.DataImages = []keziov1alpha3.MachineDataImage{{ImageRef: keziov1alpha3.NameRef{Name: "data-v2"}}}
		Expect(k8sClient.Update(ctx, &toChange)).To(Succeed())

		// mapClaimToMachine is what a real watch would resolve this claim
		// update to; reconcile the Machine request it produces directly,
		// the same convention this package's other watch-mapping tests
		// follow (see subnet_site_watch_test.go's mapSiteToSubnets).
		Expect(machineReconciler.mapClaimToMachine(ctx, &toChange)).To(ConsistOf(reconcile.Request{NamespacedName: machineKey}))

		for range 15 {
			_, err := machineReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: machineKey})
			Expect(err).NotTo(HaveOccurred())
		}
		var reprovisioned keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, machineKey, &reprovisioned)).To(Succeed())
		Expect(reprovisioned.Status.State).To(Equal(keziov1alpha3.MachineStateProvisioned))
		Expect(deployRunsFor(machineName)).To(HaveLen(2), "a changed dataImages intent must start exactly one new DeployRun")
	})

	It("maps an unbound claim to no Machine", func() {
		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}
		claim := &keziov1alpha3.MachineClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-unbound-%d", GinkgoRandomSeed()), Namespace: "default"},
		}
		Expect(reconciler.mapClaimToMachine(ctx, claim)).To(BeNil())
	})
})
