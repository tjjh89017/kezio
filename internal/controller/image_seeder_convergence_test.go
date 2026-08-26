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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// Mirrors subnet_bootd_convergence_test.go's two bootd specs for the
// per-(Image, Site) seeder Deployment: it must be recreated once deleted
// (createImageSeederDeployment's dep==nil branch, the same branch the
// Deployment watch's delete event drives in production), and its
// container image must catch up once ImageSeederConfig.Image changes,
// without a placement-only reconcile silently leaving the old image in
// place.
var _ = Describe("Image Controller seeder Deployment convergence", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("recreates a Site's seeder Deployment once it is deleted", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-740", "site-740")
		mustCreateMachineSubnet(ctx, "machine-subnet-740", "site-740")

		contentName := "pc-" + imageSeederTestHash(740)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-740", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-740", img.Name, "machine-subnet-740")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		Expect(k8sClient.Delete(ctx, &dep)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, depKey, &appsv1.Deployment{})
		}).ShouldNot(Succeed(), "the Deployment must actually be gone before the next reconcile")

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.UID).NotTo(Equal(firstUID), "a genuinely new object, not a stale read of the deleted one")
	})

	It("converges an existing seeder Deployment's container image once ImageSeederConfig.Image changes", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-741", "site-741")
		mustCreateMachineSubnet(ctx, "machine-subnet-741", "site-741")

		contentName := "pc-" + imageSeederTestHash(741)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-741", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-741", img.Name, "machine-subnet-741")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:old"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentity), Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(mustFindContainer(&dep, "ezio").Image).To(Equal("example.test/kezio-seeder:old"))
		Expect(mustFindContainer(&dep, "seeder-register").Image).To(Equal("example.test/kezio-seeder:old"))

		// Simulate the manager restarting with a new
		// PARTITIONCONTENT_SEEDER_IMAGE: same running Image and Site, a
		// reconciler carrying the changed config.
		r.Seeder.Image = "example.test/kezio-seeder:new"
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(mustFindContainer(&dep, "ezio").Image).To(Equal("example.test/kezio-seeder:new"),
			"an existing seeder Deployment must converge to the newly configured image, not keep the one it was created with")
		Expect(mustFindContainer(&dep, "seeder-register").Image).To(Equal("example.test/kezio-seeder:new"))
	})
})
