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
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

var _ = Describe("PartitionContent Controller seeder Subnet placement", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newPlacementReconciler := func() *PartitionContentReconciler {
		return &PartitionContentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(16),
			Publish: PartitionContentPublishConfig{
				Image:      "example.test/kezio-ingest:test",
				TrackerURL: "http://tracker.example.test/announce",
			},
			Seeder: PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"},
		}
	}

	// advanceToReadyWithSeederDeployment drives nn through Pending ->
	// Publishing -> Ready and one further Reconcile so reconcileSeeder
	// creates the seeder Deployment, mirroring
	// partitioncontent_seeder_test.go's advancePartitionContentToReady
	// plus the caller's own follow-up Reconcile there.
	advanceToReadyWithSeederDeployment := func(r *PartitionContentReconciler, nn types.NamespacedName, hashHex string) {
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

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
	}

	It("builds the seeder Deployment with no Multus annotation when no Subnet resolves placement in the namespace, then adds the annotation and nodeSelector once a Subnet with seederNetworkRef appears", func() {
		ns := createSubnetTestNamespace(ctx)

		hashHex := partitionContentTestHash(900)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: ns}
		pc := newTestPartitionContent(name)
		pc.Namespace = ns
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{
			Image:      "example.test/kezio-ingest:test",
			TrackerURL: "http://tracker.example.test/announce",
		}, PartitionContentSeederConfig{Image: "example.test/kezio-seeder:test"})
		DeferCleanup(cancel)

		img := newTestImage("image-"+hashHex, name)
		img.Namespace = ns
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachine("machine-"+hashHex, img.Name)
		machine.Namespace = ns
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, machine) })

		advanceToReadyWithSeederDeployment(r, nn, hashHex)

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		depKey := types.NamespacedName{Name: seederdeploy.Name(hash), Namespace: ns}

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(multusDefaultNetworkAnnotation))
		Expect(dep.Spec.Template.Spec.NodeSelector).To(BeEmpty())
		Expect(dep.Labels).NotTo(HaveKey(partitionContentSeederSubnetLabel))

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
			s.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "node-1"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, subnet)).To(Succeed()) })

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations[multusDefaultNetworkAnnotation]).To(Equal(ns + "/seeder-nad"))
		Expect(dep.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"kubernetes.io/hostname": "node-1"}))
		Expect(dep.Labels[partitionContentSeederSubnetLabel]).To(Equal(testSubnetName))
		Expect(dep.Spec.Selector.MatchLabels).NotTo(HaveKey(partitionContentSeederSubnetLabel))
	})

	It("maps a Subnet event to every PartitionContent in the Subnet's own namespace", func() {
		ns := createSubnetTestNamespace(ctx)
		other := "default"

		pcA := newTestPartitionContent("pc-" + partitionContentTestHash(901))
		pcA.Namespace = ns
		Expect(k8sClient.Create(ctx, pcA)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pcA) })

		pcB := newTestPartitionContent("pc-" + partitionContentTestHash(902))
		pcB.Namespace = other
		Expect(k8sClient.Create(ctx, pcB)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pcB) })

		subnet := testSubnet(ns)
		r := newPlacementReconciler()

		requests := r.mapSubnetToPartitionContents(ctx, subnet)
		names := make([]string, 0, len(requests))
		for _, req := range requests {
			Expect(req.Namespace).To(Equal(ns))
			names = append(names, req.Name)
		}
		Expect(names).To(ConsistOf(pcA.Name))
	})
})
