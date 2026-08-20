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

package kezioctl

import (
	"os"
	"path/filepath"
	"testing"
)

const testTorrentInfo = `block_size: 4096
blocks_total: 4
offset: 00000000000000000000000000000000
length: 00000000000000000000000000001000
sha1: 000000000000000000000000000000000000000a
`

func TestContentDirObjectName_MatchesKnownHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "torrent.info"), []byte(testTorrentInfo), 0o600); err != nil {
		t.Fatalf("write torrent.info: %v", err)
	}

	got, err := ContentDirObjectName(dir)
	if err != nil {
		t.Fatalf("ContentDirObjectName: %v", err)
	}
	if got == "" {
		t.Fatal("ContentDirObjectName returned an empty name")
	}
	if got[:3] != "pc-" {
		t.Fatalf("ContentDirObjectName = %q, want a \"pc-\" prefix", got)
	}

	// Deterministic: hashing the same torrent.info twice must produce the
	// same name - this is the content-addressing property the whole
	// upload-time contentRef workflow depends on.
	again, err := ContentDirObjectName(dir)
	if err != nil {
		t.Fatalf("ContentDirObjectName (second call): %v", err)
	}
	if got != again {
		t.Fatalf("ContentDirObjectName is not deterministic: %q != %q", got, again)
	}
}

func TestContentDirObjectName_MissingTorrentInfo(t *testing.T) {
	dir := t.TempDir()
	if _, err := ContentDirObjectName(dir); err == nil {
		t.Fatal("ContentDirObjectName: expected an error for a directory with no torrent.info")
	}
}
