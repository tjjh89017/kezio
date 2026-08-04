/*
Copyright 2026 Date Huang.

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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// testIngestConfig returns an IngestConfig with real ingest Jobs enabled,
// backed by an emptyDir store volume (envtest never runs a real Job, so
// no actual volume is ever mounted).
func testIngestConfig() IngestConfig {
	return IngestConfig{
		Image:       "ghcr.io/tjjh89017/kezio-ingest:test",
		StoreVolume: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// setJobStatus patches a Job's status subresource directly, standing in
// for what a real Job controller would report; envtest runs no
// kube-controller-manager, so tests drive this by hand.
func setJobStatus(ctx context.Context, key types.NamespacedName, succeeded, failed int32) {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
	job.Status.Succeeded = succeeded
	job.Status.Failed = failed
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// createIngestPod creates a Pod carrying the job-name label the real Job
// controller would set, with a single terminated container reporting
// message as its termination message - standing in for the completed
// ingest Job's pod.
func createIngestPod(ctx context.Context, namespace, jobName, podName, message string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels:    map[string]string{"job-name": jobName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "ingest", Image: "ghcr.io/tjjh89017/kezio-ingest:test"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "ingest",
		Image: "ghcr.io/tjjh89017/kezio-ingest:test",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("Image Controller, ingest mode", func() {
	ctx := context.Background()

	Context("an image ingested with a real Job configured", func() {
		It("creates an ingest Job with the expected image, env, and volumes", func() {
			const resourceName = "ingest-job-shape"
			// The validating webhook (now active in this suite, see
			// suite_test.go) requires a well-formed checksum; a
			// well-formed but arbitrary sha256 digest stands in here since
			// this test only asserts the value passes through unchanged.
			checksum := "sha256:" + strings.Repeat("a", 64)
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:      "https://example.com/golden.qcow2",
						Format:   keziov1alpha1.ImageFormatQCOW2,
						Checksum: checksum,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: testIngestConfig()}
			DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, image) })

			By("reconciling to attach the finalizer, then to Ingesting")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling once more, which creates the ingest Job")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: ingestJobName(resourceName), Namespace: namespace}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())

			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Name).To(Equal(resourceName))
			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))

			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(testIngestConfig().Image))

			envByName := map[string]string{}
			for _, e := range container.Env {
				envByName[e.Name] = e.Value
			}
			Expect(envByName["IMAGE_NAME"]).To(Equal(resourceName))
			Expect(envByName["SOURCE_URL"]).To(Equal("https://example.com/golden.qcow2"))
			Expect(envByName["SOURCE_FORMAT"]).To(Equal(keziov1alpha1.ImageFormatQCOW2))
			Expect(envByName["SOURCE_CHECKSUM"]).To(Equal(checksum))
			Expect(envByName["STORE_ROOT"]).To(Equal(storeMountPath))
			Expect(envByName).NotTo(HaveKey("STAGING_ROOT"))

			var storeMount *corev1.VolumeMount
			for i := range container.VolumeMounts {
				if container.VolumeMounts[i].Name == "store" {
					storeMount = &container.VolumeMounts[i]
				}
			}
			Expect(storeMount).NotTo(BeNil())
			Expect(storeMount.MountPath).To(Equal(storeMountPath))

			By("asserting the Job's pod is restricted-PodSecurity compliant")
			// kezio-system enforces PodSecurity "restricted:latest";
			// a pod missing any of these fields is rejected outright
			// and its Job never gets a pod at all (the ingest Image
			// then sits in Ingesting until the caller's own timeout -
			// see the regression this pins down). Assert every field
			// "restricted" actually checks, not just presence.
			podSC := job.Spec.Template.Spec.SecurityContext
			Expect(podSC).NotTo(BeNil())
			Expect(podSC.RunAsNonRoot).NotTo(BeNil())
			Expect(*podSC.RunAsNonRoot).To(BeTrue())
			Expect(podSC.RunAsUser).NotTo(BeNil())
			Expect(*podSC.RunAsUser).To(Equal(int64(65532)))
			Expect(podSC.RunAsGroup).NotTo(BeNil())
			Expect(*podSC.RunAsGroup).To(Equal(int64(65532)))
			Expect(podSC.SeccompProfile).NotTo(BeNil())
			Expect(podSC.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

			containerSC := container.SecurityContext
			Expect(containerSC).NotTo(BeNil())
			Expect(containerSC.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*containerSC.AllowPrivilegeEscalation).To(BeFalse())
			Expect(containerSC.Capabilities).NotTo(BeNil())
			Expect(containerSC.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))

			By("reconciling again while the Job has not completed, which does not create a second Job")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(namespace), client.MatchingLabels{ingestJobLabel: resourceName})).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
		})

		It("populates status and reaches Ready once the Job succeeds", func() {
			const resourceName = "ingest-job-success"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{URL: "https://example.com/golden.qcow2", Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: testIngestConfig()}
			DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, image) })

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			jobKey := types.NamespacedName{Name: ingestJobName(resourceName), Namespace: namespace}
			setJobStatus(ctx, jobKey, 1, 0)

			result := ingest.Result{
				Success: true,
				Disk: &ingest.ResultDisk{
					SizeBytes:      4194304,
					PartitionTable: "gpt",
					SfdiskJSON:     `{"partitiontable":{"label":"gpt"}}`,
					Partitions: []ingest.ResultPartition{
						{Number: 1, Role: "esp", FSType: "vfat", UsedBytes: 100, InfoHash: "aa"},
						{Number: 2, Role: "swap", UUID: "swap-uuid"},
					},
				},
			}
			data, err := ingest.MarshalResult(result)
			Expect(err).NotTo(HaveOccurred())
			createIngestPod(ctx, namespace, jobKey.Name, jobKey.Name+"-pod", string(data))

			By("reconciling, which reads the result and advances to Ready")
			final, err := reconcileImageUntil(ctx, r, key, 3, func(i *keziov1alpha1.Image) bool {
				return i.Status.State == keziov1alpha1.ImageStateReady
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(final.Status.State).To(Equal(keziov1alpha1.ImageStateReady))
			Expect(final.Status.Disk).NotTo(BeNil())
			Expect(final.Status.Disk.SizeBytes).To(Equal(int64(4194304)))
			Expect(final.Status.Disk.PartitionTable).To(Equal("gpt"))
			Expect(final.Status.Disk.LayoutRef).NotTo(BeNil())
			Expect(final.Status.Partitions).To(HaveLen(2))
			Expect(final.Status.Partitions[0].InfoHash).To(Equal("aa"))
			Expect(final.Status.Partitions[1].UUID).To(Equal("swap-uuid"))

			cm := &corev1.ConfigMap{}
			cmKey := types.NamespacedName{Name: final.Status.Disk.LayoutRef.Name, Namespace: namespace}
			Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			Expect(cm.Data["sfdisk.json"]).To(Equal(result.Disk.SfdiskJSON))
		})

		It("moves to Failed when the Job fails, using the result's error message", func() {
			const resourceName = "ingest-job-failure"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{URL: "https://example.com/golden.qcow2", Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Ingest: testIngestConfig()}
			DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, image) })

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			jobKey := types.NamespacedName{Name: ingestJobName(resourceName), Namespace: namespace}
			setJobStatus(ctx, jobKey, 0, 1)

			data, err := ingest.MarshalResult(ingest.Result{Success: false, Error: "qemu-img: unsupported format"})
			Expect(err).NotTo(HaveOccurred())
			createIngestPod(ctx, namespace, jobKey.Name, jobKey.Name+"-pod", string(data))

			final, err := reconcileImageUntil(ctx, r, key, 3, func(i *keziov1alpha1.Image) bool {
				return i.Status.State == keziov1alpha1.ImageStateFailed
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(final.Status.State).To(Equal(keziov1alpha1.ImageStateFailed))

			readyCond := apimeta.FindStatusCondition(final.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonIngestFailed))
			Expect(readyCond.Message).To(Equal("qemu-img: unsupported format"))
		})
	})
})
