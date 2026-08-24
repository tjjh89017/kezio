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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// partitionContentSeederSiteTestHash keeps this file's info-hash sequence
// independent from every other test file's own.
func partitionContentSeederSiteTestHash(seq int) string {
	return fmt.Sprintf("%040x", seq+9500)
}

// mustMarkDeploymentAvailable stands in for a scheduled, ready seeder pod:
// no Deployment controller runs in envtest, so status.availableReplicas
// stays zero unless a test writes it.
func mustMarkDeploymentAvailable(ctx context.Context, name string) {
	var dep appsv1.Deployment
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &dep)).To(Succeed())
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	dep.Status.AvailableReplicas = 1
	Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())
}

var _ = Describe("PartitionContent status.seeders[] site keys and machine counts", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("reports one entry keyed by the Site identity with machineCount 2 when two machines of one Site demand the content", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-9501", "site-9501")
		mustCreateMachineSubnet(ctx, "machine-subnet-9501a", "site-9501")
		mustCreateMachineSubnet(ctx, "machine-subnet-9501b", "site-9501")

		contentName := "pc-" + partitionContentSeederSiteTestHash(1)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9501", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		for _, m := range []*keziov1alpha2.Machine{
			newTestMachineOnSubnet("machine-9501a", img.Name, "machine-subnet-9501a"),
			newTestMachineOnSubnet("machine-9501b", img.Name, "machine-subnet-9501b"),
		} {
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		}

		seeder := ImageSeederConfig{Image: "example.test/kezio-seeder:test"}
		ir := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: seeder}
		_, err := ir.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: img.Name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		mustMarkDeploymentAvailable(ctx, seederdeploy.Name(img.Name, siteIdentity))

		r, cancel := newIndexedReconcilerWithSeeder(ctx, seeder)
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		Expect(got.Status.Seeders).To(ConsistOf(keziov1alpha2.PartitionContentSeederSite{
			Site: siteIdentity, MachineCount: 2,
		}))
	})

	It("reports one entry per Site with machineCount 1 each when one machine of each of two Sites demands the content", func() {
		siteXIdentity := mustCreateSeedingSite(ctx, "seed-subnet-9502x", "site-9502x")
		siteYIdentity := mustCreateSeedingSite(ctx, "seed-subnet-9502y", "site-9502y")
		mustCreateMachineSubnet(ctx, "machine-subnet-9502x", "site-9502x")
		mustCreateMachineSubnet(ctx, "machine-subnet-9502y", "site-9502y")

		contentName := "pc-" + partitionContentSeederSiteTestHash(2)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-9502", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		for _, m := range []*keziov1alpha2.Machine{
			newTestMachineOnSubnet("machine-9502x", img.Name, "machine-subnet-9502x"),
			newTestMachineOnSubnet("machine-9502y", img.Name, "machine-subnet-9502y"),
		} {
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		}

		seeder := ImageSeederConfig{Image: "example.test/kezio-seeder:test"}
		ir := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: seeder}
		_, err := ir.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: img.Name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		mustMarkDeploymentAvailable(ctx, seederdeploy.Name(img.Name, siteXIdentity))
		mustMarkDeploymentAvailable(ctx, seederdeploy.Name(img.Name, siteYIdentity))

		r, cancel := newIndexedReconcilerWithSeeder(ctx, seeder)
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, pcNN)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		Expect(got.Status.Seeders).To(ConsistOf(
			keziov1alpha2.PartitionContentSeederSite{Site: siteXIdentity, MachineCount: 1},
			keziov1alpha2.PartitionContentSeederSite{Site: siteYIdentity, MachineCount: 1},
		))
	})
})
