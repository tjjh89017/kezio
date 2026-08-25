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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// partitionContentSeederReasonTestHash keeps this file's info-hash
// sequence independent from every other test file's own.
func partitionContentSeederReasonTestHash(seq int) string {
	return fmt.Sprintf("%040x", seq+9000)
}

// newIndexedReconcilerWithSeeder mirrors newIndexedReconciler
// (partitioncontent_finalizer_test.go), additionally wiring Seeder - the
// field only this file's tests need set to something other than its zero
// value.
func newIndexedReconcilerWithSeeder(ctx context.Context, seeder ImageSeederConfig) (*PartitionContentReconciler, func()) {
	c, err := cache.New(cfg, cache.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	Expect(c.IndexField(ctx, &keziov1alpha3.Image{}, imageContentRefIndex, indexImageContentRefs)).To(Succeed())
	Expect(c.IndexField(ctx, &keziov1alpha3.Machine{}, machineImageRefIndex, indexMachineImageRefs)).To(Succeed())

	cacheCtx, cancel := context.WithCancel(ctx)
	go func() { _ = c.Start(cacheCtx) }()
	Expect(c.WaitForCacheSync(cacheCtx)).To(BeTrue())

	cl, err := client.New(cfg, client.Options{
		Scheme: k8sClient.Scheme(),
		Cache: &client.CacheOptions{
			Reader: c,
			DisableFor: []client.Object{
				&keziov1alpha3.PartitionContent{},
				&keziov1alpha3.DeployRun{},
				&corev1.PersistentVolumeClaim{},
				&batchv1.Job{},
				&appsv1.Deployment{},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())

	r := &PartitionContentReconciler{
		Client:   cl,
		Scheme:   k8sClient.Scheme(),
		Recorder: record.NewFakeRecorder(16),
		Seeder:   seeder,
	}
	return r, cancel
}

var _ = Describe("PartitionContent seeder degraded reasons", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("reports SeederImageUnconfigured, not the generic unavailable reason, when demand exists but no seeder image is configured on the manager", func() {
		mustCreateSeedingSite(ctx, "seed-subnet-9001", "site-9001")
		mustCreateMachineSubnet(ctx, "machine-subnet-9001", "site-9001")

		contentName := "pc-" + partitionContentSeederReasonTestHash(1)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9001", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-9001", img.Name, "machine-subnet-9001")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, machine) })

		// Zero-value ImageSeederConfig: not ready() - no seeder image
		// configured anywhere on the manager.
		r, cancel := newIndexedReconcilerWithSeeder(ctx, ImageSeederConfig{})
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederImageUnconfigured"))
	})

	It("reports SeederUnavailable, distinct from the configuration gap, once a seeder image is configured but no pod is available yet", func() {
		mustCreateSeedingSite(ctx, "seed-subnet-9002", "site-9002")
		mustCreateMachineSubnet(ctx, "machine-subnet-9002", "site-9002")

		contentName := "pc-" + partitionContentSeederReasonTestHash(2)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9002", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-9002", img.Name, "machine-subnet-9002")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, machine) })

		// Configured, but no ImageReconciler runs in this suite, so no
		// Deployment exists anywhere for this Site yet.
		r, cancel := newIndexedReconcilerWithSeeder(ctx, ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederUnavailable"))
	})

	It("reports SeederSubnetRefUnset, naming the Site, when demand comes only from a Site with no seeding Subnet", func() {
		siteName := "site-9003"
		site := &keziov1alpha3.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: "default"},
			// No SeederSubnetRef: this Site runs no seeder at all.
		}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, site) })
		siteIdentity := "default/" + siteName
		mustCreateMachineSubnet(ctx, "machine-subnet-9003", siteName)

		contentName := "pc-" + partitionContentSeederReasonTestHash(3)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9003", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-9003", img.Name, "machine-subnet-9003")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, machine) })

		r, cancel := newIndexedReconcilerWithSeeder(ctx, ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederSubnetRefUnset"))
		Expect(cond.Message).To(ContainSubstring(siteIdentity))
	})

	It("clears to SeederAvailable=False once a seeder Deployment is available, unchanged from before this reason granularity existed", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-9004", "site-9004")
		mustCreateMachineSubnet(ctx, "machine-subnet-9004", "site-9004")

		contentName := "pc-" + partitionContentSeederReasonTestHash(4)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9004", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-9004", img.Name, "machine-subnet-9004")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, machine) })

		ir := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		imgNN := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := ir.Reconcile(ctx, reconcile.Request{NamespacedName: imgNN})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		r, cancel := newIndexedReconcilerWithSeeder(ctx, ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("SeederAvailable"))
	})
})
