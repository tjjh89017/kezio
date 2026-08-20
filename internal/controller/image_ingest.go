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

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
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
// Job name derived from an arbitrary Image name must fit under this even
// though the Image name itself could be longer.
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

// ingestJobName returns the deterministic ingest Job name for image.
func ingestJobName(image *keziov1alpha2.Image) string {
	return truncatedName(ingestJobNamePrefix+image.Name, k8sNameMaxLength)
}

// ingestScratchPVCName returns the deterministic ingest scratch PVC name
// for the Image named imageName. It is exported to this package (not
// just this file) because buildPublishJob (partitioncontent_job.go)
// derives the same name from a PartitionContent's spec.source.imageName
// to mount the same scratch content a partition's publish Job needs to
// read - see buildPublishJob's doc comment.
func ingestScratchPVCName(imageName string) string {
	return truncatedName(ingestScratchPVCNamePrefix+imageName, k8sNameMaxLength)
}

// ensureIngestScratchPVC gets or creates the scratch PVC an Image's
// ingest Job writes every partition's content directory into (see
// buildIngestJob). It is owner-referenced to image, so ordinary garbage
// collection reclaims it once the Image itself is deleted - this item
// does not otherwise clean it up once ingest finishes (see
// buildIngestJob's doc comment for why it must outlive the ingest Job's
// own pod).
func (r *ImageReconciler) ensureIngestScratchPVC(ctx context.Context, image *keziov1alpha2.Image) (*corev1.PersistentVolumeClaim, error) {
	name := ingestScratchPVCName(image.Name)
	key := client.ObjectKey{Namespace: image.Namespace, Name: name}

	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, existing); err == nil {
		return existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("image %q: getting ingest scratch PVC %q: %w", image.Name, name, err)
	}

	pvc := r.buildIngestScratchPVC(image, name)
	if err := controllerutil.SetControllerReference(image, pvc, r.Scheme); err != nil {
		return nil, fmt.Errorf("image %q: setting ingest scratch PVC %q owner reference: %w", image.Name, name, err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("image %q: creating ingest scratch PVC %q: %w", image.Name, name, err)
	}
	return pvc, nil
}

// buildIngestScratchPVC constructs the (not yet created) PVC backing an
// Image's ingest scratch space.
func (r *ImageReconciler) buildIngestScratchPVC(image *keziov1alpha2.Image, name string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: image.Namespace,
			Labels: map[string]string{
				imageAppNameLabel:      imageAppNameValue,
				imageAppComponentLabel: imageIngestScratchComponentValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: r.Ingest.scratchAccessModes(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(r.Ingest.scratchSizeBytes(), resource.BinarySI),
				},
			},
		},
	}
	if r.Ingest.ScratchStorageClassName != "" {
		pvc.Spec.StorageClassName = &r.Ingest.ScratchStorageClassName
	}
	return pvc
}

// ingestJobFor gets the ingest Job for image, if any.
func (r *ImageReconciler) ingestJobFor(ctx context.Context, image *keziov1alpha2.Image) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: image.Namespace, Name: ingestJobName(image)}
	if err := r.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("image %q: getting ingest job %q: %w", image.Name, key.Name, err)
	}
	return job, nil
}

// createIngestJob creates the ingest Job for image. r.Ingest must be
// ready() - the caller gates on that before calling this.
func (r *ImageReconciler) createIngestJob(ctx context.Context, image *keziov1alpha2.Image, scratchPVCName string) error {
	job := r.buildIngestJob(image, scratchPVCName)
	if err := controllerutil.SetControllerReference(image, job, r.Scheme); err != nil {
		return fmt.Errorf("image %q: setting ingest job owner reference: %w", image.Name, err)
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("image %q: creating ingest job %q: %w", image.Name, job.Name, err)
	}
	return nil
}

// buildIngestJob constructs the (not yet created) Job that runs
// kezio-ingest against image.Spec.Source. This is the authoritative
// Job/CLI contract cmd/ingest (a later item) must implement:
//
//   - INGEST_MODE=ingest selects the ingest path - the same binary's
//     other mode, INGEST_MODE=publish, is what buildPublishJob
//     (partitioncontent_job.go) dispatches to.
//   - SOURCE_URL and SOURCE_CHECKSUM mirror image.Spec.Source verbatim.
//   - SOURCE_FORMAT is the manager-wide expected format
//     (ImageIngestConfig.SourceFormat / sourceFormat()); ImageSpec has no
//     format field of its own (see ImageSource's doc comment).
//   - WORK_DIR is ingest.DefaultWorkDir ("/work"), where the "work"
//     volume - the scratch PVC named ingestScratchPVCName(image.Name) -
//     is mounted. Every partition's content directory
//     (content-<number>, see internal/ingest's processPartition) lands
//     here and must survive past the ingest Job's own pod: each
//     partition's own PartitionContent publish Job later mounts this
//     same PVC read-only to read its content back out (see
//     buildPublishJob). This is why the work volume is a PVC and not an
//     emptyDir here, unlike the ingest work directory in legacy, whose
//     work and durable-store volumes were split in two - keeping ingest
//     and publish on one shared PVC needs no second volume kind, at the
//     cost of the large intermediate files (the downloaded source, the
//     raw conversion) sitting on that PVC as well as the content
//     directories that matter; simple over minimal footprint, matching
//     legacy's own working shape otherwise.
//   - STAGING_ROOT=/staging is set, and the staging PVC
//     (ImageIngestConfig.StagingPVCName) mounted read-only there, only
//     when spec.source.url uses the kezio-staged:// scheme
//     (ingest.StagedURLScheme) - reconcileIngesting holds the Image at
//     Pending instead of building this Job at all when that scheme is
//     used but no staging PVC is configured.
//   - On exit the container writes an ingest.Result (see
//     internal/ingest/result.go) as compact JSON to its termination
//     message (bounded at 4KiB by Kubernetes), success or failure - the
//     sole transport this controller reads back from (see
//     readIngestResult), the same mechanism legacy's ingest Job used.
func (r *ImageReconciler) buildIngestJob(image *keziov1alpha2.Image, scratchPVCName string) *batchv1.Job {
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
		{Name: "SOURCE_URL", Value: image.Spec.Source.URL},
		{Name: "SOURCE_CHECKSUM", Value: image.Spec.Source.Checksum},
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

	if strings.HasPrefix(image.Spec.Source.URL, ingest.StagedURLScheme+"://") {
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
			Name:      ingestJobName(image),
			Namespace: image.Namespace,
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

// ingestJobOutcome is what reconcileIngesting needs to know about a
// running ingest Job.
type ingestJobOutcome int

const (
	ingestJobRunning ingestJobOutcome = iota
	ingestJobSucceeded
	ingestJobFailed
)

// ingestOutcomeOf reports job's outcome from its status alone, mirroring
// outcomeOf (partitioncontent_job.go): the Job is the sole witness to
// whether ingest succeeded.
func ingestOutcomeOf(job *batchv1.Job) ingestJobOutcome {
	switch {
	case job.Status.Succeeded > 0:
		return ingestJobSucceeded
	case job.Status.Failed > 0:
		return ingestJobFailed
	default:
		return ingestJobRunning
	}
}

// readIngestResult reads back an ingest Job's Result from its pod's
// termination message - the transport legacy's ingest Job used and this
// controller reuses unchanged (see internal/ingest.Result's doc
// comment): list job's Pods by the standard "job-name" label the Job
// controller stamps on them, and parse the first terminated container's
// message as JSON.
func (r *ImageReconciler) readIngestResult(ctx context.Context, image *keziov1alpha2.Image, job *batchv1.Job) (ingest.Result, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(image.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ingest.Result{}, fmt.Errorf("image %q: listing pods for ingest job %q: %w", image.Name, job.Name, err)
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return ingest.UnmarshalResult([]byte(cs.State.Terminated.Message))
			}
		}
	}
	return ingest.Result{}, fmt.Errorf("image %q: ingest job %q completed with no pod termination message", image.Name, job.Name)
}

// reconcileIngesting dispatches, observes, or concludes the ingest Job
// for a source-bearing Image once r.Ingest.ready() - the seam
// reconcileIngestPending's doc comment names.
//
// Retry semantics: a terminal ingest Job failure records Failed and is
// not retried automatically, mirroring
// PartitionContentReconciler.recordFailed - an operator deletes the
// failed Job (owner-referenced to this Image, so ordinary garbage
// collection is not what removes it) to let this reconciler build a
// fresh one on the next reconcile. ImageSpec is immutable, so there is
// nothing else for a retry to change; keeping this manual mirrors
// PartitionContent's own publish-failure handling rather than adding
// bounded automatic retries this item does not need.
func (r *ImageReconciler) reconcileIngesting(ctx context.Context, image *keziov1alpha2.Image, notReady []string) (ctrl.Result, error) {
	source := image.Spec.Source

	if strings.HasPrefix(source.URL, ingest.StagedURLScheme+"://") && r.Ingest.StagingPVCName == "" {
		return r.recordIngestPending(ctx, image, "StagingUnconfigured",
			"spec.source.url uses a staged upload but no staging PVC is configured on the manager; content stays Pending until it is")
	}

	scratchPVC, err := r.ensureIngestScratchPVC(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}

	job, err := r.ingestJobFor(ctx, image)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if err := r.createIngestJob(ctx, image, scratchPVC.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordIngesting(ctx, image, "IngestJobCreated", "ingest job created")
	}

	switch ingestOutcomeOf(job) {
	case ingestJobSucceeded:
		return r.completeIngest(ctx, image, job)
	case ingestJobFailed:
		return r.recordIngestFailed(ctx, image, fmt.Sprintf("ingest job %q failed", job.Name))
	default:
		return r.recordIngesting(ctx, image, "IngestRunning", "ingest job is running: "+boundedMessage(notReady))
	}
}

// completeIngest handles a successfully completed ingest Job: it reads
// back the per-partition Result (see readIngestResult), verifies every
// declared contentRef slot's computed info hash against what the slot
// already names, and ensures the PartitionContent objects that pass -
// creating one only if it does not already exist (the produce-or-find,
// content-addressed dedupe rule: an existing object by that name is
// reused untouched, with no new PVC and no publish Job of its own - see
// ensurePartitionContent).
//
// A declared contentRef slot's info hash must match what ingest actually
// computed for that partition: ImageSpec is immutable, so contentRef.name
// is the operator's upfront claim about what the source image's bytes
// hash to (see api/v1alpha2's XValidation rule on ImageSlot.ContentRef),
// and this is where that claim is checked. A mismatch fails the Image by
// name rather than silently creating a different content the slot never
// asked for. A blank or swap slot (no ContentRef) needs no content and is
// skipped, mirroring aggregateSlotContents.
func (r *ImageReconciler) completeIngest(ctx context.Context, image *keziov1alpha2.Image, job *batchv1.Job) (ctrl.Result, error) {
	result, err := r.readIngestResult(ctx, image, job)
	if err != nil {
		return r.recordIngestFailed(ctx, image, fmt.Sprintf("reading ingest result: %s", err))
	}
	if !result.Success {
		return r.recordIngestFailed(ctx, image, "ingest job reported failure: "+result.Error)
	}
	if result.Disk == nil {
		return r.recordIngestFailed(ctx, image, "ingest job reported success with no disk result")
	}

	byNumber := make(map[int32]ingest.ResultPartition, len(result.Disk.Partitions))
	for _, p := range result.Disk.Partitions {
		byNumber[p.Number] = p
	}

	for _, slot := range image.Spec.Layout.Slots {
		if slot.ContentRef == nil {
			continue
		}
		rp, ok := byNumber[slot.Number]
		if !ok || rp.InfoHash == "" {
			return r.recordIngestFailed(ctx, image, fmt.Sprintf("slot %d: ingest result has no content for this partition", slot.Number))
		}

		hash, err := store.ParseInfoHash(rp.InfoHash)
		if err != nil {
			return r.recordIngestFailed(ctx, image, fmt.Sprintf("slot %d: ingest result has an invalid info hash: %s", slot.Number, err))
		}
		gotName := store.ObjectName(hash)
		if gotName != slot.ContentRef.Name {
			return r.recordIngestFailed(ctx, image, fmt.Sprintf(
				"slot %d: ingested content hashes to %q, but the slot declares contentRef %q", slot.Number, gotName, slot.ContentRef.Name))
		}

		if err := r.ensurePartitionContent(ctx, image, slot, rp); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Every declared contentRef slot's PartitionContent now exists (fresh
	// or reused); the normal aggregation path (onChange, via
	// aggregateSlotContents) picks up from here on the next reconcile,
	// including driving state to Ready once every content's own publish
	// walk finishes.
	return ctrl.Result{Requeue: true}, nil
}

// ensurePartitionContent creates the PartitionContent slot.ContentRef
// names from rp, if it does not already exist. It is not
// owner-referenced to image: a PartitionContent is content-addressed and
// may outlive, or be shared by, the Image that first produced it (see
// PartitionContentFinalizer's doc comment) - only
// PartitionContentReconciler's own finalizer governs its lifetime.
func (r *ImageReconciler) ensurePartitionContent(ctx context.Context, image *keziov1alpha2.Image, slot keziov1alpha2.ImageSlot, rp ingest.ResultPartition) error {
	existing := &keziov1alpha2.PartitionContent{}
	key := client.ObjectKey{Namespace: image.Namespace, Name: slot.ContentRef.Name}
	if err := r.Get(ctx, key, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("image %q: getting PartitionContent %q for slot %d: %w", image.Name, slot.ContentRef.Name, slot.Number, err)
	}

	pc := &keziov1alpha2.PartitionContent{
		ObjectMeta: metav1.ObjectMeta{Name: slot.ContentRef.Name, Namespace: image.Namespace},
		Spec: keziov1alpha2.PartitionContentSpec{
			FSType:        rp.FSType,
			UsedBytes:     rp.UsedBytes,
			SizeBytes:     rp.SizeBytes,
			LastExtentEnd: rp.LastExtentEnd,
			PieceLength:   rp.PieceLength,
			Source: keziov1alpha2.PartitionContentSource{
				ImageName:       image.Name,
				PartitionNumber: rp.Number,
			},
		},
	}
	if err := r.Create(ctx, pc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("image %q: creating PartitionContent %q for slot %d: %w", image.Name, slot.ContentRef.Name, slot.Number, err)
	}
	return nil
}

// recordIngesting records Ingesting: the ingest Job exists and has not
// yet reported success or failure (including the moment it was just
// created).
func (r *ImageReconciler) recordIngesting(ctx context.Context, image *keziov1alpha2.Image, reason, message string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStateIngesting
	setImageReadyCondition(image, metav1.ConditionFalse, reason, message)
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Ingesting: %w", image.Name, err)
	}
	return ctrl.Result{RequeueAfter: imageIngestPollInterval}, nil
}

// recordIngestPending records Pending for a source-bearing Image that is
// held back from dispatching an ingest Job by a manager-side
// configuration gap narrower than r.Ingest.ready() itself (currently: an
// unconfigured staging PVC for a kezio-staged:// source) - a visible,
// non-error hold that clears on its own once the manager is configured,
// mirroring PartitionContentReconciler.recordPending.
func (r *ImageReconciler) recordIngestPending(ctx context.Context, image *keziov1alpha2.Image, reason, message string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStatePending
	setImageReadyCondition(image, metav1.ConditionFalse, reason, message)
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Pending: %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}

// recordIngestFailed records Failed: the ingest Job failed terminally,
// or succeeded but its result could not be trusted (unreadable, reported
// failure, or a declared contentRef hash mismatch). See
// reconcileIngesting's doc comment for retry semantics.
func (r *ImageReconciler) recordIngestFailed(ctx context.Context, image *keziov1alpha2.Image, message string) (ctrl.Result, error) {
	image.Status.State = keziov1alpha2.ImageStateFailed
	setImageReadyCondition(image, metav1.ConditionFalse, "IngestFailed", message)
	if err := r.applyImageStatus(ctx, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("image %q: recording Failed: %w", image.Name, err)
	}
	return ctrl.Result{}, nil
}
