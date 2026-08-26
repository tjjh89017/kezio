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

// Package ingest implements the orchestration logic of the kezio-ingest
// Job: resolve a source disk image, verify it, dump its partition table,
// and hand each partition to partclone (or partclone.dd for an
// unrecognized file system), producing one content directory per
// partition in a scratch work directory and a compact per-partition
// summary (Result). It never talks to the Kubernetes API: the caller
// (cmd/ingest, and the Image controller that dispatches it) maps
// Result's per-partition summaries onto PartitionContent objects.
//
// Publishing (RunPublish, in publish.go) is a separate step: once a
// PartitionContent's own PVC exists, it copies that partition's scratch
// content into the PVC (mounted at ContentMountPath) and builds the
// content's .torrent file there.
//
// Two ways to get from the source image to something partclone can read
// (Config.AttachMode, see runAttached and ensureRawDisk):
//
//   - AttachModeNBD (the default): attach the source with qemu-nbd and
//     read straight off the resulting kernel block device and its
//     partition device nodes. No raw conversion copy and no per-partition
//     slice ever exist in the scratch work directory - only each
//     partition's own cloned content does. This needs a privileged ingest
//     Job (CAP_SYS_ADMIN, access to /dev, and the nbd kernel module
//     loaded on the node with max_part>0 - see internal/controller's Job
//     builder and docs/crd-reference.md).
//   - AttachModeCopy: the original unprivileged pipeline, for clusters
//     that cannot run a privileged ingest Job. The source is normalized
//     to raw (ensureRawDisk converts a non-raw source with qemu-img) and
//     each partition is sliced out with a plain Go file copy
//     (extractPartition) before partclone ever sees it - no nbd attach,
//     no loop device, no elevated privilege of any kind, since
//     partclone's open_source handles a regular file exactly like a
//     block device.
//
// Every step that shells out to an external tool (qemu-img, sfdisk,
// blkid, partclone, qemu-nbd) sits behind a small interface (QemuImg,
// Sfdisk, Blkid, Partclone, Attacher) so Run's orchestration can be unit
// tested with fakes; the exec-backed implementations of those interfaces
// live in cmd/ingest, which is the only place that actually needs the
// tools installed.
package ingest
