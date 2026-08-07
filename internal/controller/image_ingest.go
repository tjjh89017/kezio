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
	"k8s.io/apimachinery/pkg/api/resource"
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

// reasonPublishing is the Ready condition reason while an Image sits
// between a succeeded ingest Job and a succeeded publish Job (see
// image_publish.go): ingest itself is done, but content has not yet
// landed in its own per-partition PVCs, so the Image is not Ready.
const reasonPublishing = "PublishingPartitions"

// reasonPublishFailed is the Ready condition reason when the publish
// Job fails or reports failure.
const reasonPublishFailed = "PublishFailed"

// ingestPollInterval is how often onChange re-checks an in-flight ingest
// or publish Job's status.
const ingestPollInterval = 5 * time.Second

// scratchMountPath and stagingMountPath are where the ingest Job's
// container mounts its scratch and staging volumes; kezio-ingest is told
// about them through the STORE_ROOT / STAGING_ROOT environment variables
// set on its container (see cmd/ingest). workMountPath is where its
// scratch work volume is mounted; it is always ingest.DefaultWorkDir so
// the WORK_DIR env var this file sets and cmd/ingest's own fallback stay
// in lockstep even if neither is read.
//
// scratchMountPath is deliberately not called a "store" any more: it
// holds ingest's own working copy of a single Image's content plus the
// per-Image publish Job's read side (see image_publish.go), never
// something a seeder or the manager reads - a partition's actual
// content lives in its own PVC once publishing succeeds (see
// partitionPVCName), and its torrent.info metadata rides inline on
// ImagePartitionStatus.
const (
	scratchMountPath = "/store"
	stagingMountPath = "/staging"
	workMountPath    = ingest.DefaultWorkDir
)

// scratchVolumeName is the ingest Job's Volume/VolumeMount name for its
// scratch volume (see scratchMountPath), reused by the publish Job's
// read-only mount of the same PVC.
const scratchVolumeName = "store"

// defaultScratchStorageSize is the ingest scratch PVC's requested
// capacity when IngestConfig.ScratchStorageSize is unset: generous
// enough for a cloud-image-sized source (hundreds of MiB to a few GiB)
// converted to raw plus every partition's content awaiting publish, but
// this volume is per-Image and transient (deleted once its publish Job
// succeeds - see deleteIngestScratchPVC), so being generous costs
// nothing beyond the Image's own ingest window.
const defaultScratchStorageSize = "10Gi"

// ingestJobLabel marks every resource (Job, its Pods) created for one
// Image's ingest run, and doubles as the Job's controller-runtime watch
// key.
const ingestJobLabel = "kezio.kojuro.date/image"

// IngestConfig configures how the Image reconciler runs real ingest
// Jobs. A zero value (Image == "") disables ingest Jobs entirely: onChange
// takes the stub fast path instead (see onChange's doc comment). This is
// deliberate, not a placeholder to remove later - it is how the e2e
// suite and any deployment without the ingest pipeline wired up keep
// working: Ingesting -> Ready with no Job, no scratch, no staging volume
// required.
//
// There is no shared store volume to configure here any more (compare
// the single, pre-provisioned INGEST_STORE_PVC this type used to
// require): the reconciler creates a fresh, per-Image scratch PVC for
// the ingest Job to write into (see buildIngestScratchPVC) and, once it
// succeeds, one PVC per partition for the publish Job to copy content
// into (see partitionPVCName). ScratchStorageClassName and
// PartitionStorageClassName pick the StorageClass each of those uses;
// empty means the cluster/namespace default.
type IngestConfig struct {
	// Image is the kezio-ingest container image reference. Empty
	// disables ingest Jobs.
	Image string
	// LegacySharedStoreVolume, if set, keeps the pre-per-partition-PVC
	// behavior: the ingest Job mounts this one volume directly (at
	// scratchMountPath) and writes every partition's content into it
	// permanently, with no publish phase and no per-partition PVCs -
	// exactly what this type did before ScratchStorageClassName/
	// PartitionStorageClassName existed. This exists solely for the
	// shared seeder fleet (SeederConfig/SeederReconciler, untouched by
	// this type's own change): that fleet still serves every Ready
	// Image straight out of one shared, permanently-accumulating store
	// volume, and has no code of its own yet to consume per-partition
	// PVCs - migrating it is its own separate, larger piece of work.
	// Leave this nil for the per-Image seeder Deployment path, which
	// this type's other fields configure.
	LegacySharedStoreVolume *corev1.VolumeSource
	// ScratchStorageClassName is the StorageClass for the per-Image
	// ingest scratch PVC (see buildIngestScratchPVC). Empty uses the
	// cluster/namespace default. This volume only needs to be
	// node-local and ReadWriteOnce: only the ingest Job and its own
	// publish Job ever mount it, sequentially, never a seeder.
	ScratchStorageClassName string
	// ScratchStorageSize overrides defaultScratchStorageSize when set.
	ScratchStorageSize resource.Quantity
	// PartitionStorageClassName is the StorageClass for the per-partition
	// PVCs the publish step creates (see partitionPVCName). Empty uses
	// the cluster/namespace default. Production deployments that run a
	// per-Image seeder Deployment across more than one node need a
	// StorageClass whose volumes support being mounted by several nodes
	// at once (RWX or ROX); a single-node e2e cluster can use an
	// ordinary ReadWriteOnce class instead (see PartitionAccessModes).
	PartitionStorageClassName string
	// PartitionAccessModes overrides the per-partition PVCs' access
	// modes; a nil/empty slice defaults to
	// []corev1.ReadWriteOnce, the correct choice for a single-node
	// cluster (see PartitionStorageClassName's doc comment for the
	// multi-node requirement).
	PartitionAccessModes []corev1.PersistentVolumeAccessMode
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

// legacy reports whether ingest runs in the pre-per-partition-PVC shape
// (see LegacySharedStoreVolume's doc comment).
func (c IngestConfig) legacy() bool {
	return c.LegacySharedStoreVolume != nil
}

// scratchStorageSize returns c.ScratchStorageSize, falling back to
// defaultScratchStorageSize when unset.
func (c IngestConfig) scratchStorageSize() resource.Quantity {
	if c.ScratchStorageSize.IsZero() {
		return resource.MustParse(defaultScratchStorageSize)
	}
	return c.ScratchStorageSize
}

// partitionAccessModes returns c.PartitionAccessModes, falling back to
// ReadWriteOnce when unset (see IngestConfig.PartitionAccessModes).
func (c IngestConfig) partitionAccessModes() []corev1.PersistentVolumeAccessMode {
	if len(c.PartitionAccessModes) > 0 {
		return c.PartitionAccessModes
	}
	return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
}

// reconcileIngesting drives one Image through real ingest: creating and
// polling the ingest Job first (idempotent - a Job name is deterministic
// from the Image name, so a second reconcile that finds one already
// there just adopts it), then - once ingest has populated
// image.Status.Partitions - handing off to reconcilePublishing to copy
// content into its own per-partition PVCs before the Image reaches
// Ready. Partitions being populated is what distinguishes the two
// phases; no separate state or annotation is needed; This is only
// called when r.Ingest is enabled (see onChange).
func (r *ImageReconciler) reconcileIngesting(ctx context.Context, image *keziov1alpha1.Image) (ctrl.Result, error) {
	if !r.Ingest.legacy() && image.Status.Partitions != nil {
		return r.reconcilePublishing(ctx, image)
	}

	if !r.Ingest.legacy() {
		if err := r.ensureIngestScratchPVC(ctx, image); err != nil {
			return ctrl.Result{}, err
		}
	}

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

// ingestScratchPVCName returns the deterministic per-Image ingest
// scratch PVC name, following ingestJobName's own truncate-and-hash
// convention so it always fits a PVC name's DNS-1123 subdomain limit.
func ingestScratchPVCName(imageName string) string {
	name := "kezio-ingest-scratch-" + imageName
	if len(name) <= maxJobNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(imageName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	maxBaseLen := maxJobNameLength - len("kezio-ingest-scratch-") - len(suffix)
	return "kezio-ingest-scratch-" + imageName[:maxBaseLen] + suffix
}

// ensureIngestScratchPVC creates image's ingest scratch PVC if it does
// not already exist. Idempotent and side-effect-free on every reconcile
// after the first, the same shape reconcileIngesting's Job creation
// uses.
func (r *ImageReconciler) ensureIngestScratchPVC(ctx context.Context, image *keziov1alpha1.Image) error {
	key := types.NamespacedName{Name: ingestScratchPVCName(image.Name), Namespace: image.Namespace}
	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get ingest scratch pvc: %w", err)
	}

	size := r.Ingest.scratchStorageSize()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if r.Ingest.ScratchStorageClassName != "" {
		pvc.Spec.StorageClassName = &r.Ingest.ScratchStorageClassName
	}
	if err := ctrl.SetControllerReference(image, pvc, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on ingest scratch pvc: %w", err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ingest scratch pvc: %w", err)
	}
	return nil
}

// deleteIngestScratchPVC removes image's ingest scratch PVC once its
// content has been published into per-partition PVCs (see
// handlePublishJobSucceeded): nothing reads it again after that point,
// and it duplicates every partition's content until then, so this is
// explicit cleanup rather than left to ordinary garbage collection on
// Image deletion.
func (r *ImageReconciler) deleteIngestScratchPVC(ctx context.Context, image *keziov1alpha1.Image) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ingestScratchPVCName(image.Name), Namespace: image.Namespace},
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ingest scratch pvc: %w", err)
	}
	return nil
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
		{Name: "STORE_ROOT", Value: scratchMountPath},
		// WORK_DIR is set explicitly (rather than left to cmd/ingest's
		// own "/work" fallback) so this Job template and the binary's
		// default can never independently drift; both ultimately come
		// from ingest.DefaultWorkDir (see workMountPath).
		{Name: "WORK_DIR", Value: workMountPath},
	}
	scratchVolume := corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ingestScratchPVCName(image.Name)},
	}
	if r.Ingest.legacy() {
		scratchVolume = *r.Ingest.LegacySharedStoreVolume
	}
	volumes := []corev1.Volume{
		{Name: scratchVolumeName, VolumeSource: scratchVolume},
		// work is scratch space for the fetched/staged source image,
		// the qemu-img-converted raw disk, and per-partition slices
		// and content before they are moved into the store - never the
		// store or staging volumes themselves (see ingest.Config.WorkDir's
		// doc comment). A cloud image (e.g. Ubuntu's) inflates to several
		// GiB once converted to raw, so this is plain node-disk emptyDir
		// with no sizeLimit rather than a small tmpfs-backed one; nothing
		// here needs to survive the Job.
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: scratchVolumeName, MountPath: scratchMountPath},
		{Name: "work", MountPath: workMountPath},
	}

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
								Drop: []corev1.Capability{dropAllCapabilities},
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
			// TorrentInfo is deliberately left unset here: torrent.info
			// no longer rides the ingest Job's termination message (see
			// internal/ingest.ResultPartition), so this reconciler has
			// no more torrent.info bytes to copy across. The publish Job
			// now writes a ready-made .torrent alongside each
			// partition's content in its own PVC instead (see
			// image_publish.go).
		})
	}
	image.Status.Partitions = partitions

	if r.Ingest.legacy() {
		// The shared seeder fleet reads every Ready Image's content
		// straight out of LegacySharedStoreVolume itself (see that
		// field's doc comment) - there is no per-Image scratch PVC to
		// clean up and no publish phase, exactly like this reconciler
		// behaved before per-partition PVCs existed.
		return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete, "Image ingest complete")
	}

	if !anyPartitionHasContent(partitions) {
		// Nothing to publish (every partition is swap or blank): skip
		// the publish phase and the scratch PVC it would otherwise need,
		// straight to Ready.
		if err := r.deleteIngestScratchPVC(ctx, image); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, image, keziov1alpha1.ImageStateReady, reasonIngestComplete, "Image ingest complete")
	}

	return r.advance(ctx, image, keziov1alpha1.ImageStateIngesting, reasonPublishing,
		"Image ingest complete; publishing partition content volumes")
}

// anyPartitionHasContent reports whether at least one partition carries
// an InfoHash - content the publish phase needs to copy into its own
// PVC. An Image made entirely of swap/blank partitions has none, and
// skips publishing altogether.
func anyPartitionHasContent(partitions []keziov1alpha1.ImagePartitionStatus) bool {
	for _, p := range partitions {
		if p.InfoHash != "" {
			return true
		}
	}
	return false
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
