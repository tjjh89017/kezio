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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// testClock is a settable time source for driving
// PartitionContentSeederConfig.Now in tests, so the grace-period
// countdown advances without sleeping real time.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var _ = Describe("PartitionContent Controller seeder lifecycle", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// advancePartitionContentToReady drives an already-created
	// PartitionContent through Pending -> Publishing -> Ready, faking the
	// publish Job's success the way the publish-walk tests in
	// partitioncontent_controller_test.go do. It leaves the seeder
	// lifecycle untouched - callers issue their own Reconcile calls after
	// this to exercise it.
	advancePartitionContentToReady := func(r *PartitionContentReconciler, nn types.NamespacedName, hashHex string) {
		reconcileAddsFinalizer(ctx, r, nn)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(hash), Namespace: nn.Namespace}, &job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var ready keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &ready)).To(Succeed())
		Expect(ready.Status.State).To(Equal(keziov1alpha2.PartitionContentStateReady))
	}

	// addSeedDemand creates an Image (in "default") referencing
	// contentName and a Machine naming that Image as its spec.imageRef -
	// the minimal referencing pair resolveSeedDemand looks for. The
	// caller names both so a test that toggles demand mid-run can delete
	// and recreate the Machine (dropSeedDemand/restoreSeedDemand) without
	// colliding with another test's objects.
	addSeedDemand := func(imageName, machineName, contentName string) {
		img := newTestImage(imageName, contentName)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachine(machineName, imageName)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	}

	// dropSeedDemand deletes machineName's Machine, the seed-demand source
	// addSeedDemand created - no finalizer holds it in this suite (nothing
	// runs MachineReconciler here), so the delete is immediate.
	dropSeedDemand := func(machineName string) {
		machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: "default"}}
		Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
	}

	// restoreSeedDemand recreates machineName's Machine after
	// dropSeedDemand removed it.
	restoreSeedDemand := func(imageName, machineName string) {
		machine := newTestMachine(machineName, imageName)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	}

	It("creates an owner-referenced seeder Deployment with the configured image and a read-only content mount once seed-demand is set on Ready content", func() {
		hashHex := partitionContentTestHash(100)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		addSeedDemand("image-demand-100", "machine-demand-100", name)

		seederImage := "example.test/kezio-seeder:test"
		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{Image: seederImage})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}, &dep)).To(Succeed())
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal(name))
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Template.Spec.Containers).NotTo(BeEmpty())
		for _, c := range dep.Spec.Template.Spec.Containers {
			Expect(c.Image).To(Equal(seederImage))
			Expect(c.VolumeMounts).To(HaveLen(1))
			Expect(c.VolumeMounts[0].MountPath).To(Equal(ingest.ContentMountPath(hash)))
			Expect(c.VolumeMounts[0].ReadOnly).To(BeTrue())
		}
		Expect(dep.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim).NotTo(BeNil())
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(store.PVCName(hash)))
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())

		var registerContainer *corev1.Container
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "seeder-register" {
				registerContainer = &dep.Spec.Template.Spec.Containers[i]
			}
		}
		Expect(registerContainer).NotTo(BeNil())
		Expect(registerContainer.ReadinessProbe).NotTo(BeNil())
		Expect(registerContainer.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(registerContainer.ReadinessProbe.HTTPGet.Path).To(Equal(seederdeploy.TorrentHealthzPath))
		Expect(registerContainer.ReadinessProbe.HTTPGet.Port).To(Equal(intstr.FromString("torrent")))
	})

	It("creates no seeder Deployment when content is Ready but no seed-demand marker is set", func() {
		hashHex := partitionContentTestHash(101)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		err = k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("creates no seeder Deployment while content is not yet Ready, even with seed-demand set", func() {
		hashHex := partitionContentTestHash(102)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		addSeedDemand("image-demand-102", "machine-demand-102", name)

		r, cancel := newIndexedReconciler(ctx,
			PartitionContentPublishConfig{}, // not ready() -> stays Pending
			PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.PartitionContentStatePending))

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		err = k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("holds SeederDegraded=True with no Deployment when seed-demand is set but no seeder image is configured, leaving Ready untouched", func() {
		hashHex := partitionContentTestHash(103)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		addSeedDemand("image-demand-103", "machine-demand-103", name)

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{}) // no image
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.PartitionContentStateReady))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal("SeederImageMissing"))

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		err = k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("publishes one seeders[] entry and flips SeederDegraded to False once the seeder Deployment reports an available replica", func() {
		hashHex := partitionContentTestHash(104)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		addSeedDemand("image-demand-104", "machine-demand-104", name)

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.Seeders).To(BeEmpty())
		degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal("SeederUnavailable"))

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.Seeders).To(ConsistOf(keziov1alpha2.PartitionContentSeederSite{Site: defaultSeederSite, MachineCount: 0}))
		degraded = meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
		Expect(degraded.Reason).To(Equal("SeederAvailable"))
	})

	It("keeps a seeder Deployment through its grace period after seed-demand is removed, deletes it once elapsed, and cancels shutdown if demand reappears mid-grace", func() {
		hashHex := partitionContentTestHash(105)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		imageName, machineName := "image-demand-105", "machine-demand-105"
		addSeedDemand(imageName, machineName, name)

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{
			Image:       "example.test/kezio-seeder:test",
			GracePeriod: grace,
			Now:         clock.now,
		})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		depKey := types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())

		// Demand drops: the Deployment must survive, with a grace-period
		// countdown started on it. Reconcile is retried (not just the
		// Get/assert) because the Machine's removal reaches
		// resolveSeedDemand through the index-backed cache, which can lag
		// the delete this test just issued by a beat.
		dropSeedDemand(machineName)
		Eventually(func(g Gomega) {
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(grace))
			g.Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
			g.Expect(dep.Annotations).To(HaveKey(partitionContentSeederEmptySinceAnnotation))
		}).Should(Succeed())

		// Demand reappears mid-grace: the countdown must be cancelled.
		restoreSeedDemand(imageName, machineName)
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
			g.Expect(dep.Annotations).NotTo(HaveKey(partitionContentSeederEmptySinceAnnotation))
		}).Should(Succeed())

		// Demand drops again and the clock runs past the grace period: the
		// Deployment must actually be deleted.
		dropSeedDemand(machineName)
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
			g.Expect(dep.Annotations).To(HaveKey(partitionContentSeederEmptySinceAnnotation))
		}).Should(Succeed())
		clock.advance(grace + time.Second)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, depKey, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("tolerates a terminating seeder Deployment without recreating or re-deleting it, whether demand is present or absent", func() {
		hashHex := partitionContentTestHash(106)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		machineName := "machine-demand-106"
		addSeedDemand("image-demand-106", machineName, name)

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		depKey := types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())

		// Pin a finalizer on the Deployment so deleting it leaves it
		// lingering with DeletionTimestamp set instead of removing it, the
		// way real Kubernetes GC leaves an object mid-cleanup.
		const testFinalizer = "kezio.kojuro.date/test-hold"
		dep.Finalizers = append(dep.Finalizers, testFinalizer)
		Expect(k8sClient.Update(ctx, &dep)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &dep)).To(Succeed())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
		terminatingUID := dep.UID

		// Demand still present: must not attempt to recreate under the same
		// name while GC is still cleaning up (Create would fail
		// AlreadyExists).
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.UID).To(Equal(terminatingUID))
		Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())

		// Demand removed: must not attempt to force-delete or re-delete an
		// already-terminating Deployment.
		dropSeedDemand(machineName)
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
			g.Expect(dep.UID).To(Equal(terminatingUID))
			g.Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())

		// Let it go.
		dep.Finalizers = nil
		Expect(k8sClient.Update(ctx, &dep)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, depKey, &dep))
		}).Should(BeTrue())
	})

	It("keeps seeders[] populated during a grace-period countdown while the Deployment still reports an available replica", func() {
		hashHex := partitionContentTestHash(107)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		machineName := "machine-demand-107"
		addSeedDemand("image-demand-107", machineName, name)

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{
			Image:       "example.test/kezio-seeder:test",
			GracePeriod: grace,
			Now:         clock.now,
		})
		DeferCleanup(cancel)
		advancePartitionContentToReady(r, nn, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		depKey := types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		// Demand drops while the Deployment is already reporting an
		// available replica: the grace-period countdown starts, but the
		// Deployment itself is untouched, so Seeders must still reflect it.
		dropSeedDemand(machineName)
		var got keziov1alpha2.PartitionContent
		Eventually(func(g Gomega) {
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(grace))
			g.Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			g.Expect(got.Status.Seeders).To(ConsistOf(keziov1alpha2.PartitionContentSeederSite{Site: defaultSeederSite, MachineCount: 0}))
		}).Should(Succeed())
		// No demand means SeederDegraded does not apply - see
		// recordSeederStatus's doc comment - so it must be absent, not
		// False.
		degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
		Expect(degraded).To(BeNil())
	})
})
