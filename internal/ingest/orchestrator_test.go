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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjjh89017/kezio/internal/store"
)

const fixtureSfdiskJSON = `{
  "partitiontable": {
    "label": "gpt",
    "id": "11111111-1111-1111-1111-111111111111",
    "device": "/dev/loop0",
    "unit": "sectors",
    "sectorsize": 512,
    "partitions": [
      {"node": "/dev/loop0p1", "start": 2048, "size": 2048, "type": "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", "uuid": "AAAAAAAA-1111-1111-1111-111111111111"},
      {"node": "/dev/loop0p2", "start": 4096, "size": 2048, "type": "0FC63DAF-8483-4772-8E79-3D69D8477DE4", "uuid": "BBBBBBBB-2222-2222-2222-222222222222"},
      {"node": "/dev/loop0p3", "start": 6144, "size": 2048, "type": "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F", "uuid": "CCCCCCCC-3333-3333-3333-333333333333"}
    ]
  }
}`

const fixtureRawSize = 8192 * 512 // 3 partitions of 2048 sectors starting past a 2048-sector gap

func baseFakeBlkid() *fakeBlkid {
	return &fakeBlkid{byPathSubstring: map[string]FSInfo{
		"part-1": {FSType: "vfat"},
		"part-2": {FSType: "ext4"},
		"part-3": {FSType: "swap", UUID: "CCCC-UUID"},
	}}
}

func baseDeps() Dependencies {
	return Dependencies{
		Downloader: &fakeDownloader{content: []byte("qcow2-body")},
		QemuImg:    &fakeQemuImg{format: "qcow2", rawSize: fixtureRawSize},
		Sfdisk:     &fakeSfdisk{json: []byte(fixtureSfdiskJSON)},
		Blkid:      baseFakeBlkid(),
		Partclone:  &fakePartclone{},
	}
}

func TestRun_Success(t *testing.T) {
	storeRoot := t.TempDir()
	cfg := Config{
		ImageName:    "golden",
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		StoreRoot:    storeRoot,
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, baseDeps())
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if res.Disk == nil {
		t.Fatal("expected a non-nil Disk")
	}
	if res.Disk.SizeBytes != fixtureRawSize {
		t.Errorf("SizeBytes = %d, want %d", res.Disk.SizeBytes, fixtureRawSize)
	}
	if res.Disk.PartitionTable != "gpt" {
		t.Errorf("PartitionTable = %q, want gpt", res.Disk.PartitionTable)
	}
	if len(res.Disk.Partitions) != 3 {
		t.Fatalf("got %d partitions, want 3", len(res.Disk.Partitions))
	}

	esp, data, swap := res.Disk.Partitions[0], res.Disk.Partitions[1], res.Disk.Partitions[2]
	if esp.Role != "esp" || esp.FSType != "vfat" || esp.InfoHash == "" {
		t.Errorf("esp partition = %+v", esp)
	}
	if data.Role != "data" || data.FSType != "ext4" || data.InfoHash == "" {
		t.Errorf("data partition = %+v", data)
	}
	if swap.Role != "swap" || swap.FSType != "" || swap.UUID != "CCCC-UUID" || swap.InfoHash != "" {
		t.Errorf("swap partition = %+v", swap)
	}

	// Both content partitions used identical fixture bytes, so they
	// dedup to the same info hash and only one content directory is
	// stored.
	if esp.InfoHash != data.InfoHash {
		t.Errorf("expected esp and data to dedup to the same info hash, got %q and %q", esp.InfoHash, data.InfoHash)
	}
	entries, err := os.ReadDir(filepath.Join(storeRoot, "contents"))
	if err != nil {
		t.Fatalf("read contents dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d content directories, want 1 (deduped)", len(entries))
	}

	layout, err := store.LoadImageLayout(storeRoot, "golden")
	if err != nil {
		t.Fatalf("LoadImageLayout: %v", err)
	}
	if len(layout.Slots) != 3 {
		t.Fatalf("got %d layout slots, want 3", len(layout.Slots))
	}
	if layout.Slots[2].SwapUUID != "CCCC-UUID" || layout.Slots[2].IsBlank() {
		t.Errorf("swap slot = %+v", layout.Slots[2])
	}
	if layout.Slots[0].IsBlank() {
		t.Errorf("esp slot should not be blank: %+v", layout.Slots[0])
	}
}

func TestRun_StagedSourceRemovedOnSuccess(t *testing.T) {
	workDir := t.TempDir()
	stagedPath := filepath.Join(workDir, "staged-upload.bin")
	if err := os.WriteFile(stagedPath, []byte("qcow2-body"), 0o600); err != nil {
		t.Fatal(err)
	}

	staging := &fakeStaging{paths: map[string]string{"golden": stagedPath}}
	deps := baseDeps()
	deps.Staging = staging
	deps.StagedRemover = staging

	cfg := Config{
		ImageName:    "golden",
		SourceURL:    "kezio-staged://golden",
		SourceFormat: "qcow2",
		StoreRoot:    t.TempDir(),
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, deps)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if len(staging.removed) != 1 || staging.removed[0] != "golden" {
		t.Errorf("staging.removed = %v, want [golden]", staging.removed)
	}
}

func TestRun_ChecksumMismatch(t *testing.T) {
	cfg := Config{
		ImageName:      "golden",
		SourceURL:      "https://example.com/golden.qcow2",
		SourceFormat:   "qcow2",
		SourceChecksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		StoreRoot:      t.TempDir(),
		WorkDir:        t.TempDir(),
	}

	res := Run(context.Background(), cfg, baseDeps())
	if res.Success {
		t.Fatal("expected failure on checksum mismatch")
	}
	if res.Error == "" {
		t.Error("expected a non-empty Error message")
	}
}

func TestRun_ChecksumMatchSucceeds(t *testing.T) {
	content := []byte("qcow2-body")
	sum := sha256.Sum256(content)
	cfg := Config{
		ImageName:      "golden",
		SourceURL:      "https://example.com/golden.qcow2",
		SourceFormat:   "qcow2",
		SourceChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		StoreRoot:      t.TempDir(),
		WorkDir:        t.TempDir(),
	}
	deps := baseDeps()
	deps.Downloader = &fakeDownloader{content: content}

	res := Run(context.Background(), cfg, deps)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
}

func TestRun_FormatMismatch(t *testing.T) {
	deps := baseDeps()
	deps.QemuImg = &fakeQemuImg{format: "raw", rawSize: fixtureRawSize}

	cfg := Config{
		ImageName:    "golden",
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		StoreRoot:    t.TempDir(),
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, deps)
	if res.Success {
		t.Fatal("expected failure on format mismatch")
	}
}

func TestRun_UnsupportedSourceScheme(t *testing.T) {
	cfg := Config{
		ImageName:    "golden",
		SourceURL:    "ftp://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		StoreRoot:    t.TempDir(),
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, baseDeps())
	if res.Success {
		t.Fatal("expected failure on an unsupported source URL scheme")
	}
}

func TestRun_PartcloneFailurePropagates(t *testing.T) {
	deps := baseDeps()
	deps.Partclone = &fakePartclone{err: fmt.Errorf("boom")}

	cfg := Config{
		ImageName:    "golden",
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		StoreRoot:    t.TempDir(),
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, deps)
	if res.Success {
		t.Fatal("expected failure when partclone fails")
	}
}
