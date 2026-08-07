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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// publishJobNamePrefix identifies a Job as this controller's publish Job
// for an Image, at a glance in `kubectl get jobs`.
const publishJobNamePrefix = "kezio-publish-"

// publishJobName returns the deterministic Job name for imageName's
// publish run, following ingestJobName's own truncate-and-hash
// convention.
func publishJobName(imageName string) string {
	name := publishJobNamePrefix + imageName
	if len(name) <= maxJobNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(imageName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	maxBaseLen := maxJobNameLength - len(publishJobNamePrefix) - len(suffix)
	return publishJobNamePrefix + imageName[:maxBaseLen] + suffix
}

// reconcilePublishing runs once ingest has populated image.Status.Partitions:
// it ensures every content partition has its own PVC (see
// ensurePartitionPVCs), then creates and polls the publish Job that
// copies each partition's content out of the ingest scratch volume into
// its own PVC. Reaching Ready is gated on this Job succeeding, so a
// per-Image seeder Deployment - which mounts these PVCs directly - never
// races ahead of the content actually being there.
func (r *ImageReconciler) reconcilePublishing(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	pvcNames, err := r.ensurePartitionPVCs(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(pvcNames) == 0 {
		// Every partition turned out blank or swap by the time this ran
		// (handleIngestJobSucceeded already special-cases the common
		// case, but this stays correct if that check is ever bypassed).
		if err := r.deleteIngestScratchPVC(ctx, image); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete, "Image ingest complete")
	}

	jobKey := types.NamespacedName{Name: publishJobName(image.Name), Namespace: image.Namespace}
	job := &batchv1.Job{}
	err = r.Get(ctx, jobKey, job)
	switch {
	case apierrors.IsNotFound(err):
		newJob, buildErr := r.buildPublishJob(image, jobKey.Name, pvcNames)
		if buildErr != nil {
			return ctrl.Result{}, buildErr
		}
		if createErr := r.Create(ctx, newJob); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, createErr
		}
		return ctrl.Result{RequeueAfter: ingestPollInterval}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	switch {
	case job.Status.Succeeded > 0:
		return r.handlePublishJobSucceeded(ctx, image)
	case job.Status.Failed > 0:
		return r.handlePublishJobFailed(ctx, image, job)
	default:
		return ctrl.Result{RequeueAfter: ingestPollInterval}, nil
	}
}

// buildPublishJob constructs the (not yet created) Job that copies
// image's content partitions from its ingest scratch volume into
// pvcNames, owner-ref'd to image the same way buildIngestJob is.
func (r *ImageReconciler) buildPublishJob(image *keziov1alpha1.Image, jobName string, pvcNames map[int32]string) (*batchv1.Job, error) {
	backoffLimit := int32(0)
	ttlSecondsAfterFinished := int32(3600)

	// Sorted for a deterministic env value across reconciles (map
	// iteration order is not stable), so an unrelated reconcile never
	// looks like a spec change to anything diffing this Job.
	numbers := make([]int32, 0, len(pvcNames))
	for n := range pvcNames {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })

	parts := make([]string, 0, len(numbers))
	volumes := make([]corev1.Volume, 0, 1+len(numbers))
	volumes = append(volumes, corev1.Volume{Name: scratchVolumeName, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: ingestScratchPVCName(image.Name),
			ReadOnly:  true,
		},
	}})
	mounts := make([]corev1.VolumeMount, 0, 1+len(numbers))
	mounts = append(mounts, corev1.VolumeMount{Name: scratchVolumeName, MountPath: scratchMountPath, ReadOnly: true})

	for _, number := range numbers {
		infoHash := partitionInfoHash(image, number)
		parts = append(parts, fmt.Sprintf("%d:%s", number, infoHash))

		volName := fmt.Sprintf("content-%d", number)
		volumes = append(volumes, corev1.Volume{Name: volName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcNames[number]},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, MountPath: partitionMountPath(number)})
	}

	env := []corev1.EnvVar{
		{Name: "INGEST_MODE", Value: "publish"},
		{Name: "STORE_ROOT", Value: scratchMountPath},
		{Name: "PUBLISH_PARTITIONS", Value: strings.Join(parts, ",")},
		// TRACKER_URL reuses SeederDeploymentConfig's tracker (the same
		// one the per-Image seeder Deployment is seeded through) rather
		// than introducing a second, parallel setting: whichever .torrent
		// this Job bakes into a partition's PVC is exactly what that
		// Deployment needs to hand ezio later. Empty is valid - the
		// publish step then just copies content with no .torrent, the
		// same as before this field existed.
		{Name: "TRACKER_URL", Value: r.SeederDeployment.TrackerURL},
	}

	labels := map[string]string{ingestJobLabel: image.Name, appComponentLabel: "ingest-publish"}

	runAsUser := int64(65532)
	trueVal := true
	falseVal := false
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: image.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.Ingest.ServiceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &trueVal,
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:                     "publish",
						Image:                    r.Ingest.Image,
						Env:                      env,
						VolumeMounts:             mounts,
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(image, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on publish job: %w", err)
	}
	return job, nil
}

// partitionInfoHash returns the recorded InfoHash for image's partition
// number, or "" if not found (buildPublishJob is only ever called with
// numbers taken from image.Status.Partitions itself, so this should
// always resolve).
func partitionInfoHash(image *keziov1alpha1.Image, number int32) string {
	for _, p := range image.Status.Partitions {
		if p.Number == number {
			return p.InfoHash
		}
	}
	return ""
}

// handlePublishJobSucceeded deletes the now-unneeded ingest scratch PVC
// and advances image to Ready: every content partition's bytes live in
// their own PVC now, which is the precondition a per-Image seeder
// Deployment's read-only mount relies on.
func (r *ImageReconciler) handlePublishJobSucceeded(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	if err := r.deleteIngestScratchPVC(ctx, image); err != nil {
		return ctrl.Result{}, err
	}
	return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete, "Image ingest complete")
}

// handlePublishJobFailed moves image to Failed, using the publish
// result's error message when its pod left one behind. The ingest
// scratch PVC is deliberately left in place on failure - a future retry
// (once retrying a failed Image becomes a feature) would need it, and
// nothing else claims that capacity back while an Image sits Failed.
func (r *ImageReconciler) handlePublishJobFailed(ctx context.Context, image *keziov1alpha1.Image, job *batchv1.Job) (ctrl.Result, error) {
	message := "publish job failed"
	if result, err := r.readPublishResult(ctx, image, job); err == nil && result.Error != "" {
		message = result.Error
	}
	return r.advance(ctx, image, keziov1alpha1.ImageStateFailed, reasonPublishFailed, message)
}

// readPublishResult finds job's pod and parses the ingest.Result its
// container left in its termination message, the same handoff mechanism
// readIngestResult uses.
func (r *ImageReconciler) readPublishResult(ctx context.Context, image *keziov1alpha1.Image, job *batchv1.Job) (ingest.Result, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(image.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ingest.Result{}, fmt.Errorf("list pods for publish job %s: %w", job.Name, err)
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return ingest.UnmarshalResult([]byte(cs.State.Terminated.Message))
			}
		}
	}
	return ingest.Result{}, fmt.Errorf("publish job %s completed with no pod termination message", job.Name)
}
