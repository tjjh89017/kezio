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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

// partitionContentTestHash returns a distinct, valid-looking 40-character
// hex info hash for seq, so each test case gets its own PartitionContent
// name ("pc-" + hash) with no collisions in the shared envtest apiserver.
func partitionContentTestHash(seq int) string {
	return fmt.Sprintf("%040x", seq+1)
}

func newTestPartitionContent(name string) *keziov1alpha2.PartitionContent {
	return &keziov1alpha2.PartitionContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: keziov1alpha2.PartitionContentSpec{
			FSType:        "ext4",
			UsedBytes:     1024,
			SizeBytes:     2048,
			LastExtentEnd: 2048,
			PieceLength:   16384,
			Source: keziov1alpha2.PartitionContentSource{
				ImageName:       "image-a",
				PartitionNumber: 1,
			},
		},
	}
}

// reconcileAddsFinalizer drives one Reconcile call whose only job is
// adding PartitionContentFinalizer to a freshly created PartitionContent -
// the reconciler's first step (see Reconcile) before onChange ever runs.
// Tests call this once right after Create, so the "real" Reconcile calls
// that follow drive the publish/seeder walk exactly as the
// actually-finalized object would.
func reconcileAddsFinalizer(ctx context.Context, r *PartitionContentReconciler, nn types.NamespacedName) {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	Expect(err).NotTo(HaveOccurred())
	var pc keziov1alpha2.PartitionContent
	Expect(k8sClient.Get(ctx, nn, &pc)).To(Succeed())
	Expect(pc.Finalizers).To(ContainElement(keziov1alpha2.PartitionContentFinalizer))
}

// deletePartitionContent deletes pc and drives the reconciler's onDelete
// path once so PartitionContentFinalizer actually clears - a plain
// k8sClient.Delete alone would leave the object stuck with a deletion
// timestamp forever, since nothing else reconciles it in these tests. Safe
// to call on an already-deleted object (Delete/Reconcile both tolerate
// NotFound).
func deletePartitionContent(ctx context.Context, pc *keziov1alpha2.PartitionContent) {
	nn := types.NamespacedName{Name: pc.Name, Namespace: pc.Namespace}
	_ = k8sClient.Delete(ctx, pc)
	r := &PartitionContentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(16)}
	_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
}

var _ = Describe("PartitionContent Controller", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newReconciler := func(publish PartitionContentPublishConfig) *PartitionContentReconciler {
		return &PartitionContentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(16),
			Publish:  publish,
		}
	}

	It("creates an owner-referenced RWX content PVC sized from spec.sizeBytes", func() {
		hashHex := partitionContentTestHash(1)
		name := "pc-" + hashHex
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		r := newReconciler(PartitionContentPublishConfig{})
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, nn)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.PVCName(hash), Namespace: "default"}, &pvc)).To(Succeed())

		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteMany))
		wantSize := partitionContentPVCSize(pc.Spec.SizeBytes)
		got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(got.Cmp(wantSize)).To(Equal(0))

		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Name).To(Equal(name))
		Expect(pvc.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*pvc.OwnerReferences[0].Controller).To(BeTrue())
	})

	It("holds Pending with a condition and creates no Job when publish config is missing", func() {
		hashHex := partitionContentTestHash(2)
		name := "pc-" + hashHex
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		r := newReconciler(PartitionContentPublishConfig{})
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, nn)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.PartitionContentStatePending))
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.PartitionContentConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PublishConfigMissing"))

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var job batchv1.Job
		err = k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(hash), Namespace: "default"}, &job)
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err).To(HaveOccurred(), "no publish job should have been created")
	})

	It("creates a publish Job shaped correctly once publish config is set, and walks Pending->Publishing->Ready", func() {
		hashHex := partitionContentTestHash(3)
		name := "pc-" + hashHex
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		publish := PartitionContentPublishConfig{
			Image: "example.test/kezio-ingest:test",
		}
		// Indexed, not the local plain-client newReconciler: this test
		// drives the content all the way to Ready, where reconcileSeeder
		// (via resolveSeedDemand) lists Images through imageContentRefIndex
		// - a field selector a plain envtest client cannot serve.
		r, cancel := newIndexedReconciler(ctx, publish)
		DeferCleanup(cancel)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, nn)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(hash), Namespace: "default"}, &job)).To(Succeed())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal(name))

		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(publish.Image))
		// No Site is in scope at publish time, so no announce-bearing
		// setting is ever handed to the publish Job.
		for _, e := range container.Env {
			Expect(e.Name).NotTo(Equal("TRACKER_URL"))
		}
		Expect(container.VolumeMounts).To(HaveLen(2))
		Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "content",
			MountPath: ingest.ContentMountPath(hash),
		}))
		Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "scratch",
			MountPath: ingest.DefaultWorkDir,
			ReadOnly:  true,
		}))

		var afterCreate keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &afterCreate)).To(Succeed())
		Expect(afterCreate.Status.State).To(Equal(keziov1alpha2.PartitionContentStatePublishing))
		Expect(afterCreate.Status.PVCRef).NotTo(BeNil())
		Expect(afterCreate.Status.PVCRef.Name).To(Equal(store.PVCName(hash)))

		// A second reconcile with the Job still running must not create a
		// second Job and must stay Publishing.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace("default"), client.MatchingLabels{partitionContentAppComponentLabel: partitionContentJobComponentValue})).To(Succeed())
		count := 0
		for _, j := range jobs.Items {
			if len(j.OwnerReferences) > 0 && j.OwnerReferences[0].Name == name {
				count++
			}
		}
		Expect(count).To(Equal(1))

		// The reconciler cannot literally write a .torrent in envtest (no
		// real Job pod runs); it trusts the Job's own status instead - fake
		// that here the way a real Job controller would report success.
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// publish carries no tracker setting at all (there is none to
		// carry - see PartitionContentPublishConfig's doc comment), and
		// this reconciler still reaches Ready: content readiness never
		// depended on a tracker being configured anywhere.
		var ready keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &ready)).To(Succeed())
		Expect(ready.Status.State).To(Equal(keziov1alpha2.PartitionContentStateReady))
		readyCond := meta.FindStatusCondition(ready.Status.Conditions, keziov1alpha2.PartitionContentConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		// Valid is trivially True on every reconcile (spec is
		// CEL-validated at admission - see setPartitionContentValidCondition),
		// but it must still be written and fresh: a reader applying the
		// cross-reference contract needs a current observedGeneration to
		// check.
		validCond := meta.FindStatusCondition(ready.Status.Conditions, keziov1alpha2.PartitionContentConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(validCond.ObservedGeneration).To(Equal(ready.Generation))

		// Already-Ready reconcile is a no-op: it must not create a second
		// publish Job for this content hash.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace("default"), client.MatchingLabels{partitionContentAppComponentLabel: partitionContentJobComponentValue})).To(Succeed())
		count = 0
		for _, j := range jobs.Items {
			if len(j.OwnerReferences) > 0 && j.OwnerReferences[0].Name == name {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})

	It("records Failed with a Ready=False condition on a terminal Job failure", func() {
		hashHex := partitionContentTestHash(4)
		name := "pc-" + hashHex
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		DeferCleanup(func() { deletePartitionContent(ctx, pc) })

		publish := PartitionContentPublishConfig{
			Image: "example.test/kezio-ingest:test",
		}
		r := newReconciler(publish)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, r, nn)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hash, err := store.ParseInfoHash(hashHex)
		Expect(err).NotTo(HaveOccurred())
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(hash), Namespace: "default"}, &job)).To(Succeed())
		job.Status.Failed = 1
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var failed keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &failed)).To(Succeed())
		Expect(failed.Status.State).To(Equal(keziov1alpha2.PartitionContentStateFailed))
		cond := meta.FindStatusCondition(failed.Status.Conditions, keziov1alpha2.PartitionContentConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PublishJobFailed"))
	})
})
