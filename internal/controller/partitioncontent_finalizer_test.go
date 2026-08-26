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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// newTestImage builds an (uncreated) Image with a single data slot bound
// to contentName, in the "default" namespace.
func newTestImage(name, contentName string) *keziov1alpha3.Image {
	return &keziov1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha3.ImageSpec{
			Layout: keziov1alpha3.ImageDiskLayout{
				PartitionTable: keziov1alpha3.PartitionTableGPT,
				SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
				Slots: []keziov1alpha3.ImageSlot{
					{Number: 1, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: contentName}},
				},
			},
		},
	}
}

// newTestClaim builds an (uncreated) MachineClaim whose spec.imageRef
// names imageName, in the "default" namespace - a minimal seed-demand
// source for resolveSeedDemand. Nothing in this suite runs
// MachineReconciler or the MachineClaim webhook, so machineName need not
// resolve to a real Machine.
func newTestClaim(name, imageName string) *keziov1alpha3.MachineClaim {
	return &keziov1alpha3.MachineClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha3.MachineClaimSpec{
			MachineName: name + "-machine",
			ImageRef:    &keziov1alpha3.NameRef{Name: imageName},
		},
	}
}

// newTestDeployRun builds an (uncreated) DeployRun whose resolved snapshot
// names imageName, in the "default" namespace. Status.Phase is left empty
// - an active phase (see isDeployRunActive). spec.machineRef is required
// by the schema but not meaningful to any caller, so it is a fixed
// placeholder rather than a parameter.
func newTestDeployRun(name, imageName string) *keziov1alpha3.DeployRun {
	return &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha3.DeployRunSpec{
			MachineRef: keziov1alpha3.NameRef{Name: "machine-a"},
			ImageRef:   &keziov1alpha3.NameRef{Name: imageName},
		},
	}
}

// newIndexedReconciler returns a PartitionContentReconciler whose Client
// resolves Image and Machine reads (Get and List) through a real,
// index-backed cache - the same mechanism SetupWithManager wires in
// production - so imagesReferencing and resolveSeedDemand exercise the
// actual field-selector List rather than a full scan. Every other type is
// read directly (client.CacheOptions.DisableFor): those stay on the plain
// envtest client so a status write this test just made is visible on the
// very next Get, with no cache-sync lag to race against.
func newIndexedReconciler(ctx context.Context, publish PartitionContentPublishConfig) (*PartitionContentReconciler, func()) {
	c, err := cache.New(cfg, cache.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	Expect(c.IndexField(ctx, &keziov1alpha3.Image{}, imageContentRefIndex, indexImageContentRefs)).To(Succeed())
	Expect(c.IndexField(ctx, &keziov1alpha3.MachineClaim{}, claimImageRefIndex, indexClaimImageRefs)).To(Succeed())

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
		Publish:  publish,
	}
	return r, cancel
}

var _ = Describe("PartitionContent Controller deletion-blocking finalizer", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("keeps the finalizer and reports the blocking Image, by name, while a slot still references the content", func() {
		hashHex := partitionContentTestHash(200)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		img := newTestImage("image-blocking-1", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(keziov1alpha3.PartitionContentFinalizer))

		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionDeletionBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("image/" + img.Name))

		Eventually(r.Recorder.(*record.FakeRecorder).Events).Should(Receive(ContainSubstring(img.Name)))
	})

	It("blocks while an active DeployRun's resolved snapshot names an Image that still references the content", func() {
		hashHex := partitionContentTestHash(201)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		img := newTestImage("image-blocking-2", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		run := newTestDeployRun("run-blocking-1", img.Name)
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(keziov1alpha3.PartitionContentFinalizer))
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionDeletionBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("deployrun/" + run.Name))
	})

	It("does not block on an active DeployRun once its referenced Image no longer exists", func() {
		hashHex := partitionContentTestHash(202)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		// run.spec.imageRef names an Image that is never created: the
		// deleted-Image edge case documented on activeDeployRunsReferencing.
		run := newTestDeployRun("run-orphaned-1", "image-does-not-exist")
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, nn, &keziov1alpha3.PartitionContent{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an unresolvable Image reference on an active run must not block deletion")
	})

	It("does not block on a terminal DeployRun", func() {
		hashHex := partitionContentTestHash(203)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		img := newTestImage("image-terminal-run", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		run := newTestDeployRun("run-terminal-1", img.Name)
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		run.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

		// The Image itself still references the content, so deletion stays
		// blocked - by the Image, not the (now terminal) DeployRun.
		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.PartitionContentConditionDeletionBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("image/" + img.Name))
		Expect(cond.Message).NotTo(ContainSubstring("deployrun/" + run.Name))
	})

	It("unblocks and actually removes the content once the last referencing Image is deleted", func() {
		hashHex := partitionContentTestHash(204)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		img := newTestImage("image-unblocks-1", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var stillBlocked keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &stillBlocked)).To(Succeed())
		Expect(stillBlocked.Finalizers).To(ContainElement(keziov1alpha3.PartitionContentFinalizer))

		Expect(k8sClient.Delete(ctx, img)).To(Succeed())

		// mapImageToPartitionContents is exactly the mapping the Image
		// watch registered in SetupWithManager uses to enqueue this
		// re-check on the Image's Delete event - checked directly here
		// since these tests drive Reconcile manually rather than running a
		// live manager/watch loop (see suite_test.go).
		requests := r.mapImageToPartitionContents(ctx, img)
		Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: nn}))

		Eventually(func() error {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			if err != nil {
				return err
			}
			return k8sClient.Get(ctx, nn, &keziov1alpha3.PartitionContent{})
		}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"))
	})

	It("keeps reporting seed demand status while deletion is blocked", func() {
		hashHex := partitionContentTestHash(205)
		name := partitionContentTestName(205)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		publish := PartitionContentPublishConfig{
			Image: "example.test/kezio-ingest:test",
		}
		r, cancel := newIndexedReconciler(ctx, publish)
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		// image-blocks-seeded-content both blocks pc's deletion below and,
		// through the MachineClaim created next, is the seed-demand source
		// that must outlive that block.
		img := newTestImage("image-blocks-seeded-content", name)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		claim := newTestClaim("machine-demands-"+hashHex, img.Name)
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakePublishJobSucceeded(ctx, pc, hashHex)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn}) // -> Ready
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn}) // -> reflects seed demand
		Expect(err).NotTo(HaveOccurred())

		var readyGot keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &readyGot)).To(Succeed())
		degraded := meta.FindStatusCondition(readyGot.Status.Conditions, keziov1alpha3.PartitionContentConditionSeederDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue), "demand exists but ImageReconciler owns no seeder Deployment in this suite")

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// onDelete returns before ever reaching onChange/reconcileSeeder:
		// a blocked deletion must not itself change the seed-demand status
		// reconcileSeeder would otherwise still be reflecting.
		var got keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(keziov1alpha3.PartitionContentFinalizer))
	})

	It("deletes its own publish Job outright rather than waiting on the Job's TTL", func() {
		name := partitionContentTestName(207)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		publish := PartitionContentPublishConfig{Image: "example.test/kezio-ingest:test"}
		r, cancel := newIndexedReconciler(ctx, publish)
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(name), Namespace: "default"}, &job)).To(Succeed())

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, nn, &keziov1alpha3.PartitionContent{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the content itself must be gone once nothing blocks it")

		err = k8sClient.Get(ctx, types.NamespacedName{Name: publishJobName(name), Namespace: "default"}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the publish Job must be deleted immediately, not left for its TTLSecondsAfterFinished")
	})

	It("removes the finalizer and the content disappears promptly when nothing references it", func() {
		hashHex := partitionContentTestHash(206)
		name := "pc-" + hashHex
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		pc := newTestPartitionContent(name)
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		r, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		reconcileAddsFinalizer(ctx, r, nn)

		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, nn, &keziov1alpha3.PartitionContent{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})

func TestMapImageToPartitionContentsDeduplicatesSlotsAndSkipsBlankOnes(t *testing.T) {
	r := &PartitionContentReconciler{}
	img := &keziov1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "image-a", Namespace: "ns"},
		Spec: keziov1alpha3.ImageSpec{
			Layout: keziov1alpha3.ImageDiskLayout{
				Slots: []keziov1alpha3.ImageSlot{
					{Number: 1, Role: keziov1alpha3.PartitionRoleESP, FSType: "vfat"},
					{Number: 2, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: "pc-a"}},
					{Number: 3, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: "pc-a"}},
					{Number: 4, Role: keziov1alpha3.PartitionRoleData, ContentRef: &keziov1alpha3.NameRef{Name: "pc-b"}},
				},
			},
		},
	}

	got := r.mapImageToPartitionContents(context.Background(), img)

	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "pc-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "pc-b"}},
	}
	if len(got) != len(want) {
		t.Fatalf("mapImageToPartitionContents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapImageToPartitionContents()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFormatBlockers(t *testing.T) {
	cases := []struct {
		name     string
		blockers []string
		want     string
	}{
		{
			name:     "empty input",
			blockers: nil,
			want:     "",
		},
		{
			name:     "under limit joins verbatim",
			blockers: []string{"image/a", "image/b"},
			want:     "image/a, image/b",
		},
		{
			name:     "exactly at limit joins verbatim",
			blockers: []string{"image/a", "image/b", "image/c", "image/d", "image/e"},
			want:     "image/a, image/b, image/c, image/d, image/e",
		},
		{
			name:     "over limit names first N and counts the rest",
			blockers: []string{"image/a", "image/b", "image/c", "image/d", "image/e", "image/f", "image/g"},
			want:     "image/a, image/b, image/c, image/d, image/e, and 2 more",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatBlockers(tc.blockers)
			if got != tc.want {
				t.Errorf("formatBlockers(%v) = %q, want %q", tc.blockers, got, tc.want)
			}
		})
	}
}
