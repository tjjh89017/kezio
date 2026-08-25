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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

var _ = Describe("PartitionContent Controller seed demand from Machines and DeployRuns", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newDemandReconciler := func() (*PartitionContentReconciler, func()) {
		return newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image: "example.test/kezio-ingest:test",
		})
	}

	advanceToReady := func(r *PartitionContentReconciler, pc *keziov1alpha3.PartitionContent, hashHex string) {
		nn := types.NamespacedName{Name: pc.Name, Namespace: pc.Namespace}
		reconcileAddsFinalizer(ctx, r, nn)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		fakePublishJobSucceeded(ctx, pc, hashHex)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
	}

	It("holds SeederDegraded=True for a Machine's referenced Image while no seeder is available, and clears it once the Machine is deleted", func() {
		hashHex := partitionContentTestHash(500)
		name := partitionContentTestName(500)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		img := newTestImage("image-demand-500", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachine("machine-demand-500", img.Name)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r, cancel := newDemandReconciler()
		DeferCleanup(cancel)
		advanceToReady(r, pc, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// No ImageReconciler runs in this suite, so no Image ever owns a
		// seeder Deployment for image-demand-500: demand is real, but
		// nothing is available yet.
		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Status.Seeders).To(BeEmpty())

		Expect(k8sClient.Delete(ctx, machine)).To(Succeed())

		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			var got keziov1alpha3.PartitionContent
			g.Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
			g.Expect(degraded).To(BeNil(), "no demand must clear SeederDegraded rather than leave it True")
		}).Should(Succeed())
	})

	It("holds SeederDegraded=True for a non-terminal DeployRun's resolved Image and clears it once the run reaches a terminal phase", func() {
		hashHex := partitionContentTestHash(501)
		name := partitionContentTestName(501)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		img := newTestImage("image-demand-501", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		run := newTestDeployRun("run-demand-501", img.Name)
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

		r, cancel := newDemandReconciler()
		DeferCleanup(cancel)
		advanceToReady(r, pc, hashHex)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		degraded := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))

		run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		degraded = meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(degraded).To(BeNil(), "no demand must clear SeederDegraded rather than leave it True")
	})

	It("maps a Machine event to every PartitionContent its referenced Image's slots name", func() {
		img := newTestImage("image-demand-map", "pc-"+partitionContentTestHash(502))
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachine("machine-demand-map", img.Name)

		r, cancel := newDemandReconciler()
		DeferCleanup(cancel)

		requests := r.mapMachineToPartitionContents(ctx, machine)
		Expect(requests).To(ContainElement(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "pc-" + partitionContentTestHash(502)},
		}))
	})

	It("maps a DeployRun event to every PartitionContent its resolved Image's slots name", func() {
		img := newTestImage("image-demand-map-2", "pc-"+partitionContentTestHash(503))
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		run := newTestDeployRun("run-demand-map", img.Name)

		r, cancel := newDemandReconciler()
		DeferCleanup(cancel)

		requests := r.mapDeployRunToPartitionContents(ctx, run)
		Expect(requests).To(ContainElement(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "pc-" + partitionContentTestHash(503)},
		}))
	})
})
