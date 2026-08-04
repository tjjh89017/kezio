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

// Blkid detects a file system (and its UUID) inside a partition slice
// file. The exec-backed production implementation (`blkid -o value -s
// TYPE|UUID <path>`) lives in cmd/ingest; tests use a fake.
//
// An unrecognized file system is not an error: blkid exits 2 when it
// finds no recognizable signature, which this package's real
// implementation reports as an empty FSType (see Detect's doc comment)
// so Run falls back to partclone.dd instead of failing ingest outright.
type Blkid interface {
	// Detect reports the file-system type and (when applicable) UUID of
	// the partition slice file at path. FSType is "" when no
	// recognizable file system was found; UUID is "" when the file
	// system has none or none was found.
	Detect(ctx context.Context, path string) (FSInfo, error)
}

// FSInfo is what Blkid.Detect reports about one partition slice file.
type FSInfo struct {
	FSType string
	UUID   string
}
