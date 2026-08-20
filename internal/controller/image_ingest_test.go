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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// fakeIngestJobSucceeded fakes the ingest Job named ingestJobName(image)
// as succeeded with result as its termination message - envtest runs no
// real Job controller or kubelet, so this stands in for what a real
// ingest Job pod would report (see readIngestResult).
func fakeIngestJobSucceeded(ctx context.Context, image *keziov1alpha2.Image, result ingest.Result) {
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(image), Namespace: image.Namespace}, &job)).To(Succeed())
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

	data, err := ingest.MarshalResult(result)
	Expect(err).NotTo(HaveOccurred())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-pod",
			Namespace: image.Namespace,
			Labels:    map[string]string{"job-name": job.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "ingest", Image: "example.test/kezio-ingest:test"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "ingest",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Message: string(data)},
		},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// fakeIngestJobFailed fakes the ingest Job named ingestJobName(image) as
// terminally failed, the way a real Job controller would report it.
func fakeIngestJobFailed(ctx context.Context, image *keziov1alpha2.Image) {
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(image), Namespace: image.Namespace}, &job)).To(Succeed())
	job.Status.Failed = 1
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}

var _ = Describe("Image ingest Job orchestration", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("dispatches an ingest Job with the source env and a scratch work volume for a sourced Image", func() {
		img := newTestImageWithSlots("image-ingest-job-shape", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + imageTestHash(20)}},
		}, &keziov1alpha2.ImageSource{URL: "https://example.test/disk.img", Checksum: imageTestChecksum(20)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(img), Namespace: "default"}, &job)).To(Succeed())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal(img.Name))

		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal("example.test/kezio-ingest:test"))
		envByName := map[string]string{}
		for _, e := range container.Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["INGEST_MODE"]).To(Equal("ingest"))
		Expect(envByName["SOURCE_URL"]).To(Equal(img.Spec.Source.URL))
		Expect(envByName["SOURCE_CHECKSUM"]).To(Equal(img.Spec.Source.Checksum))
		Expect(envByName["WORK_DIR"]).To(Equal(ingest.DefaultWorkDir))
		Expect(envByName).NotTo(HaveKey("STAGING_ROOT"))

		Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "work", MountPath: ingest.DefaultWorkDir}))
		Expect(job.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ingestScratchPVCName(img.Name)))

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestScratchPVCName(img.Name), Namespace: "default"}, &pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Name).To(Equal(img.Name))
	})

	It("mounts the staging PVC and sets STAGING_ROOT for a kezio-staged:// source, once configured", func() {
		img := newTestImageWithSlots("image-ingest-staged-source", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + imageTestHash(21)}},
		}, &keziov1alpha2.ImageSource{URL: "kezio-staged://upload-1", Checksum: imageTestChecksum(21)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test", StagingPVCName: "imageservice-staging"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(img), Namespace: "default"}, &job)).To(Succeed())
		container := job.Spec.Template.Spec.Containers[0]
		envByName := map[string]string{}
		for _, e := range container.Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["STAGING_ROOT"]).To(Equal("/staging"))
		Expect(job.Spec.Template.Spec.Volumes).To(HaveLen(2))
	})

	It("holds a kezio-staged:// source at Pending with StagingUnconfigured when no staging PVC is configured", func() {
		img := newTestImageWithSlots("image-ingest-staged-unconfigured", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + imageTestHash(22)}},
		}, &keziov1alpha2.ImageSource{URL: "kezio-staged://upload-2", Checksum: imageTestChecksum(22)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStatePending))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("StagingUnconfigured"))

		var job batchv1.Job
		err = k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(img), Namespace: "default"}, &job)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("creates a PartitionContent per declared slot from a successful ingest result, then leads the Image to Ready", func() {
		hash1 := imageTestHash(23)
		hash2 := imageTestHash(24)
		img := newTestImageWithSlots("image-ingest-success", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleESP, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + hash1}},
			{Number: 2, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + hash2}},
			{Number: 3, Role: keziov1alpha2.PartitionRoleSwap, UUID: "11111111-1111-1111-1111-111111111111"},
		}, &keziov1alpha2.ImageSource{URL: "https://example.test/disk.img", Checksum: imageTestChecksum(23)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		result := ingest.Result{
			Success: true,
			Disk: &ingest.ResultDisk{
				SizeBytes:      1 << 30,
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				Partitions: []ingest.ResultPartition{
					{Number: 1, Role: keziov1alpha2.PartitionRoleESP, FSType: "vfat", UsedBytes: 1024, SizeBytes: 4096, LastExtentEnd: 2048, PieceLength: 16384, InfoHash: hash1},
					{Number: 2, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4", UsedBytes: 2048, SizeBytes: 8192, LastExtentEnd: 4096, PieceLength: 16384, InfoHash: hash2},
					{Number: 3, Role: keziov1alpha2.PartitionRoleSwap, UUID: "11111111-1111-1111-1111-111111111111"},
				},
			},
		}
		fakeIngestJobSucceeded(ctx, img, result)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var pc1, pc2 keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pc-" + hash1, Namespace: "default"}, &pc1)).To(Succeed())
		Expect(pc1.Spec.FSType).To(Equal("vfat"))
		Expect(pc1.Spec.Source.ImageName).To(Equal(img.Name))
		Expect(pc1.Spec.Source.PartitionNumber).To(Equal(int32(1)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pc-" + hash2, Namespace: "default"}, &pc2)).To(Succeed())
		Expect(pc2.Spec.FSType).To(Equal("ext4"))

		// No PartitionContent was created for the swap slot (no
		// ContentRef): drive both real contents to Ready and confirm the
		// Image reaches Ready with only its two declared contents.
		setContentStatus(ctx, &pc1, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc1.Generation)
		setContentStatus(ctx, &pc2, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", pc2.Generation)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateReady))
	})

	It("reuses an already-Ready PartitionContent by name instead of creating a duplicate", func() {
		hash := imageTestHash(25)

		img := newTestImageWithSlots("image-ingest-dedupe", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + hash}},
		}, &keziov1alpha2.ImageSource{URL: "https://example.test/disk.img", Checksum: imageTestChecksum(25)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		// First reconcile: no content exists yet, so the ingest Job gets
		// dispatched normally.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// Simulate the dedupe case: by the time ingest completes, a
		// PartitionContent with this exact hash already exists and is
		// Ready (e.g. produced by an earlier, unrelated ingest of
		// identical bytes) - completeIngest must reuse it untouched
		// rather than recreating or republishing it.
		existing := newTestPartitionContent("pc-" + hash)
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		setContentStatus(ctx, existing, keziov1alpha2.PartitionContentStateReady, metav1.ConditionTrue, "PublishJobSucceeded", "ready", existing.Generation)
		resourceVersionBefore := existing.ResourceVersion

		result := ingest.Result{
			Success: true,
			Disk: &ingest.ResultDisk{
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				Partitions: []ingest.ResultPartition{
					{Number: 1, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4", UsedBytes: 999, SizeBytes: 999, LastExtentEnd: 999, PieceLength: 16384, InfoHash: hash},
				},
			},
		}
		fakeIngestJobSucceeded(ctx, img, result)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var pc keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pc-" + hash, Namespace: "default"}, &pc)).To(Succeed())
		Expect(pc.ResourceVersion).To(Equal(resourceVersionBefore), "the existing Ready content must not be recreated or modified")

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateReady))
	})

	It("fails the Image, naming the slot, when the ingest result's info hash does not match the declared contentRef", func() {
		declaredHash := imageTestHash(26)
		actualHash := imageTestHash(27)
		img := newTestImageWithSlots("image-ingest-hash-mismatch", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + declaredHash}},
		}, &keziov1alpha2.ImageSource{URL: "https://example.test/disk.img", Checksum: imageTestChecksum(26)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		result := ingest.Result{
			Success: true,
			Disk: &ingest.ResultDisk{
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				Partitions: []ingest.ResultPartition{
					{Number: 1, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4", UsedBytes: 1, SizeBytes: 1, LastExtentEnd: 1, PieceLength: 16384, InfoHash: actualHash},
				},
			},
		}
		fakeIngestJobSucceeded(ctx, img, result)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("IngestFailed"))
		Expect(readyCond.Message).To(ContainSubstring("slot 1"))
		Expect(readyCond.Message).To(ContainSubstring("pc-" + declaredHash))

		var pc keziov1alpha2.PartitionContent
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "pc-" + declaredHash, Namespace: "default"}, &pc)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("fails the Image when the ingest Job terminally fails", func() {
		img := newTestImageWithSlots("image-ingest-job-failed", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: "pc-" + imageTestHash(28)}},
		}, &keziov1alpha2.ImageSource{URL: "https://example.test/disk.img", Checksum: imageTestChecksum(28)})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		fakeIngestJobFailed(ctx, img)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha2.ImageStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha2.ImageConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("IngestFailed"))
	})

	It("never creates an ingest Job for a composed Image with no spec.source", func() {
		contentName := "pc-" + imageTestHash(29)
		content := createReadyContent(ctx, contentName)
		Expect(content.Name).To(Equal(contentName))

		img := newTestImageWithSlots("image-composed-no-ingest", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		}, nil)
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		err = k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(img), Namespace: "default"}, &job)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
