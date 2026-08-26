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
	corev1 "k8s.io/api/core/v1"
)

// defaultIngestSourceFormat is ImageIngestConfig.SourceFormat's default.
// ImageImportSpec carries no format field of its own, so a manager-wide
// default stands in for it.
const defaultIngestSourceFormat = "qcow2"

// defaultIngestScratchSizeBytes is ImageIngestConfig.ScratchSizeBytes'
// default (16Gi): the floor computeIngestScratchSizeBytes never sizes an
// ingest scratch PVC below, whether or not the source image's size could
// be discovered ahead of running ingest.
const defaultIngestScratchSizeBytes = 16 * 1024 * 1024 * 1024

// defaultIngestScratchAccessModes is ImageIngestConfig.ScratchAccessModes'
// default - see the field's doc comment for why ReadWriteMany.
var defaultIngestScratchAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}

// defaultIngestIOBandwidthBytesPerSec is ImageIngestConfig.
// IOBandwidthBytesPerSec's default (64Mi/s): a cap loose enough not to
// meaningfully slow a routine import, but tight enough that one ingest
// Job can no longer saturate a shared disk the way an unthrottled one
// can.
const defaultIngestIOBandwidthBytesPerSec = 64 * 1024 * 1024

// ImageIngestConfig configures the ImageImport reconciler: the ingest
// Job's image plus everything needed to run it against a source image.
// The zero value (Image == "") is not ready() and holds every import at
// Pending with a condition naming that ingest is unconfigured, mirroring
// PartitionContentPublishConfig's zero-value behavior.
type ImageIngestConfig struct {
	// Image is the ingest Job's container image. Read from
	// IMAGE_INGEST_IMAGE.
	Image string
	// SourceFormat is the format ingest is told every spec.source.url
	// must actually be in (see internal/ingest.Config.SourceFormat, which
	// rejects a mismatch). Read from IMAGE_INGEST_SOURCE_FORMAT; defaults
	// to defaultIngestSourceFormat when unset.
	SourceFormat string
	// ServiceAccountName is the ingest Job pod's service account. Empty
	// uses the namespace default. Read from IMAGE_INGEST_SERVICE_ACCOUNT.
	ServiceAccountName string
	// ScratchStorageClassName selects the ingest scratch PVC's
	// StorageClass. Empty leaves spec.storageClassName unset, so the
	// cluster default applies. Read from
	// IMAGE_INGEST_SCRATCH_STORAGE_CLASS.
	ScratchStorageClassName string
	// ScratchSizeBytes is the floor computeIngestScratchSizeBytes never
	// sizes the ingest scratch PVC below - the discovered source size can
	// only push the request up from here, never down. Read from
	// IMAGE_INGEST_SCRATCH_SIZE_BYTES; defaults to
	// defaultIngestScratchSizeBytes when unset or not a positive integer.
	ScratchSizeBytes int64
	// ScratchAccessModes are the ingest scratch PVC's access modes. Empty
	// defaults to ReadWriteMany (defaultIngestScratchAccessModes): the
	// scratch PVC is written once by the ingest Job and then read back by
	// every partition's own PartitionContent publish Job (see
	// buildPublishJob), which can land on a different node and can run
	// concurrently with the others. Read from
	// IMAGE_INGEST_SCRATCH_ACCESS_MODES (comma-separated).
	ScratchAccessModes []corev1.PersistentVolumeAccessMode
	// StagingPVCName names the PVC holding imageservice's staged uploads
	// (see internal/imageservice.Staging), mounted read-only into the
	// ingest Job when spec.source.url uses the kezio-staged:// scheme.
	// Empty holds such an import at Pending with a condition naming that
	// staging is unconfigured - unlike Image, this only blocks imports
	// that actually reference a staged upload; one with an http(s)://
	// source ingests fine with no staging PVC configured. Read from
	// IMAGE_INGEST_STAGING_PVC.
	StagingPVCName string
	// ImageServiceURL is the base URL of the image-service instance
	// fronting StagingPVCName's staging volume (see cmd/image-service).
	// Sizing a kezio-staged:// import's scratch PVC calls its HEAD
	// /uploads/{name} endpoint; empty leaves that source's size
	// undiscoverable, and computeIngestScratchSizeBytes falls back to the
	// floor. Read from IMAGE_INGEST_IMAGE_SERVICE_URL.
	ImageServiceURL string
	// ImageServiceToken authenticates the ImageServiceURL call above
	// (imageservice.Authenticator's bearer token). Read from
	// IMAGE_INGEST_IMAGE_SERVICE_TOKEN.
	ImageServiceToken string
	// IOBandwidthBytesPerSec caps the ingest Job's own write rate for the
	// steps it performs directly - the source download and each
	// partition's slice extraction (see internal/ingest.Config's doc
	// comment) - on top of the best-effort ionice/nice priority every
	// heavy step runs under regardless. Read from
	// IMAGE_INGEST_IO_BANDWIDTH_BYTES_PER_SEC; defaults to
	// defaultIngestIOBandwidthBytesPerSec when unset or non-positive. A
	// deliberately very large value is how an operator opts back out to
	// unthrottled.
	IOBandwidthBytesPerSec int64
}

// ready reports whether cfg carries enough configuration to dispatch an
// ingest Job at all.
func (cfg ImageIngestConfig) ready() bool {
	return cfg.Image != ""
}

// sourceFormat returns cfg.SourceFormat, or defaultIngestSourceFormat when
// unset.
func (cfg ImageIngestConfig) sourceFormat() string {
	if cfg.SourceFormat != "" {
		return cfg.SourceFormat
	}
	return defaultIngestSourceFormat
}

// scratchSizeBytes returns cfg.ScratchSizeBytes, or
// defaultIngestScratchSizeBytes when unset or non-positive.
func (cfg ImageIngestConfig) scratchSizeBytes() int64 {
	if cfg.ScratchSizeBytes > 0 {
		return cfg.ScratchSizeBytes
	}
	return defaultIngestScratchSizeBytes
}

// scratchAccessModes returns cfg.ScratchAccessModes, or
// defaultIngestScratchAccessModes when unset.
func (cfg ImageIngestConfig) scratchAccessModes() []corev1.PersistentVolumeAccessMode {
	if len(cfg.ScratchAccessModes) > 0 {
		return cfg.ScratchAccessModes
	}
	return defaultIngestScratchAccessModes
}

// ioBandwidthBytesPerSec returns cfg.IOBandwidthBytesPerSec, or
// defaultIngestIOBandwidthBytesPerSec when unset or non-positive.
func (cfg ImageIngestConfig) ioBandwidthBytesPerSec() int64 {
	if cfg.IOBandwidthBytesPerSec > 0 {
		return cfg.IOBandwidthBytesPerSec
	}
	return defaultIngestIOBandwidthBytesPerSec
}
