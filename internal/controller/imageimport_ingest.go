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
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/ingest"
)

// imageIngestPollInterval is the safety-net requeue interval while an
// ingest Job is running - mirrors publishPollInterval's role as a
// fallback against a missed Job watch event rather than the primary
// progress signal (SetupWithManager owns the ingest Job, so its watch
// normally retriggers a reconcile the moment its status changes).
var imageIngestPollInterval = 30 * time.Second

// k8sNameMaxLength is the Kubernetes label-value length limit (RFC 1123
// label rules): the Job controller stamps a "job-name" label carrying the
// Job's own name onto every pod it creates (see readIngestResult), so a
// Job name derived from an arbitrary ImageImport name must fit under this
// even though that name itself could be longer.
const k8sNameMaxLength = 63

// truncatedName returns base if it already fits within max characters,
// or base truncated and suffixed with an 8-hex-char hash of the
// untruncated name otherwise - so two long names that only differ in
// their truncated tail cannot collide once shortened.
func truncatedName(base string, max int) string {
	if len(base) <= max {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	suffix := fmt.Sprintf("-%x", sum[:4])
	keep := max - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return base[:keep] + suffix
}

const (
	ingestJobNamePrefix        = "image-ingest-"
	ingestScratchPVCNamePrefix = "image-ingest-scratch-"

	imageAppNameLabel                = "app.kubernetes.io/name"
	imageAppNameValue                = "kezio"
	imageAppComponentLabel           = "app.kubernetes.io/component"
	imageIngestJobComponentValue     = "image-ingest-job"
	imageIngestScratchComponentValue = "image-ingest-scratch"

	ingestWorkVolumeName    = "work"
	ingestStagingVolumeName = "staging"
	ingestStagingMountPath  = "/staging"
)

// ingestJobName returns the deterministic ingest Job name for imp.
func ingestJobName(imp *keziov1alpha3.ImageImport) string {
	return truncatedName(ingestJobNamePrefix+imp.Name, k8sNameMaxLength)
}

// ingestScratchPVCName returns the deterministic ingest scratch PVC name
// for the ImageImport named importName. It is exported to this package
// (not just this file) because buildPublishJob (partitioncontent_job.go)
// derives the same name from a PartitionContent's spec.source.importName
// to mount the same scratch content a partition's publish Job needs to
// read - see buildPublishJob's doc comment.
func ingestScratchPVCName(importName string) string {
	return truncatedName(ingestScratchPVCNamePrefix+importName, k8sNameMaxLength)
}

// ensureIngestScratchPVC gets or creates the scratch PVC an import's
// ingest Job writes every partition's content directory into (see
// buildIngestJob). It is owner-referenced to imp, so ordinary garbage
// collection reclaims it once the ImageImport itself is deleted - nothing
// else cleans it up once ingest finishes (see buildIngestJob's doc
// comment for why it must outlive the ingest Job's own pod).
//
// The PVC's size is decided exactly once, when it is created. An existing
// PVC is returned untouched, whatever size it already carries: resizing a
// PVC that ingest may already be writing to is out of scope here, and a
// storage request can only grow anyway, so a reconcile that runs after a
// spec change (spec is immutable, so the only real cause is the manager's
// own IMAGE_INGEST_SCRATCH_SIZE_BYTES default changing) must not attempt
// it either.
func (r *ImageImportReconciler) ensureIngestScratchPVC(ctx context.Context, imp *keziov1alpha3.ImageImport) (*corev1.PersistentVolumeClaim, error) {
	name := ingestScratchPVCName(imp.Name)
	key := client.ObjectKey{Namespace: imp.Namespace, Name: name}

	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, existing); err == nil {
		return existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("imageimport %q: getting ingest scratch PVC %q: %w", imp.Name, name, err)
	}

	sizeBytes := r.ingestScratchSizeBytes(ctx, imp)
	pvc := r.buildIngestScratchPVC(imp, name, sizeBytes)
	if err := controllerutil.SetControllerReference(imp, pvc, r.Scheme); err != nil {
		return nil, fmt.Errorf("imageimport %q: setting ingest scratch PVC %q owner reference: %w", imp.Name, name, err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("imageimport %q: creating ingest scratch PVC %q: %w", imp.Name, name, err)
	}
	return pvc, nil
}

// ingestScratchSizeBytes decides how large a new ingest scratch PVC for
// imp should be requested. imp.Spec.ScratchSize, when set, wins outright
// - no source size discovery runs, and the floor does not apply. Otherwise
// it tries to discover imp.Spec.Source.URL's size and feeds it through
// computeIngestScratchSizeBytes; an undiscoverable size falls back to the
// floor alone and is reported as a ScratchSizeUnknown Event; r.Recorder
// may be nil in tests that never reach an undiscoverable source.
func (r *ImageImportReconciler) ingestScratchSizeBytes(ctx context.Context, imp *keziov1alpha3.ImageImport) int64 {
	if imp.Spec.ScratchSize != nil {
		return imp.Spec.ScratchSize.Value()
	}

	floor := r.Ingest.scratchSizeBytes()
	sourceSize, known := discoverSourceSizeBytes(ctx, r.Ingest, imp.Spec.Source.URL)
	if !known && r.Recorder != nil {
		r.Recorder.Event(imp, corev1.EventTypeWarning, scratchSizeUnknownEventReason, scratchSizeUnknownEventMessage(imp.Spec.Source.URL))
	}
	factor := int64(scratchSizeSourceFactor)
	if strings.EqualFold(r.Ingest.sourceFormat(), "raw") {
		factor = scratchSizeSourceFactorRaw
	}
	return computeIngestScratchSizeBytes(floor, sourceSize, known, factor)
}

// buildIngestScratchPVC constructs the (not yet created) PVC backing an
// import's ingest scratch space, requesting sizeBytes.
func (r *ImageImportReconciler) buildIngestScratchPVC(imp *keziov1alpha3.ImageImport, name string, sizeBytes int64) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: imp.Namespace,
			Labels: map[string]string{
				imageAppNameLabel:      imageAppNameValue,
				imageAppComponentLabel: imageIngestScratchComponentValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: r.Ingest.scratchAccessModes(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(sizeBytes, resource.BinarySI),
				},
			},
		},
	}
	if r.Ingest.ScratchStorageClassName != "" {
		pvc.Spec.StorageClassName = &r.Ingest.ScratchStorageClassName
	}
	return pvc
}

// ingestJobFor gets the ingest Job for imp, if any.
func (r *ImageImportReconciler) ingestJobFor(ctx context.Context, imp *keziov1alpha3.ImageImport) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: imp.Namespace, Name: ingestJobName(imp)}
	if err := r.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("imageimport %q: getting ingest job %q: %w", imp.Name, key.Name, err)
	}
	return job, nil
}

// createIngestJob creates the ingest Job for imp. r.Ingest must be
// ready() - the caller gates on that before calling this.
func (r *ImageImportReconciler) createIngestJob(ctx context.Context, imp *keziov1alpha3.ImageImport, scratchPVCName string) error {
	job := r.buildIngestJob(imp, scratchPVCName)
	if err := controllerutil.SetControllerReference(imp, job, r.Scheme); err != nil {
		return fmt.Errorf("imageimport %q: setting ingest job owner reference: %w", imp.Name, err)
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("imageimport %q: creating ingest job %q: %w", imp.Name, job.Name, err)
	}
	return nil
}

// buildIngestJob constructs the (not yet created) Job that runs
// kezio-ingest against imp.Spec.Source. This is the authoritative
// Job/CLI contract cmd/ingest implements:
//
//   - INGEST_MODE=ingest selects the ingest path - the same binary's
//     other mode, INGEST_MODE=publish, is what buildPublishJob
//     (partitioncontent_job.go) dispatches to.
//   - SOURCE_URL and SOURCE_CHECKSUM mirror imp.Spec.Source verbatim.
//   - SOURCE_FORMAT is the manager-wide expected format
//     (ImageIngestConfig.SourceFormat / sourceFormat()); ImageImportSpec
//     has no format field of its own.
//   - WORK_DIR is ingest.DefaultWorkDir ("/work"), where the "work"
//     volume - the scratch PVC named ingestScratchPVCName(imp.Name) -
//     is mounted. Every partition's content directory
//     (content-<number>, see internal/ingest's processPartition) lands
//     here and must survive past the ingest Job's own pod: each
//     partition's own PartitionContent publish Job later mounts this
//     same PVC read-only to read its content back out (see
//     buildPublishJob). This is why the work volume is a PVC and not an
//     emptyDir here, at the cost of the large intermediate files (the
//     downloaded source, the raw conversion) sitting on that PVC as well
//     as the content directories that matter.
//   - STAGING_ROOT=/staging is set, and the staging PVC
//     (ImageIngestConfig.StagingPVCName) mounted read-only there, only
//     when spec.source.url uses the kezio-staged:// scheme
//     (ingest.StagedURLScheme) - reconcileIngesting holds the import at
//     Pending instead of building this Job at all when that scheme is
//     used but no staging PVC is configured.
//   - On exit the container writes an ingest.Result (see
//     internal/ingest/result.go) as compact JSON to its termination
//     message (bounded at ingest.TerminationMessageLimit by Kubernetes),
//     success or failure - the sole transport this controller reads back
//     from (see readIngestResult).
func (r *ImageImportReconciler) buildIngestJob(imp *keziov1alpha3.ImageImport, scratchPVCName string) *batchv1.Job {
	backoffLimit := int32(0)
	labels := map[string]string{
		imageAppNameLabel:      imageAppNameValue,
		imageAppComponentLabel: imageIngestJobComponentValue,
	}
	runAsUser := int64(65532)
	trueVal := true
	falseVal := false

	env := []corev1.EnvVar{
		{Name: "INGEST_MODE", Value: "ingest"},
		{Name: "SOURCE_URL", Value: imp.Spec.Source.URL},
		{Name: "SOURCE_CHECKSUM", Value: imp.Spec.Source.Checksum},
		{Name: "SOURCE_FORMAT", Value: r.Ingest.sourceFormat()},
		{Name: "WORK_DIR", Value: ingest.DefaultWorkDir},
	}
	volumes := []corev1.Volume{{
		Name: ingestWorkVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: scratchPVCName},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: ingestWorkVolumeName, MountPath: ingest.DefaultWorkDir}}

	if strings.HasPrefix(imp.Spec.Source.URL, ingest.StagedURLScheme+"://") {
		env = append(env, corev1.EnvVar{Name: "STAGING_ROOT", Value: ingestStagingMountPath})
		volumes = append(volumes, corev1.Volume{
			Name: ingestStagingVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.Ingest.StagingPVCName, ReadOnly: true},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: ingestStagingVolumeName, MountPath: ingestStagingMountPath, ReadOnly: true})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingestJobName(imp),
			Namespace: imp.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
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
						Name:                     "ingest",
						Image:                    r.Ingest.Image,
						Env:                      env,
						VolumeMounts:             mounts,
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	return job
}

// jobOutcome is what a reconciler needs to know about a Job it dispatched.
type jobOutcome int

const (
	jobRunning jobOutcome = iota
	jobSucceeded
	jobFailed
)

// outcomeOf reports job's outcome from its status alone: the Job is the
// sole witness to whether its work succeeded, since neither the ingest
// nor the publish reconciler can mount the volumes the Job wrote.
func outcomeOf(job *batchv1.Job) jobOutcome {
	switch {
	case job.Status.Succeeded > 0:
		return jobSucceeded
	case job.Status.Failed > 0:
		return jobFailed
	default:
		return jobRunning
	}
}

// readJobResult reads back a kezio-ingest Job's Result from its pod's
// termination message - the sole transport out of either Job mode (see
// internal/ingest.Result's doc comment): list job's Pods by the standard
// "job-name" label the Job controller stamps on them, and parse the first
// terminated container's message as JSON.
func readJobResult(ctx context.Context, c client.Client, namespace string, job *batchv1.Job) (ingest.Result, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ingest.Result{}, fmt.Errorf("listing pods for job %q: %w", job.Name, err)
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return ingest.UnmarshalResult([]byte(cs.State.Terminated.Message))
			}
		}
	}
	return ingest.Result{}, fmt.Errorf("job %q completed with no pod termination message", job.Name)
}

// reconcileIngesting dispatches, observes, or concludes imp's ingest Job
// once r.Ingest.ready() - the seam reconcileIngestPending names.
//
// Retry semantics: a terminal ingest Job failure records Failed and is
// not retried automatically - an operator deletes the failed Job (owner
// referenced to this ImageImport, so ordinary garbage collection is not
// what removes it) to let this reconciler build a fresh one on the next
// reconcile. ImageImportSpec is immutable, so there is nothing else for a
// retry to change.
func (r *ImageImportReconciler) reconcileIngesting(ctx context.Context, imp *keziov1alpha3.ImageImport) (ctrl.Result, error) {
	if strings.HasPrefix(imp.Spec.Source.URL, ingest.StagedURLScheme+"://") && r.Ingest.StagingPVCName == "" {
		return r.recordImportPending(ctx, imp, "StagingUnconfigured",
			"spec.source.url uses a staged upload but no staging PVC is configured on the manager; the import stays Pending until it is")
	}

	scratchPVC, err := r.ensureIngestScratchPVC(ctx, imp)
	if err != nil {
		return ctrl.Result{}, err
	}

	job, err := r.ingestJobFor(ctx, imp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if err := r.createIngestJob(ctx, imp, scratchPVC.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordIngesting(ctx, imp, "IngestJobCreated", "ingest job created")
	}

	switch outcomeOf(job) {
	case jobSucceeded:
		return r.completeIngest(ctx, imp, job)
	case jobFailed:
		return r.recordImportFailed(ctx, imp, fmt.Sprintf("ingest job %q failed", job.Name))
	default:
		return r.recordIngesting(ctx, imp, "IngestRunning", "ingest job is running")
	}
}
