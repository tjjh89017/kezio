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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// Ready condition reason used when an ingest Job reports (or is found
// to have) failed.
const reasonIngestFailed = "IngestFailed"

// ingestPollInterval is how often onChange re-checks an in-flight ingest
// Job's status.
const ingestPollInterval = 5 * time.Second

// storeMountPath and stagingMountPath are where the ingest Job's
// container mounts the store and staging volumes; kezio-ingest is told
// about them through the STORE_ROOT / STAGING_ROOT environment variables
// set on its container (see cmd/ingest).
const (
	storeMountPath   = "/store"
	stagingMountPath = "/staging"
)

// ingestJobLabel marks every resource (Job, its Pods) created for one
// Image's ingest run, and doubles as the Job's controller-runtime watch
// key.
const ingestJobLabel = "kezio.kojuro.date/image"

// IngestConfig configures how the Image reconciler runs real ingest
// Jobs. A zero value (Image == "") disables ingest Jobs entirely: onChange
// takes the stub fast path instead (see onChange's doc comment). This is
// deliberate, not a placeholder to remove later - it is how the e2e
// suite and any deployment without the ingest pipeline wired up keep
// working: Ingesting -> Ready with no Job, no store, no staging volume
// required.
type IngestConfig struct {
	// Image is the kezio-ingest container image reference. Empty
	// disables ingest Jobs.
	Image string
	// StoreVolume is mounted into the ingest Job's pod at
	// storeMountPath.
	StoreVolume corev1.VolumeSource
	// StagingVolume, if non-nil, is mounted into the ingest Job's pod
	// at stagingMountPath and STAGING_ROOT is set on its container.
	// Only needed to ingest a kezio-staged:// source; an Image with an
	// http(s):// source works without it.
	StagingVolume *corev1.VolumeSource
	// ServiceAccountName, if set, is used for the ingest Job's pod.
	ServiceAccountName string
}

// enabled reports whether real ingest Jobs are configured.
func (c IngestConfig) enabled() bool {
	return c.Image != ""
}

// reconcileIngesting drives one Image through a real ingest Job:
// creating it if missing (idempotent - a Job name is deterministic from
// the Image name, so a second reconcile that finds one already there
// just adopts it), and polling its status once it exists. This is only
// called when r.Ingest is enabled (see onChange).
func (r *ImageReconciler) reconcileIngesting(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	jobKey := types.NamespacedName{Name: ingestJobName(image.Name), Namespace: image.Namespace}
	job := &batchv1.Job{}
	err := r.Get(ctx, jobKey, job)
	switch {
	case apierrors.IsNotFound(err):
		newJob, buildErr := r.buildIngestJob(image, jobKey.Name)
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
		return r.handleIngestJobSucceeded(ctx, image, job)
	case job.Status.Failed > 0:
		return r.handleIngestJobFailed(ctx, image, job)
	default:
		return ctrl.Result{RequeueAfter: ingestPollInterval}, nil
	}
}

// buildIngestJob constructs the (not yet created) Job that ingests
// image, owner-ref'd to it so it is garbage collected with the Image and
// so ctrl.NewControllerManagedBy's Owns(&batchv1.Job{}) watch (see
// SetupWithManager) requeues the Image on Job status changes.
func (r *ImageReconciler) buildIngestJob(image *keziov1alpha1.Image, jobName string) (*batchv1.Job, error) {
	backoffLimit := int32(0)
	ttlSecondsAfterFinished := int32(3600)

	env := []corev1.EnvVar{
		{Name: "IMAGE_NAME", Value: image.Name},
		{Name: "SOURCE_URL", Value: image.Spec.Source.URL},
		{Name: "SOURCE_FORMAT", Value: image.Spec.Source.Format},
		{Name: "SOURCE_CHECKSUM", Value: image.Spec.Source.Checksum},
		{Name: "STORE_ROOT", Value: storeMountPath},
	}
	volumes := []corev1.Volume{{Name: "store", VolumeSource: r.Ingest.StoreVolume}}
	mounts := []corev1.VolumeMount{{Name: "store", MountPath: storeMountPath}}

	if r.Ingest.StagingVolume != nil {
		env = append(env, corev1.EnvVar{Name: "STAGING_ROOT", Value: stagingMountPath})
		volumes = append(volumes, corev1.Volume{Name: "staging", VolumeSource: *r.Ingest.StagingVolume})
		mounts = append(mounts, corev1.VolumeMount{Name: "staging", MountPath: stagingMountPath})
	}

	labels := map[string]string{ingestJobLabel: image.Name}

	// ingestRunAsUser/Group match the kezio-ingest image's baked-in USER
	// (distroless-style nonroot, uid/gid 65532 - the same convention
	// every other kezio image uses), set explicitly here (rather than
	// left to the image default) so the pod satisfies the kezio-system
	// namespace's "restricted" PodSecurity level even if the image's
	// default user ever changes.
	ingestRunAsUser := int64(65532)
	trueVal := true
	falseVal := false
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: image.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.Ingest.ServiceAccountName,
					// kezio-system enforces the "restricted" Pod
					// Security Standard; the ingest pipeline needs no
					// privileges (plain-file slicing, no nbd/loop), so
					// full restricted compliance is both correct and
					// sufficient - see config/manager, config/seeder,
					// config/image-service for the same shape.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &trueVal,
						RunAsUser:    &ingestRunAsUser,
						RunAsGroup:   &ingestRunAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:                     "ingest",
						Image:                    r.Ingest.Image,
						Env:                      env,
						VolumeMounts:             mounts,
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(image, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on ingest job: %w", err)
	}
	return job, nil
}

// handleIngestJobSucceeded reads the completed Job's result and, when
// ingest itself reported success, populates the Image's status and
// advances it to Ready. A Job that exited 0 but reported Success: false
// would be a kezio-ingest bug (it always exits non-zero on failure); this
// is handled the same as a failed Job defensively, not as the expected
// path.
func (r *ImageReconciler) handleIngestJobSucceeded(ctx context.Context, image *keziov1alpha1.Image, job *batchv1.Job) (ctrl.Result, error) {
	result, err := r.readIngestResult(ctx, image, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !result.Success || result.Disk == nil {
		message := result.Error
		if message == "" {
			message = "ingest job succeeded but reported no result"
		}
		return r.advance(ctx, image, keziov1alpha1.ImageStateFailed, reasonIngestFailed, message)
	}

	layoutRefName := imageLayoutConfigMapName(image.Name)
	if err := r.ensureLayoutConfigMap(ctx, image, layoutRefName, result.Disk.SfdiskJSON); err != nil {
		return ctrl.Result{}, err
	}

	image.Status.Disk = &keziov1alpha1.ImageDiskStatus{
		SizeBytes:      result.Disk.SizeBytes,
		PartitionTable: result.Disk.PartitionTable,
		LayoutRef:      &keziov1alpha1.NameRef{Name: layoutRefName},
	}
	partitions := make([]keziov1alpha1.ImagePartitionStatus, 0, len(result.Disk.Partitions))
	for _, p := range result.Disk.Partitions {
		partitions = append(partitions, keziov1alpha1.ImagePartitionStatus{
			Number:    p.Number,
			Role:      p.Role,
			FSType:    p.FSType,
			UsedBytes: p.UsedBytes,
			UUID:      p.UUID,
			InfoHash:  p.InfoHash,
		})
	}
	image.Status.Partitions = partitions

	return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete, "Image ingest complete")
}

// handleIngestJobFailed moves image to Failed, using the ingest result's
// error message when the Job's pod left one behind.
func (r *ImageReconciler) handleIngestJobFailed(ctx context.Context, image *keziov1alpha1.Image, job *batchv1.Job) (ctrl.Result, error) {
	message := "ingest job failed"
	if result, err := r.readIngestResult(ctx, image, job); err == nil && result.Error != "" {
		message = result.Error
	}
	return r.advance(ctx, image, keziov1alpha1.ImageStateFailed, reasonIngestFailed, message)
}

// readIngestResult finds job's pod and parses the ingest.Result its
// container left in its termination message. This is the only channel
// the Image reconciler uses to learn what ingest did: it never mounts
// the store or staging volumes itself (see IngestConfig's doc comment
// and internal/ingest.Result's).
func (r *ImageReconciler) readIngestResult(ctx context.Context, image *keziov1alpha1.Image, job *batchv1.Job) (ingest.Result, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(image.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ingest.Result{}, fmt.Errorf("list pods for ingest job %s: %w", job.Name, err)
	}

	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return ingest.UnmarshalResult([]byte(cs.State.Terminated.Message))
			}
		}
	}
	return ingest.Result{}, fmt.Errorf("ingest job %s completed with no pod termination message", job.Name)
}

// ensureLayoutConfigMap creates or updates the ConfigMap
// status.disk.layoutRef names, holding the verbatim sfdisk JSON dump,
// owner-ref'd to image so it is garbage collected with it.
func (r *ImageReconciler) ensureLayoutConfigMap(ctx context.Context, image *keziov1alpha1.Image, name, sfdiskJSON string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: image.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["sfdisk.json"] = sfdiskJSON
		return ctrl.SetControllerReference(image, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure layout configmap %s: %w", name, err)
	}
	return nil
}

// imageLayoutConfigMapName returns the deterministic ConfigMap name for
// an Image's sfdisk layout dump. Unlike a Job name, a ConfigMap name
// follows the (253-char) DNS subdomain rules, so no truncation is
// needed here even though it is for ingestJobName.
func imageLayoutConfigMapName(imageName string) string {
	return imageName + "-layout"
}

// maxJobNameLength is the Kubernetes Job name limit (a DNS-1035 label:
// Job names generate Pod names with a further "-xxxxx" suffix, capping
// the total at 63 characters).
const maxJobNameLength = 63

// ingestJobNamePrefix identifies a Job as this controller's ingest Job
// for an Image, at a glance in `kubectl get jobs`.
const ingestJobNamePrefix = "kezio-ingest-"

// ingestJobName returns the deterministic Job name for imageName's
// ingest run. It is deterministic (so reconcileIngesting is idempotent:
// re-running it finds the same Job instead of creating a duplicate) and
// always a valid Job name, truncating and disambiguating with a content
// hash suffix when imageName alone would not fit.
func ingestJobName(imageName string) string {
	name := ingestJobNamePrefix + imageName
	if len(name) <= maxJobNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(imageName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	maxBaseLen := maxJobNameLength - len(ingestJobNamePrefix) - len(suffix)
	return ingestJobNamePrefix + imageName[:maxBaseLen] + suffix
}
