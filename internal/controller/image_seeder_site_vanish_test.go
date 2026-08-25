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
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

var _ = Describe("Image Controller seeder demand when a Site vanishes", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("runs the grace-period shutdown once a Site with a deploying Machine is deleted, keeps the Deployment if the Site is recreated in time, and deletes it once the grace period actually elapses", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-720", "site-720")
		mustCreateMachineSubnet(ctx, "machine-subnet-720", "site-720")

		contentName := "pc-" + imageSeederTestHash(720)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-720", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-720", img.Name, "machine-subnet-720")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test", GracePeriod: grace, Now: clock.now},
		}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		depName := seederdeploy.Name(img.Name, siteIdentity)
		depKey := types.NamespacedName{Name: depName, Namespace: "default"}

		// The Site is alive and the Machine deploys against it: the seeder
		// Deployment exists.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		// The Site is deleted. sitederive.Resolve now fails
		// (ErrSiteNotFound) for the Machine that named it, so demand for
		// this Site drops to zero exactly as it would if the Machine
		// itself had been deleted - the grace-period countdown must start,
		// not an immediate delete.
		var site keziov1alpha3.Site
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "site-720", Namespace: "default"}, &site)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &site)).To(Succeed())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(grace))
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederEmptySinceAnnotation))
		Expect(dep.UID).To(Equal(firstUID), "the grace period must not delete the Deployment on the Site's disappearance alone")

		// The Site is recreated mid-grace (a mistaken delete, an apply
		// race): the same seeding Subnet still names it back, so demand
		// resolves again and the countdown must cancel.
		recreatedSite := &keziov1alpha3.Site{
			ObjectMeta: metav1.ObjectMeta{Name: "site-720", Namespace: "default"},
			Spec:       keziov1alpha3.SiteSpec{SeederSubnetRef: &keziov1alpha3.NameRef{Name: "seed-subnet-720"}},
		}
		Expect(k8sClient.Create(ctx, recreatedSite)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation))
		Expect(dep.UID).To(Equal(firstUID), "recreating the Site within the grace period must find its seeder still there, not a fresh one")

		// Delete the Site again and this time let the grace period
		// actually elapse: the Deployment must be removed.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "site-720", Namespace: "default"}, &site)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &site)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederEmptySinceAnnotation))

		clock.t = clock.t.Add(grace + time.Second)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, depKey, &dep)
		Expect(kerrors.IsNotFound(err)).To(BeTrue(), "once the grace period actually elapses, the Site-less seeder Deployment must be deleted")
	})

	It("does not let a Machine whose Site was deleted block another Site's seeder demand", func() {
		vanishedSiteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-721a", "site-721a")
		goodSiteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-721b", "site-721b")
		mustCreateMachineSubnet(ctx, "machine-subnet-721a", "site-721a")
		mustCreateMachineSubnet(ctx, "machine-subnet-721b", "site-721b")

		contentName := "pc-" + imageSeederTestHash(721)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-721", []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		vanished := newTestMachineOnSubnet("machine-721a", img.Name, "machine-subnet-721a")
		Expect(k8sClient.Create(ctx, vanished)).To(Succeed())
		good := newTestMachineOnSubnet("machine-721b", img.Name, "machine-subnet-721b")
		Expect(k8sClient.Create(ctx, good)).To(Succeed())

		var siteA keziov1alpha3.Site
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "site-721a", Namespace: "default"}, &siteA)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &siteA)).To(Succeed())

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var depGood appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(img.Name, goodSiteIdentity), Namespace: "default"}, &depGood)).To(Succeed())
		Expect(depGood.Spec.Replicas).NotTo(BeNil())
		Expect(*depGood.Spec.Replicas).To(Equal(int32(1)))

		var depVanished appsv1.Deployment
		err = k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(img.Name, vanishedSiteIdentity), Namespace: "default"}, &depVanished)
		Expect(kerrors.IsNotFound(err)).To(BeTrue(), "a Machine whose Site cannot resolve must never gain a seeder Deployment of its own")
	})
})
