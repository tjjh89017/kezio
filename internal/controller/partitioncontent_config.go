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
	"time"

	corev1 "k8s.io/api/core/v1"
)

// defaultPublishJobTTL is PartitionContentPublishConfig.JobTTL's default:
// long enough that a completed publish Job's pod stays around for a
// while for debugging, short enough that it does not indefinitely block
// the ingest scratch PVC its pod still mounts read-only from going
// Terminating (pvc-protection) once nothing else references it - see
// PartitionContentReconciler.onDelete, which deletes the Job outright
// rather than wait on this TTL when the PartitionContent itself is
// deleted.
const defaultPublishJobTTL = time.Hour

// PartitionContentPublishConfig configures the PartitionContent
// reconciler's publish half: the container image the publish Job runs.
// It carries no tracker or announce setting - publishing never has a
// Site in scope, so it has no correct announce URL to bake into
// anything (see internal/ingest.PublishConfig's doc comment); the
// per-Site announce is applied later, at the seeder. Image is mandatory
// for publishing (see PartitionContentReconciler.reconcilePending):
// leaving it unset - the zero PartitionContentPublishConfig,
// cmd/main.go's default when the corresponding environment variable is
// absent - holds every PartitionContent at Pending with a condition
// explaining why, rather than either fabricating a fast path or
// failing.
type PartitionContentPublishConfig struct {
	// Image is the publish Job's container image (runs the ingest
	// package's publish step against the content PVC and its scratch
	// source). Read from PARTITIONCONTENT_PUBLISH_IMAGE.
	Image string
	// ServiceAccountName is the publish Job pod's service account. Empty
	// uses the namespace default. Read from
	// PARTITIONCONTENT_PUBLISH_SERVICE_ACCOUNT.
	ServiceAccountName string
	// StorageClassName selects the content PVC's StorageClass. Empty
	// leaves spec.storageClassName unset, so the cluster default applies.
	// Read from PARTITIONCONTENT_STORAGE_CLASS.
	StorageClassName string
	// AccessModes are the content PVC's access modes. Empty defaults to
	// ReadWriteMany (defaultPartitionContentAccessModes): a content PVC
	// is read during publish and later read concurrently by seeders
	// spread across nodes, so ReadWriteOnce is not viable for the normal
	// multi-node case. Read from PARTITIONCONTENT_ACCESS_MODES
	// (comma-separated).
	AccessModes []corev1.PersistentVolumeAccessMode
	// JobTTL overrides defaultPublishJobTTL when positive. Read from
	// PARTITIONCONTENT_PUBLISH_JOB_TTL (a time.ParseDuration string).
	JobTTL time.Duration
}

// defaultPartitionContentAccessModes is PartitionContentPublishConfig's
// AccessModes default - see its doc comment for why ReadWriteMany.
var defaultPartitionContentAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}

// accessModes returns cfg.AccessModes, or defaultPartitionContentAccessModes
// when unset.
func (cfg PartitionContentPublishConfig) accessModes() []corev1.PersistentVolumeAccessMode {
	if len(cfg.AccessModes) > 0 {
		return cfg.AccessModes
	}
	return defaultPartitionContentAccessModes
}

// ready reports whether cfg carries enough configuration for the
// reconciler to publish content: the Job image is mandatory (see the
// type doc comment).
func (cfg PartitionContentPublishConfig) ready() bool {
	return cfg.Image != ""
}

// jobTTLSeconds returns cfg.JobTTL in whole seconds, or
// defaultPublishJobTTL when JobTTL is unset or non-positive.
func (cfg PartitionContentPublishConfig) jobTTLSeconds() int32 {
	ttl := cfg.JobTTL
	if ttl <= 0 {
		ttl = defaultPublishJobTTL
	}
	return int32(ttl.Seconds()) //nolint:gosec // bounded by a manager-configured duration, never user input
}
