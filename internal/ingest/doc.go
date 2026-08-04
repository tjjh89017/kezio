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
// Job: resolve a source disk image, verify it, normalize it to raw,
// dump its partition table, and hand each partition to partclone (or
// partclone.dd for an unrecognized file system), storing the resulting
// content under internal/store's content-addressed layout and writing an
// Image's layout.json.
//
// Every step that shells out to an external tool (qemu-img, sfdisk,
// blkid, partclone) sits behind a small interface (QemuImg, Sfdisk,
// Blkid, Partclone) so Run's orchestration can be unit tested with fakes;
// the exec-backed implementations of those interfaces live in
// cmd/ingest, which is the only place that actually needs the tools
// installed. This package slices each partition out with a plain file
// copy rather than attaching the whole disk through nbd or loop: since
// partclone's open_source handles a regular file exactly like a block
// device, no privileged nbd/loop setup is needed at all.
package ingest
