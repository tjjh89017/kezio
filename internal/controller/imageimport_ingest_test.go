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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

// importTestChecksum returns a distinct, valid-looking "sha256:<64 hex>"
// checksum for seq.
func importTestChecksum(seq int) string {
	return fmt.Sprintf("sha256:%064x", seq+3000)
}

// newTestImageImport builds an (uncreated) ImageImport whose created Image
// and content names are both derived from name, the way `kezioctl image
// upload` defaults them.
func newTestImageImport(name, sourceURL string, seq int) *keziov1alpha3.ImageImport {
	return &keziov1alpha3.ImageImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha3.ImageImportSpec{
			Source:        keziov1alpha3.ImportSource{URL: sourceURL, Checksum: importTestChecksum(seq)},
			ImageName:     name + "-image",
			ContentPrefix: name,
		},
	}
}

// diskResult is a successful two-content-plus-swap ingest Result for imp's
// source disk, the shape a real ingest Job reports.
func diskResult() ingest.Result {
	return ingest.Result{
		Success: true,
		Disk: &ingest.ResultDisk{
			SizeBytes:      1 << 30,
			PartitionTable: keziov1alpha3.PartitionTableGPT,
			SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
			Partitions: []ingest.ResultPartition{
				{Number: 1, Role: keziov1alpha3.PartitionRoleESP, FSType: "vfat", UsedBytes: 1024, SizeBytes: 4096, LastExtentEnd: 2048, PieceLength: 16384, TypeGUID: "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"},
				{Number: 2, Role: keziov1alpha3.PartitionRoleData, FSType: "ext4", UsedBytes: 2048, SizeBytes: 8192, LastExtentEnd: 4096, PieceLength: 16384},
				{Number: 3, Role: keziov1alpha3.PartitionRoleSwap, UUID: "11111111-1111-1111-1111-111111111111"},
			},
		},
	}
}

// fakeIngestJobSucceeded fakes imp's ingest Job as succeeded with result
// as its termination message - envtest runs no real Job controller or
// kubelet, so this stands in for what a real ingest Job pod would report
// (see readJobResult).
func fakeIngestJobSucceeded(ctx context.Context, imp *keziov1alpha3.ImageImport, result ingest.Result) {
	data, err := ingest.MarshalResult(result)
	Expect(err).NotTo(HaveOccurred())
	fakeIngestJobSucceededWithMessage(ctx, imp, string(data))
}

// fakeIngestJobSucceededWithMessage fakes imp's ingest Job as succeeded,
// with a pod whose container termination message is the raw string
// message - used to exercise readJobResult's error path for a malformed
// or non-JSON message, which no real ingest binary would write but a
// corrupted transport could.
func fakeIngestJobSucceededWithMessage(ctx context.Context, imp *keziov1alpha3.ImageImport, message string) {
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: imp.Namespace}, &job)).To(Succeed())
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-pod",
			Namespace: imp.Namespace,
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
			Terminated: &corev1.ContainerStateTerminated{Message: message},
		},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// fakeIngestJobSucceededNoPod fakes imp's ingest Job as succeeded with no
// pod at all - readJobResult must still fail cleanly rather than leave the
// import stuck.
func fakeIngestJobSucceededNoPod(ctx context.Context, imp *keziov1alpha3.ImageImport) {
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: imp.Namespace}, &job)).To(Succeed())
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}

// fakeIngestJobFailed fakes imp's ingest Job as terminally failed, the way
// a real Job controller would report it.
func fakeIngestJobFailed(ctx context.Context, imp *keziov1alpha3.ImageImport) {
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: imp.Namespace}, &job)).To(Succeed())
	job.Status.Failed = 1
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}

var _ = Describe("ImageImport Controller", func() {
	var ctx context.Context
	var r *ImageImportReconciler

	BeforeEach(func() {
		ctx = context.Background()
		r = &ImageImportReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Ingest: ImageIngestConfig{Image: "example.test/kezio-ingest:test"},
		}
	})

	// createImport creates imp, registers its cleanup, and returns the key
	// every reconcile in these specs runs against.
	createImport := func(imp *keziov1alpha3.ImageImport) types.NamespacedName {
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, imp) })
		return types.NamespacedName{Name: imp.Name, Namespace: imp.Namespace}
	}

	It("holds an import at Pending with IngestUnconfigured when no ingest image is configured", func() {
		imp := newTestImageImport("import-unconfigured", "https://example.test/disk.img", 1)
		nn := createImport(imp)

		unconfigured := &ImageImportReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := unconfigured.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStatePending))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("IngestUnconfigured"))
	})

	It("dispatches an ingest Job with the source env and a scratch work volume", func() {
		imp := newTestImageImport("import-job-shape", "https://example.test/disk.img", 2)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: "default"}, &job)).To(Succeed())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal(imp.Name))

		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal("example.test/kezio-ingest:test"))
		envByName := map[string]string{}
		for _, e := range container.Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["INGEST_MODE"]).To(Equal("ingest"))
		Expect(envByName["SOURCE_URL"]).To(Equal(imp.Spec.Source.URL))
		Expect(envByName["SOURCE_CHECKSUM"]).To(Equal(imp.Spec.Source.Checksum))
		Expect(envByName["WORK_DIR"]).To(Equal(ingest.DefaultWorkDir))
		Expect(envByName).NotTo(HaveKey("STAGING_ROOT"))

		Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "work", MountPath: ingest.DefaultWorkDir}))
		Expect(job.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ingestScratchPVCName(imp.Name)))

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestScratchPVCName(imp.Name), Namespace: "default"}, &pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Name).To(Equal(imp.Name))
	})

	It("mounts the staging PVC and sets STAGING_ROOT for a kezio-staged:// source, once configured", func() {
		imp := newTestImageImport("import-staged-source", "kezio-staged://upload-1", 3)
		nn := createImport(imp)

		r.Ingest.StagingPVCName = "imageservice-staging"
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: "default"}, &job)).To(Succeed())
		envByName := map[string]string{}
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["STAGING_ROOT"]).To(Equal("/staging"))
		Expect(job.Spec.Template.Spec.Volumes).To(HaveLen(2))
	})

	It("holds a kezio-staged:// source at Pending with StagingUnconfigured when no staging PVC is configured", func() {
		imp := newTestImageImport("import-staged-unconfigured", "kezio-staged://upload-2", 4)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStatePending))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("StagingUnconfigured"))

		var job batchv1.Job
		err = k8sClient.Get(ctx, types.NamespacedName{Name: ingestJobName(imp), Namespace: "default"}, &job)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("creates one PartitionContent per non-swap partition and the Image binding them", func() {
		imp := newTestImageImport("import-success", "https://example.test/disk.img", 5)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceeded(ctx, imp, diskResult())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var pc1, pc2 keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.ContentName(imp.Spec.ContentPrefix, 1), Namespace: "default"}, &pc1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &pc1) })
		Expect(pc1.Spec.FSType).To(Equal("vfat"))
		Expect(pc1.Spec.Source.ImportName).To(Equal(imp.Name))
		Expect(pc1.Spec.Source.PartitionNumber).To(Equal(int32(1)))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.ContentName(imp.Spec.ContentPrefix, 2), Namespace: "default"}, &pc2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &pc2) })
		Expect(pc2.Spec.FSType).To(Equal("ext4"))

		// The swap partition carries no content.
		var swapPC keziov1alpha3.PartitionContent
		err = k8sClient.Get(ctx, types.NamespacedName{Name: store.ContentName(imp.Spec.ContentPrefix, 3), Namespace: "default"}, &swapPC)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		var img keziov1alpha3.Image
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: imp.Spec.ImageName, Namespace: "default"}, &img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &img) })
		Expect(img.Spec.Layout.PartitionTable).To(Equal(keziov1alpha3.PartitionTableGPT))
		Expect(img.Spec.Layout.SfdiskJSON).To(Equal(`{"partitiontable":{"label":"gpt"}}`))
		Expect(img.Spec.Layout.Slots).To(HaveLen(3))
		Expect(img.Spec.Layout.Slots[0].Role).To(Equal(keziov1alpha3.PartitionRoleESP))
		Expect(img.Spec.Layout.Slots[0].ContentRef.Name).To(Equal(pc1.Name))
		Expect(img.Spec.Layout.Slots[0].TypeGUID).To(Equal("c12a7328-f81f-11d2-ba4b-00a0c93ec93b"))
		Expect(img.Spec.Layout.Slots[2].Role).To(Equal(keziov1alpha3.PartitionRoleSwap))
		Expect(img.Spec.Layout.Slots[2].ContentRef).To(BeNil())
		Expect(img.Spec.Layout.Slots[2].UUID).To(Equal("11111111-1111-1111-1111-111111111111"))

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateReady))
		Expect(got.Status.ImageRef.Name).To(Equal(imp.Spec.ImageName))
		Expect(got.Status.ContentRefs).To(HaveLen(2))
	})

	It("fails the import rather than writing over a PartitionContent name another import holds", func() {
		imp := newTestImageImport("import-content-taken", "https://example.test/disk.img", 6)
		nn := createImport(imp)

		squatter := newTestPartitionContent(store.ContentName(imp.Spec.ContentPrefix, 1))
		squatter.Spec.Source.ImportName = "some-other-import"
		Expect(k8sClient.Create(ctx, squatter)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, squatter) })
		resourceVersionBefore := squatter.ResourceVersion

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceeded(ctx, imp, diskResult())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Message).To(ContainSubstring(squatter.Name))
		Expect(readyCond.Message).To(ContainSubstring("immutable"))

		var after keziov1alpha3.PartitionContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: squatter.Name, Namespace: "default"}, &after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(resourceVersionBefore), "the existing content must not be modified")

		var img keziov1alpha3.Image
		err = k8sClient.Get(ctx, types.NamespacedName{Name: imp.Spec.ImageName, Namespace: "default"}, &img)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("fails the import rather than writing over an Image name it did not create", func() {
		imp := newTestImageImport("import-image-taken", "https://example.test/disk.img", 7)
		nn := createImport(imp)

		squatter := newTestImageWithSlots(imp.Spec.ImageName, []keziov1alpha3.ImageSlot{
			{Number: 1, Role: keziov1alpha3.PartitionRoleData, FSType: "ext4"},
		})
		Expect(k8sClient.Create(ctx, squatter)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, squatter) })

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceeded(ctx, imp, diskResult())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Message).To(ContainSubstring(imp.Spec.ImageName))

		var after keziov1alpha3.Image
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: imp.Spec.ImageName, Namespace: "default"}, &after)).To(Succeed())
		Expect(after.Spec.Layout.Slots).To(HaveLen(1), "the existing Image must be left untouched")
		for i := int32(1); i <= 2; i++ {
			var pc keziov1alpha3.PartitionContent
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: store.ContentName(imp.Spec.ContentPrefix, i), Namespace: "default"}, &pc); err == nil {
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, &pc) })
			}
		}
	})

	It("fails the import when the ingest Job terminally fails", func() {
		imp := newTestImageImport("import-job-failed", "https://example.test/disk.img", 8)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobFailed(ctx, imp)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("ImportFailed"))
	})

	It("fails the import with the ingest binary's own error message when the ingest job reports failure", func() {
		imp := newTestImageImport("import-reported-failure", "https://example.test/disk.img", 9)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceeded(ctx, imp, ingest.Result{Success: false, Error: "boom"})
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond.Message).To(ContainSubstring("ingest job reported failure: boom"))
	})

	It("fails the import when the ingest pod's termination message is not JSON", func() {
		imp := newTestImageImport("import-malformed-result", "https://example.test/disk.img", 10)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceededWithMessage(ctx, imp, "not json")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond.Message).To(ContainSubstring("reading ingest result"))
	})

	It("fails the import when the ingest job's pod is missing entirely", func() {
		imp := newTestImageImport("import-no-pod", "https://example.test/disk.img", 11)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		fakeIngestJobSucceededNoPod(ctx, imp)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond.Message).To(ContainSubstring("reading ingest result"))
	})

	It("fails the import when the ingest result carries no partition table dump", func() {
		imp := newTestImageImport("import-no-sfdisk", "https://example.test/disk.img", 12)
		nn := createImport(imp)

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		result := diskResult()
		result.Disk.SfdiskJSON = ""
		fakeIngestJobSucceeded(ctx, imp, result)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha3.ImageImport
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		Expect(got.Status.State).To(Equal(keziov1alpha3.ImageImportStateFailed))
		readyCond := meta.FindStatusCondition(got.Status.Conditions, keziov1alpha3.ImageImportConditionReady)
		Expect(readyCond.Message).To(ContainSubstring("partition table dump"))
	})
})
