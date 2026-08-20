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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

// publishJobNamePrefix identifies a Job as this reconciler's publish Job
// for a PartitionContent, at a glance in `kubectl get jobs`.
const publishJobNamePrefix = "pc-publish-"

// publishJobBackoffLimit is 0: a failed publish attempt is reported
// through Job.status.failed on the first try rather than retried
// in-place by the Job controller - reconcilePublishing decides whether
// (and how) to retry, the same one-attempt-per-Job discipline the
// ingest/publish Jobs on the legacy branch used.
const publishJobBackoffLimit = 0

// publishJobName returns the deterministic Job name for hash's publish
// run. hash.String() is a fixed-length 40-character hex string, so
// unlike an Image- or Machine-name-derived Job name this never needs
// truncate-and-hash handling to stay under the Kubernetes name limit.
func publishJobName(hash store.InfoHash) string {
	return publishJobNamePrefix + hash.String()
}

// publishJobFor gets the publish Job for hash, if any.
func (r *PartitionContentReconciler) publishJobFor(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: pc.Namespace, Name: publishJobName(hash)}
	if err := r.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("partitioncontent %q: getting publish job %q: %w", pc.Name, key.Name, err)
	}
	return job, nil
}

// createPublishJob creates the publish Job for pc/hash. r.Publish must be
// ready() (see PartitionContentPublishConfig) - the caller gates on that
// before calling this, since an unready config means Pending, not a Job.
func (r *PartitionContentReconciler) createPublishJob(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash, pvcName string) error {
	job := r.buildPublishJob(pc, hash, pvcName)
	if err := controllerutil.SetControllerReference(pc, job, r.Scheme); err != nil {
		return fmt.Errorf("partitioncontent %q: setting publish job owner reference: %w", pc.Name, err)
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("partitioncontent %q: creating publish job %q: %w", pc.Name, job.Name, err)
	}
	return nil
}

// buildPublishJob constructs the (not yet created) Job that runs the
// publish step for pc's content: it mounts the content PVC at
// ingest.ContentMountPath(hash) - the same mount-path convention the
// seeder and the e2e lane use - and announces to r.Publish.TrackerURL.
// The scratch volume holding the ingest-side source content is wired by
// the Image-side ingest orchestration, out of scope here; this Job's
// shape is otherwise complete and does not change once that lands.
func (r *PartitionContentReconciler) buildPublishJob(pc *keziov1alpha2.PartitionContent, hash store.InfoHash, pvcName string) *batchv1.Job {
	backoffLimit := int32(publishJobBackoffLimit)
	labels := map[string]string{
		partitionContentAppNameLabel:      partitionContentAppNameValue,
		partitionContentAppComponentLabel: partitionContentJobComponentValue,
	}

	const contentVolumeName = "content"
	runAsUser := int64(65532)
	trueVal := true
	falseVal := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      publishJobName(hash),
			Namespace: pc.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.Publish.ServiceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &trueVal,
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "publish",
						Image: r.Publish.Image,
						Env: []corev1.EnvVar{
							{Name: "INGEST_MODE", Value: "publish"},
							{Name: "TRACKER_URL", Value: r.Publish.TrackerURL},
							{Name: "PARTITION_CONTENT_HASH", Value: hash.String()},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      contentVolumeName,
							MountPath: ingest.ContentMountPath(hash),
						}},
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: contentVolumeName,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
						},
					}},
				},
			},
		},
	}
	return job
}

// publishJobOutcome is what reconcilePublishing needs to know about a
// running publish Job.
type publishJobOutcome int

const (
	publishJobRunning publishJobOutcome = iota
	publishJobSucceeded
	publishJobFailed
)

// outcomeOf reports job's outcome from its status alone: the reconciler
// has no other way to observe a publish Job's result (it cannot mount the
// content PVC's filesystem itself - see PartitionContentReconciler's doc
// comment), so the Job is the sole witness to whether publishing
// succeeded.
func outcomeOf(job *batchv1.Job) publishJobOutcome {
	switch {
	case job.Status.Succeeded > 0:
		return publishJobSucceeded
	case job.Status.Failed > 0:
		return publishJobFailed
	default:
		return publishJobRunning
	}
}
