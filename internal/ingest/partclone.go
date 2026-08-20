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

package ingest

import "context"

// Partclone clones one partition slice file into a content directory of
// extent files plus torrent.info (partclone -c -T), addressing content
// by info hash the way internal/store expects. The exec-backed
// production implementation lives in cmd/ingest; tests use a fake.
type Partclone interface {
	// Clone runs `partclone.<binary for fsType> -c -T -o targetDir
	// source` (or partclone.dd when fsType is unrecognized or its
	// partclone binary is not installed) against source, a regular
	// file holding exactly one partition's bytes. targetDir is created
	// by the caller and must be empty; on success it holds torrent.info
	// plus one extent file per used-block run, matching
	// internal/store.ValidateContentDir's expectations.
	Clone(ctx context.Context, fsType, source, targetDir string) error
}
