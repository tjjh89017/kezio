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

// ImageIngestConfig configures the Image reconciler's ingest half: the
// container image the ingest Job runs for an Image with spec.source. The
// zero value (Image == "") is not ready() - see reconcileIngestPending -
// and holds every source-bearing Image at Pending with a condition
// naming that ingest is unconfigured, mirroring
// PartitionContentPublishConfig's zero-value behavior.
type ImageIngestConfig struct {
	// Image is the ingest Job's container image. Read from
	// IMAGE_INGEST_IMAGE.
	Image string
}

// ready reports whether cfg carries enough configuration to dispatch an
// ingest Job.
func (cfg ImageIngestConfig) ready() bool {
	return cfg.Image != ""
}
