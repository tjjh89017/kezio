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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// mustSetMachineState sets machineName's status.state directly, standing
// in for the Machine reconciler the same way mustSetSiteTrackerURL stands
// in for the Site reconciler in these tests.
func mustSetMachineState(ctx context.Context, machineName, state string) {
	var machine keziov1alpha3.Machine
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &machine)).To(Succeed())
	machine.Status.State = state
	Expect(k8sClient.Status().Update(ctx, &machine)).To(Succeed())
}

// mustCreateDeployRun creates a DeployRun naming machineName and imageName,
// standing in for MachineReconciler.createDeployRun - the fields the
// provisioning trigger (shouldProvision/intentSubsetEqual) and seed-demand
// (machineDeployPending) both compare.
func mustCreateDeployRun(ctx context.Context, name, machineName, imageName string) *keziov1alpha3.DeployRun {
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha3.DeployRunSpec{
			MachineRef: keziov1alpha3.NameRef{Name: machineName},
			ImageRef:   &keziov1alpha3.NameRef{Name: imageName},
		},
	}
	Expect(k8sClient.Create(ctx, run)).To(Succeed())
	return run
}

// mustSetMachineLastSuccessfulRun sets machineName's status to Provisioned
// with lastSuccessfulRunRef naming runName - the shape reconcileProvisioning
// leaves a Machine in once a DeployRun succeeds.
func mustSetMachineLastSuccessfulRun(ctx context.Context, machineName, runName string) {
	var machine keziov1alpha3.Machine
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machineName, Namespace: "default"}, &machine)).To(Succeed())
	machine.Status.State = keziov1alpha3.MachineStateProvisioned
	machine.Status.LastSuccessfulRunRef = &keziov1alpha3.NameRef{Name: runName}
	Expect(k8sClient.Status().Update(ctx, &machine)).To(Succeed())
}

var _ = Describe("Image Controller seed demand ends when a Machine's deploy finishes", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("keeps seed demand for an Available Machine with a bound claim", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-750", "site-750")
		mustCreateMachineSubnet(ctx, "machine-subnet-750", "site-750")

		contentName := "pc-" + imageSeederTestHash(750)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-750", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-750", img.Name, "machine-subnet-750")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		mustSetMachineState(ctx, machine.Name, keziov1alpha3.MachineStateAvailable)

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation))
	})

	It("keeps seed demand for a Provisioning Machine with a bound claim", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-751", "site-751")
		mustCreateMachineSubnet(ctx, "machine-subnet-751", "site-751")

		contentName := "pc-" + imageSeederTestHash(751)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-751", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-751", img.Name, "machine-subnet-751")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		mustSetMachineState(ctx, machine.Name, keziov1alpha3.MachineStateProvisioning)

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation))
	})

	It("drops seed demand once a Machine reaches Provisioned with a matching last successful run", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-752", "site-752")
		mustCreateMachineSubnet(ctx, "machine-subnet-752", "site-752")

		contentName := "pc-" + imageSeederTestHash(752)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-752", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-752", img.Name, "machine-subnet-752")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		mustSetMachineState(ctx, machine.Name, keziov1alpha3.MachineStateAvailable)

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test", GracePeriod: grace, Now: clock.now},
		}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		// Demand: 1, while the Machine has not deployed yet.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		// The deploy finishes: a Succeeded DeployRun naming img, and the
		// Machine moves to Provisioned pointing at it - exactly matching
		// the claim's still-current intent, so no re-provision is pending.
		run := mustCreateDeployRun(ctx, "run-752", machine.Name, img.Name)
		run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		mustSetMachineLastSuccessfulRun(ctx, machine.Name, run.Name)

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(grace), "a Provisioned Machine whose deploy is fully caught up must not keep the seeder demanded")
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederEmptySinceAnnotation))
		Expect(dep.UID).To(Equal(firstUID), "the grace period must not delete the Deployment the moment demand drops")

		// The grace period actually elapses: the Deployment is removed.
		clock.t = clock.t.Add(grace + time.Second)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, depKey, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "once the grace period elapses past every Machine's finished deploy, the seeder Deployment must be deleted")
	})

	It("keeps seed demand for a Provisioned Machine whose claim intent no longer matches its last successful run", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-753", "site-753")
		mustCreateMachineSubnet(ctx, "machine-subnet-753", "site-753")

		contentName := "pc-" + imageSeederTestHash(753)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-753", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-753", img.Name, "machine-subnet-753")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		// The last successful run deployed a different Image than the
		// claim currently names: a re-deploy against img is still pending
		// the next time reconcileIdle runs (shouldProvision would fire),
		// so this Machine must keep counting toward img's own seed demand.
		run := mustCreateDeployRun(ctx, "run-753", machine.Name, "image-753-superseded")
		run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		mustSetMachineLastSuccessfulRun(ctx, machine.Name, run.Name)

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation), "a Provisioned Machine with a pending re-provision trigger must still count as seed demand")
	})

	It("keeps seed demand for a Provisioned Machine from an active DeployRun naming the Image, independent of the Machine's own bound claim", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-754", "site-754")
		mustCreateMachineSubnet(ctx, "machine-subnet-754", "site-754")

		contentName := "pc-" + imageSeederTestHash(754)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-754", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-754", img.Name, "machine-subnet-754")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		// The Machine's own bound claim already matches its last
		// successful run - by itself, this Machine would not count. An
		// independent active (non-terminal) DeployRun naming img must
		// still keep the Site's seed demand alive - demandMachinesForImage
		// treats an active DeployRun as its own demand source, unaffected
		// by the Machine's own state.
		lastRun := mustCreateDeployRun(ctx, "run-754-last", machine.Name, img.Name)
		lastRun.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, lastRun)).To(Succeed())
		mustSetMachineLastSuccessfulRun(ctx, machine.Name, lastRun.Name)

		mustCreateDeployRun(ctx, "run-754-active", machine.Name, img.Name)

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation))
	})

	It("cancels the grace-period shutdown once a new claim binds at the Site before it elapses", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-755", "site-755")
		mustCreateMachineSubnet(ctx, "machine-subnet-755", "site-755")

		contentName := "pc-" + imageSeederTestHash(755)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-755", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-755a", img.Name, "machine-subnet-755")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		mustSetMachineState(ctx, machine.Name, keziov1alpha3.MachineStateAvailable)

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test", GracePeriod: grace, Now: clock.now},
		}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		// machine-755a finishes deploying: demand drops to zero and the
		// grace countdown starts.
		run := mustCreateDeployRun(ctx, "run-755a", machine.Name, img.Name)
		run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		mustSetMachineLastSuccessfulRun(ctx, machine.Name, run.Name)

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(grace))
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederEmptySinceAnnotation))

		// A new claim binds a second Machine to the same Image at the same
		// Site, mid-grace: demand must reappear and the countdown cancel,
		// on the very same Deployment.
		machineB := newTestMachineOnSubnet("machine-755b", img.Name, "machine-subnet-755")
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation), "a new claim binding before the grace period elapses must cancel the shutdown")
		Expect(dep.UID).To(Equal(firstUID), "the same seeder Deployment must survive, never deleted and recreated")

		// Advancing the clock now must not delete it either: demand is
		// live again.
		clock.t = clock.t.Add(grace + time.Second)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
	})
})
