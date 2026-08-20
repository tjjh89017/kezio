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

import corev1 "k8s.io/api/core/v1"

// PartitionContentPublishConfig configures the PartitionContent
// reconciler's publish half: the container image the publish Job runs
// and the tracker every .torrent it builds announces to. Both are
// mandatory for publishing (see PartitionContentReconciler.reconcilePending):
// leaving either unset - the zero PartitionContentPublishConfig, cmd/main.go's
// default when the corresponding environment variable is absent - holds
// every PartitionContent at Pending with a condition explaining why,
// rather than either fabricating a fast path or failing.
type PartitionContentPublishConfig struct {
	// Image is the publish Job's container image (runs the ingest
	// package's publish step against the content PVC and its scratch
	// source). Read from PARTITIONCONTENT_PUBLISH_IMAGE.
	Image string
	// TrackerURL is the BitTorrent announce URL baked into every
	// .torrent the publish step builds. Read from
	// PARTITIONCONTENT_TRACKER_URL.
	TrackerURL string
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
// reconciler to publish content: both the Job image and the tracker URL
// are mandatory (see the type doc comment).
func (cfg PartitionContentPublishConfig) ready() bool {
	return cfg.Image != "" && cfg.TrackerURL != ""
}
