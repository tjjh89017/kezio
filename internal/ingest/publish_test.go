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

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tjjh89017/kezio/internal/store"
)

// writeScratchContent writes a minimal, valid content directory (a
// torrent.info plus its one extent file, nested under content/) under
// scratchRoot's contents/ tree, and returns its info hash.
func writeScratchContent(t *testing.T, scratchRoot string) store.InfoHash {
	t.Helper()

	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 100,
		Extents:     []store.Extent{{Offset: 0, Length: 16}},
		PieceHashes: []store.PieceHash{{1, 2, 3, 4, 5}},
	}
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	dir := store.ContentDir(scratchRoot, hash)
	if err := os.MkdirAll(store.ContentDataDir(dir), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	infoFile, err := os.Create(store.ContentTorrentInfoPath(scratchRoot, hash))
	if err != nil {
		t.Fatalf("create torrent.info: %v", err)
	}
	if err := store.WriteTorrentInfo(infoFile, info); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}
	if err := infoFile.Close(); err != nil {
		t.Fatalf("close torrent.info: %v", err)
	}
	extentPath := store.ContentExtentPath(scratchRoot, hash, 0)
	if err := os.WriteFile(extentPath, make([]byte, 16), 0o644); err != nil { //nolint:gosec // test fixture path
		t.Fatalf("write extent file: %v", err)
	}
	return hash
}

func TestRunPublish_CopiesAndValidatesContent(t *testing.T) {
	scratchRoot := t.TempDir()
	hash := writeScratchContent(t, scratchRoot)
	destDir := filepath.Join(t.TempDir(), "content-dest")

	result := RunPublish(PublishConfig{
		ScratchRoot: scratchRoot,
		Partitions:  []PublishPartition{{Number: 1, InfoHash: hash.String(), DestDir: destDir}},
	})
	if !result.Success {
		t.Fatalf("RunPublish: Success=false, Error=%q", result.Error)
	}

	info, err := store.LoadContentDirTorrentInfo(destDir)
	if err != nil {
		t.Fatalf("LoadContentDirTorrentInfo(destDir): %v", err)
	}
	if err := store.ValidateContentDir(destDir, info); err != nil {
		t.Fatalf("ValidateContentDir(destDir): %v", err)
	}

	extentPath := filepath.Join(store.ContentDataDir(destDir), store.ExtentFileName(0))
	extentData, err := os.ReadFile(extentPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read copied extent: %v", err)
	}
	if len(extentData) != 16 {
		t.Errorf("len(extentData) = %d, want 16", len(extentData))
	}
}

func TestRunPublish_MultiplePartitions(t *testing.T) {
	scratchRoot := t.TempDir()
	hash1 := writeScratchContent(t, scratchRoot)

	// A second, distinct content directory: writeScratchContent always
	// builds the same fixture, so hand-roll a second one with a
	// different extent length to get a different info hash.
	info2 := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 200,
		Extents:     []store.Extent{{Offset: 0, Length: 32}},
		PieceHashes: []store.PieceHash{{9, 9, 9}},
	}
	hash2, err := store.ComputeInfoHash(info2)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}
	dir2 := store.ContentDir(scratchRoot, hash2)
	if err := os.MkdirAll(store.ContentDataDir(dir2), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f2, err := os.Create(store.ContentTorrentInfoPath(scratchRoot, hash2))
	if err != nil {
		t.Fatalf("create torrent.info: %v", err)
	}
	if err := store.WriteTorrentInfo(f2, info2); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatalf("close torrent.info: %v", err)
	}
	if err := os.WriteFile(store.ContentExtentPath(scratchRoot, hash2, 0), make([]byte, 32), 0o644); err != nil { //nolint:gosec // test fixture path
		t.Fatalf("write extent file: %v", err)
	}

	dest1 := filepath.Join(t.TempDir(), "p1")
	dest2 := filepath.Join(t.TempDir(), "p2")

	result := RunPublish(PublishConfig{
		ScratchRoot: scratchRoot,
		Partitions: []PublishPartition{
			{Number: 1, InfoHash: hash1.String(), DestDir: dest1},
			{Number: 3, InfoHash: hash2.String(), DestDir: dest2},
		},
	})
	if !result.Success {
		t.Fatalf("RunPublish: Success=false, Error=%q", result.Error)
	}

	for _, dest := range []string{dest1, dest2} {
		info, err := store.LoadContentDirTorrentInfo(dest)
		if err != nil {
			t.Fatalf("LoadContentDirTorrentInfo(%s): %v", dest, err)
		}
		if err := store.ValidateContentDir(dest, info); err != nil {
			t.Fatalf("ValidateContentDir(%s): %v", dest, err)
		}
	}
}

func TestRunPublish_MissingSourceContentIsAFailure(t *testing.T) {
	scratchRoot := t.TempDir()
	// A well-formed hash for content that was never written to
	// scratchRoot.
	info := &store.TorrentInfo{BlockSize: 4096, BlocksTotal: 1, Extents: []store.Extent{{Offset: 0, Length: 1}}, PieceHashes: []store.PieceHash{{1}}}
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	result := RunPublish(PublishConfig{
		ScratchRoot: scratchRoot,
		Partitions:  []PublishPartition{{Number: 1, InfoHash: hash.String(), DestDir: t.TempDir()}},
	})
	if result.Success {
		t.Fatal("RunPublish: Success=true, want a failure for content never written to the scratch root")
	}
	if result.Error == "" {
		t.Error("result.Error is empty, want a message naming the missing content")
	}
}
