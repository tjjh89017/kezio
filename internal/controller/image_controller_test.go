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
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageTestHash returns a distinct, valid-looking 40-character hex info
// hash for seq, so each test case gets its own PartitionContent name
// ("pc-" + hash) with no collisions in the shared envtest apiserver -
// mirrors partitionContentTestHash but keeps this file's sequence
// independent from partitioncontent_controller_test.go's.
func imageTestHash(seq int) string {
	return fmt.Sprintf("%040x", seq+1000)
}

// newTestImageWithSlots builds an (uncreated) Image with the given slots.
func newTestImageWithSlots(name string, slots []keziov1alpha2.ImageSlot) *keziov1alpha2.Image {
	return &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha2.ImageSpec{
			Layout: keziov1alpha2.ImageDiskLayout{
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
				Slots:          slots,
			},
		},
	}
}

// createReadyContent creates a PartitionContent and drives its status
// straight to Ready with a fresh (matching-generation) Ready condition,
// without running PartitionContentReconciler - this file only exercises
// ImageReconciler, which never writes PartitionContent.
func createReadyContent(ctx context.Context, name string) *keziov1alpha2.PartitionContent {
	pc := newTestPartitionContent(name)
	Expect(k8sClient.Create(ctx, pc)).To(Succeed())
	setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc.Generation)
	// A Ready content always carries the info hash its publish Job
	// reported; the seeder placement path skips one that does not.
	sum := sha1.Sum([]byte(name))
	setContentInfoHash(ctx, pc, hex.EncodeToString(sum[:]))
	return pc
}

// setContentInfoHash stamps status.infoHash on pc, the way recordReady
// does from a real publish Job's reported result.
func setContentInfoHash(ctx context.Context, pc *keziov1alpha2.PartitionContent, infoHash string) {
	var fresh keziov1alpha2.PartitionContent
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pc.Name, Namespace: pc.Namespace}, &fresh)).To(Succeed())
	fresh.Status.InfoHash = infoHash
	Expect(k8sClient.Status().Update(ctx, &fresh)).To(Succeed())
	*pc = fresh
}

// setContentStatus overwrites pc's status (State plus a Ready condition
// stamped with the given observedGeneration) via a direct status Update -
// observedGeneration is a test-controlled parameter so callers can
// construct a stale condition (observedGeneration != pc.Generation) to
// exercise the cross-reference contract.
func setContentStatus(ctx context.Context, pc *keziov1alpha2.PartitionContent, state string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) {
	var fresh keziov1alpha2.PartitionContent
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pc.Name, Namespace: pc.Namespace}, &fresh)).To(Succeed())
	fresh.Status.State = state
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PartitionContentConditionReady,
		Status:             status,
		ObservedGeneration: observedGeneration,
		Reason:             reason,
		Message:            message,
	})
	Expect(k8sClient.Status().Update(ctx, &fresh)).To(Succeed())
	*pc = fresh
}

// newIndexedImageReconciler returns an ImageReconciler whose Client
// resolves Image reads (Get and List) through a real, index-backed cache -
// the same mechanism SetupWithManager wires in production - so
// mapPartitionContentToImages exercises the actual field-selector List
// rather than a full scan, mirroring newIndexedReconciler.
func newIndexedImageReconciler(ctx context.Context) (*ImageReconciler, func()) {
	c, err := cache.New(cfg, cache.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	Expect(c.IndexField(ctx, &keziov1alpha2.Image{}, imageContentRefIndex, indexImageContentRefs)).To(Succeed())

	cacheCtx, cancel := context.WithCancel(ctx)
	go func() { _ = c.Start(cacheCtx) }()
	Expect(c.WaitForCacheSync(cacheCtx)).To(BeTrue())

	cl, err := client.New(cfg, client.Options{
		Scheme: k8sClient.Scheme(),
		Cache: &client.CacheOptions{
			Reader: c,
			DisableFor: []client.Object{
				&keziov1alpha2.PartitionContent{},
				&keziov1alpha2.DeployRun{},
				&corev1.PersistentVolumeClaim{},
				&batchv1.Job{},
				&appsv1.Deployment{},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())

	r := &ImageReconciler{Client: cl, Scheme: k8sClient.Scheme()}
	return r, cancel
}

var _ = Describe("Image Controller", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("becomes Ready when every referenced content is Ready, ignoring blank and swap slots", func() {
		contentName := "pc-" + imageTestHash(1)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-composed-ready", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"},
			{Number: 3, Role: keziov1alpha2.PartitionRoleSwap, UUID: "11111111-1111-1111-1111-111111111111"},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateReady))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		validCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("stays not Ready and names the content when it is not yet Ready", func() {
		contentName := "pc-" + imageTestHash(2)
		pc := newTestPartitionContent(contentName)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStatePublishing, metav1.ConditionFalse, "Publishing", "publish job is running", pc.Generation)

		img := newTestImageWithSlots("image-content-not-ready", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStatePending))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("ContentNotReady"))
		Expect(readyCond.Message).To(ContainSubstring(contentName))
	})

	It("stays not Ready when the referenced content does not exist", func() {
		img := newTestImageWithSlots("image-content-missing", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + imageTestHash(3)}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStatePending))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Message).To(ContainSubstring("not found"))
	})

	It("requeues without writing status when a referenced content's Ready condition is stale", func() {
		contentName := "pc-" + imageTestHash(4)
		pc := newTestPartitionContent(contentName)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		// Simulate staleness directly: a Ready=True condition whose
		// observedGeneration does not match the content's current
		// generation (spec is immutable, so this cannot arise from a
		// real spec update - see the cross-reference contract).
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc.Generation+1)

		img := newTestImageWithSlots("image-stale-content", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(BeEmpty())
		Expect(got.Status.Conditions).To(BeEmpty())
	})

	It("sets Valid=False and keeps Ready=False when a slot's sizeBytes is smaller than its content's lastExtentEnd, for an Image admitted before the content existed", func() {
		contentName := "pc-" + imageTestHash(7)

		img := newTestImageWithSlots("image-invalid-size", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}, SizeBytes: 1024},
		})
		// Created while the content does not exist yet - the webhook
		// (not exercised by this envtest suite) would only warn, not
		// deny, in that situation; the reconciler re-derives Valid once
		// the content exists.
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		pc := newTestPartitionContent(contentName)
		pc.Spec.LastExtentEnd = 4096
		pc.Spec.SizeBytes = 4096
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc.Generation)

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStatePending))
		validCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Message).To(ContainSubstring("1024"))
		Expect(validCond.Message).To(ContainSubstring("4096"))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	It("becomes Failed and names the content when a referenced content is terminally Failed", func() {
		contentName := "pc-" + imageTestHash(9)
		pc := newTestPartitionContent(contentName)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStateFailed, metav1.ConditionFalse, "PublishJobFailed", "publish job failed", pc.Generation)

		img := newTestImageWithSlots("image-content-failed", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("ContentFailed"))
		Expect(readyCond.Message).To(ContainSubstring(contentName))
	})

	It("records Failed when one slot's content is Failed alongside another that is only not-ready, pinning failed's precedence over notReady", func() {
		failedName := "pc-" + imageTestHash(10)
		failedPC := newTestPartitionContent(failedName)
		Expect(k8sClient.Create(ctx, failedPC)).To(Succeed())
		setContentStatus(ctx, failedPC, keziov1alpha2.PartitionContentStateFailed, metav1.ConditionFalse, "PublishJobFailed", "publish job failed", failedPC.Generation)

		notReadyName := "pc-" + imageTestHash(11)
		notReadyPC := newTestPartitionContent(notReadyName)
		Expect(k8sClient.Create(ctx, notReadyPC)).To(Succeed())
		setContentStatus(ctx, notReadyPC, keziov1alpha2.PartitionContentStatePublishing, metav1.ConditionFalse, "Publishing", "publish job is running", notReadyPC.Generation)

		img := newTestImageWithSlots("image-content-failed-and-not-ready", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: failedName}},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: notReadyName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("ContentFailed"))
		Expect(readyCond.Message).To(ContainSubstring(failedName))
	})

	It("propagates a content flipping to Ready onto the Image that references it, via the reverse index watch mapping", func() {
		contentName := "pc-" + imageTestHash(8)
		pc := newTestPartitionContent(contentName)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStatePublishing, metav1.ConditionFalse, "Publishing", "publish job is running", pc.Generation)

		img := newTestImageWithSlots("image-watch-propagation", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r, cancel := newIndexedImageReconciler(ctx)
		DeferCleanup(cancel)
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var pending keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &pending)).To(Succeed())
		Expect(pending.Status.State).To(Equal(keziov1alpha2.ImageStatePending))

		// The mapping function is what SetupWithManager wires as the
		// PartitionContent watch's handler; exercise it directly against
		// the now-Ready content and confirm it names this Image.
		setContentStatus(ctx, pc, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc.Generation)
		Eventually(func() []reconcile.Request {
			return r.mapPartitionContentToImages(ctx, pc)
		}).Should(ContainElement(reconcile.Request{NamespacedName: nn}))

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var ready keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &ready)).To(Succeed())
		Expect(ready.Status.State).To(Equal(keziov1alpha2.ImageStateReady))
	})
})
