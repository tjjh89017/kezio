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
	"strings"
	"syscall"
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
	cfg := Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
		AttachMode:   AttachModeCopy,
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

	if res.Disk.SfdiskJSON == "" {
		t.Error("expected the partition table dump to travel back in the result")
	}

	esp, data, swap := res.Disk.Partitions[0], res.Disk.Partitions[1], res.Disk.Partitions[2]
	if esp.Role != "esp" || esp.FSType != "vfat" {
		t.Errorf("esp partition = %+v", esp)
	}
	if esp.TypeGUID != "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" || esp.PartUUID != "AAAAAAAA-1111-1111-1111-111111111111" {
		t.Errorf("esp partition identity = %+v", esp)
	}
	if data.Role != "data" || data.FSType != "ext4" {
		t.Errorf("data partition = %+v", data)
	}
	if swap.Role != "swap" || swap.FSType != "" || swap.UUID != "CCCC-UUID" {
		t.Errorf("swap partition = %+v", swap)
	}
	if esp.PieceLength != store.PieceSize || data.PieceLength != store.PieceSize {
		t.Errorf("expected content partitions to record store.PieceSize, got esp=%d data=%d", esp.PieceLength, data.PieceLength)
	}
	if esp.LastExtentEnd == 0 || data.LastExtentEnd == 0 {
		t.Errorf("expected content partitions to record a non-zero LastExtentEnd, got esp=%d data=%d", esp.LastExtentEnd, data.LastExtentEnd)
	}
	if swap.PieceLength != 0 || swap.LastExtentEnd != 0 {
		t.Errorf("swap partition should record no content fields, got %+v", swap)
	}

	// Each content-bearing partition gets its own scratch content
	// directory under WorkDir, with extents already nested under
	// content/ (see finalizeContent).
	for _, num := range []int32{1, 2} {
		contentDir := filepath.Join(cfg.WorkDir, fmt.Sprintf("content-%d", num))
		if _, err := os.Stat(store.ContentDirTorrentInfoPath(contentDir)); err != nil {
			t.Errorf("partition %d: missing torrent.info in scratch content dir: %v", num, err)
		}
		if _, err := os.Stat(filepath.Join(store.ContentDataDir(contentDir), store.ExtentFileName(0))); err != nil {
			t.Errorf("partition %d: missing nested extent file: %v", num, err)
		}
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
		SourceURL:    "kezio-staged://golden",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
		AttachMode:   AttachModeCopy,
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
		SourceURL:      "https://example.com/golden.qcow2",
		SourceFormat:   "qcow2",
		SourceChecksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
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
		SourceURL:      "https://example.com/golden.qcow2",
		SourceFormat:   "qcow2",
		SourceChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		WorkDir:        t.TempDir(),
		AttachMode:     AttachModeCopy,
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
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
	}

	res := Run(context.Background(), cfg, deps)
	if res.Success {
		t.Fatal("expected failure on format mismatch")
	}
}

// TestRun_RawSourceSkipsConversion checks that a raw-format source is
// sliced in place - no disk.raw conversion copy - and that the
// downloaded source.img is removed once every partition has been
// extracted from it, leaving no large scratch file behind Run.
func TestRun_RawSourceSkipsConversion(t *testing.T) {
	deps := baseDeps()
	qemuImg := &fakeQemuImg{format: "raw", rawSize: fixtureRawSize}
	deps.QemuImg = qemuImg
	// With no conversion, extraction reads straight from the downloaded
	// file - it must already be disk-sized, unlike the qcow2 fixture
	// tests where the fake ConvertToRaw fabricates disk.raw.
	deps.Downloader = &fakeDownloader{content: make([]byte, fixtureRawSize)}

	workDir := t.TempDir()
	cfg := Config{
		SourceURL:    "https://example.com/golden.raw",
		SourceFormat: "raw",
		WorkDir:      workDir,
		AttachMode:   AttachModeCopy,
	}

	res := Run(context.Background(), cfg, deps)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if qemuImg.convertCalls != 0 {
		t.Errorf("ConvertToRaw called %d times, want 0 for a raw source", qemuImg.convertCalls)
	}
	if _, err := os.Stat(filepath.Join(workDir, "disk.raw")); !os.IsNotExist(err) {
		t.Errorf("expected no disk.raw for a raw source, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "source.img")); !os.IsNotExist(err) {
		t.Errorf("expected source.img to be removed once every partition was extracted, stat err = %v", err)
	}
}

// TestRun_NonRawSourceRemovesEachScratchFileAsSoonAsPossible checks the
// peak-scratch layout for a converted (non-raw) source: source.img is
// gone once disk.raw exists, at most one partition slice exists at a
// time, and disk.raw itself is gone once Run returns.
func TestRun_NonRawSourceRemovesEachScratchFileAsSoonAsPossible(t *testing.T) {
	workDir := t.TempDir()
	deps := baseDeps()

	res := Run(context.Background(), Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      workDir,
		AttachMode:   AttachModeCopy,
	}, deps)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}

	if _, err := os.Stat(filepath.Join(workDir, "source.img")); !os.IsNotExist(err) {
		t.Errorf("expected source.img to be removed after conversion, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "disk.raw")); !os.IsNotExist(err) {
		t.Errorf("expected disk.raw to be removed once every partition was extracted, stat err = %v", err)
	}
	for _, num := range []int32{1, 2, 3} {
		slice := filepath.Join(workDir, fmt.Sprintf("part-%d.raw", num))
		if _, err := os.Stat(slice); !os.IsNotExist(err) {
			t.Errorf("expected partition %d's slice to be removed, stat err = %v", num, err)
		}
	}
}

// When the work directory does not have enough available space for even
// the first partition's content, Run fails fast with a clear error
// before partclone is ever invoked - not partway through with an
// ENOSPC-shaped failure from partclone itself.
func TestRun_InsufficientScratchSpaceFailsFast(t *testing.T) {
	deps := baseDeps()
	deps.Statfs = fakeIngestStatfs(1 << 10) // far too little for any real partition
	partclone := &fakePartclone{}
	deps.Partclone = partclone

	cfg := Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
		AttachMode:   AttachModeCopy,
	}

	res := Run(context.Background(), cfg, deps)
	if res.Success {
		t.Fatal("expected failure when the work directory lacks scratch space")
	}
	if !strings.Contains(res.Error, "scratch space") {
		t.Errorf("Error = %q, want it to mention scratch space", res.Error)
	}
	if len(partclone.calls) != 0 {
		t.Errorf("partclone.calls = %v, want none: the space pre-flight must reject before partclone ever runs", partclone.calls)
	}
}

func TestRun_UnsupportedSourceScheme(t *testing.T) {
	cfg := Config{
		SourceURL:    "ftp://example.com/golden.qcow2",
		SourceFormat: "qcow2",
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
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
		AttachMode:   AttachModeCopy,
	}

	res := Run(context.Background(), cfg, deps)
	if res.Success {
		t.Fatal("expected failure when partclone fails")
	}
}

// writeFixtureContentDir writes a minimal, valid partclone content
// directory (one extent file plus a matching torrent.info) at dir,
// mirroring what fakePartclone.Clone produces - see its doc comment for
// why an arbitrary piece hash is fine here.
func writeFixtureContentDir(t *testing.T, dir string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 1,
		Extents:     []store.Extent{{Offset: 0, Length: uint64(len(content))}}, //nolint:gosec // fixture length
		PieceHashes: []store.PieceHash{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}},
	}
	if err := os.WriteFile(filepath.Join(dir, store.ExtentFileName(0)), content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(store.ContentDirTorrentInfoPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := store.WriteTorrentInfo(f, info); err != nil {
		t.Fatal(err)
	}
}

// attachDeps returns a Dependencies wired for the nbd-attach path
// (default AttachMode): baseDeps() plus a fakeAttacher whose fabricated
// partition device paths ("/dev/nbd0p<N>") the fake blkid map matches.
func attachDeps() (Dependencies, *fakeAttacher) {
	deps := baseDeps()
	deps.Blkid = &fakeBlkid{byPathSubstring: map[string]FSInfo{
		"nbd0p1": {FSType: "vfat"},
		"nbd0p2": {FSType: "ext4"},
		"nbd0p3": {FSType: "swap", UUID: "CCCC-UUID"},
	}}
	attacher := &fakeAttacher{}
	deps.Attacher = attacher
	return deps, attacher
}

// TestRun_AttachIsDefault checks that an unset AttachMode drives the
// nbd-attach path: no disk.raw or part-N.raw scratch file is ever
// written, and Run reads straight through the Attacher.
func TestRun_AttachIsDefault(t *testing.T) {
	deps, attacher := attachDeps()
	workDir := t.TempDir()

	res := Run(context.Background(), Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      workDir,
	}, deps)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if res.Disk == nil || len(res.Disk.Partitions) != 3 {
		t.Fatalf("Disk = %+v, want 3 partitions", res.Disk)
	}
	if !attacher.attached || !attacher.detached {
		t.Errorf("attacher.attached=%v detached=%v, want both true", attacher.attached, attacher.detached)
	}
	if _, err := os.Stat(filepath.Join(workDir, "disk.raw")); !os.IsNotExist(err) {
		t.Errorf("expected no disk.raw under the attach path, stat err = %v", err)
	}
	for _, num := range []int32{1, 2, 3} {
		slice := filepath.Join(workDir, fmt.Sprintf("part-%d.raw", num))
		if _, err := os.Stat(slice); !os.IsNotExist(err) {
			t.Errorf("expected no partition %d slice under the attach path, stat err = %v", num, err)
		}
	}
}

// TestRun_AttachDetachesOnFailure checks that a failure partway through
// (here, blkid) still runs the Attacher's detach - a connected nbd
// device must never leak just because a later step failed.
func TestRun_AttachDetachesOnFailure(t *testing.T) {
	deps, attacher := attachDeps()
	deps.Blkid = &fakeBlkid{err: fmt.Errorf("boom")}

	res := Run(context.Background(), Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
	}, deps)
	if res.Success {
		t.Fatal("expected failure when blkid fails")
	}
	if !attacher.detached {
		t.Error("expected the attacher to be detached even though the run failed")
	}
	if len(attacher.events) != 2 || attacher.events[0] != "attach" || attacher.events[1] != "detach" {
		t.Errorf("events = %v, want [attach detach]", attacher.events)
	}
}

// TestRun_AttachMissingPartitionDevice checks that a partition device
// node that never appears is reported as a clear failure (not a panic or
// an opaque tool error), and still detaches.
func TestRun_AttachMissingPartitionDevice(t *testing.T) {
	deps, attacher := attachDeps()
	attacher.partitionErr = fmt.Errorf("partition device /dev/nbd0p1 did not appear")

	res := Run(context.Background(), Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
	}, deps)
	if res.Success {
		t.Fatal("expected failure when a partition device node never appears")
	}
	if !strings.Contains(res.Error, "did not appear") {
		t.Errorf("Error = %q, want it to mention the missing partition device", res.Error)
	}
	if !attacher.detached {
		t.Error("expected the attacher to be detached even though the run failed")
	}
}

// TestRun_AttachFailurePropagates checks that Attach itself failing (for
// example, no free nbd device) is reported as a clear failure with no
// detach call - there is nothing to detach.
func TestRun_AttachFailurePropagates(t *testing.T) {
	deps, attacher := attachDeps()
	attacher.attachErr = fmt.Errorf("no free /dev/nbd* device found")

	res := Run(context.Background(), Config{
		SourceURL:    "https://example.com/golden.qcow2",
		SourceFormat: "qcow2",
		WorkDir:      t.TempDir(),
	}, deps)
	if res.Success {
		t.Fatal("expected failure when Attach fails")
	}
	if attacher.detached {
		t.Error("expected no detach call when Attach itself failed")
	}
}

func TestFinalizeContent_MeasuresContentAndNestsExtents(t *testing.T) {
	contentDir := t.TempDir()
	writeFixtureContentDir(t, contentDir, []byte("payload"))

	usedBytes, lastExtentEnd, err := finalizeContent(contentDir)
	if err != nil {
		t.Fatalf("finalizeContent: %v", err)
	}
	if usedBytes != int64(len("payload")) {
		t.Errorf("usedBytes = %d, want %d", usedBytes, len("payload"))
	}
	if lastExtentEnd != int64(len("payload")) {
		t.Errorf("lastExtentEnd = %d, want %d", lastExtentEnd, len("payload"))
	}
	if _, err := os.Stat(filepath.Join(store.ContentDataDir(contentDir), store.ExtentFileName(0))); err != nil {
		t.Errorf("expected the extent file nested under content/, stat err = %v", err)
	}
}

func TestFinalizeContent_MissingTorrentInfoFails(t *testing.T) {
	contentDir := t.TempDir()
	if _, _, err := finalizeContent(contentDir); err == nil {
		t.Fatal("expected an error for a content dir with no torrent.info")
	}
}

// withGeteuid temporarily replaces the package's geteuid seam, restoring
// it on test cleanup.
func withGeteuid(t *testing.T, uid int) {
	t.Helper()
	orig := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = orig })
}

// TestFixContentOwnership_NonRootIsNoOp checks that an unprivileged
// (AttachModeCopy) ingest process - already running as the identity the
// publish Job expects - never calls chown at all.
func TestFixContentOwnership_NonRootIsNoOp(t *testing.T) {
	withGeteuid(t, ContentOwnerUID)

	called := false
	chown := func(string, int, int) error {
		called = true
		return nil
	}

	if err := fixContentOwnership(chown, "/some/content-1"); err != nil {
		t.Fatalf("fixContentOwnership: %v", err)
	}
	if called {
		t.Error("expected no chown call for a non-root process")
	}
}

// TestFixContentOwnership_RootHandsOwnershipToContentOwner checks that
// the nbd-attach path (running as root) hands its content directory back
// to ContentOwnerUID/GID.
func TestFixContentOwnership_RootHandsOwnershipToContentOwner(t *testing.T) {
	withGeteuid(t, 0)

	var gotDir string
	var gotUID, gotGID int
	chown := func(dir string, uid, gid int) error {
		gotDir, gotUID, gotGID = dir, uid, gid
		return nil
	}

	if err := fixContentOwnership(chown, "/work/content-1"); err != nil {
		t.Fatalf("fixContentOwnership: %v", err)
	}
	if gotDir != "/work/content-1" || gotUID != ContentOwnerUID || gotGID != ContentOwnerGID {
		t.Errorf("chown called with (%q, %d, %d), want (%q, %d, %d)",
			gotDir, gotUID, gotGID, "/work/content-1", ContentOwnerUID, ContentOwnerGID)
	}
}

// TestFixContentOwnership_ChownFailurePropagates checks that a chown
// failure surfaces as an error rather than being swallowed.
func TestFixContentOwnership_ChownFailurePropagates(t *testing.T) {
	withGeteuid(t, 0)

	err := fixContentOwnership(func(string, int, int) error {
		return fmt.Errorf("boom")
	}, "/work/content-1")
	if err == nil {
		t.Fatal("expected an error when chown fails")
	}
}

// TestFixContentOwnership_NilChownUsesRealChownTree checks that a nil
// chownFunc falls back to walking and lchowning the real directory tree
// - this is the only test that actually needs to run as root to observe
// a changed owner, so it self-skips otherwise.
func TestFixContentOwnership_NilChownUsesRealChownTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown to an arbitrary uid/gid")
	}
	withGeteuid(t, 0)

	dir := t.TempDir()
	nested := filepath.Join(dir, "content")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "extent")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fixContentOwnership(nil, dir); err != nil {
		t.Fatalf("fixContentOwnership: %v", err)
	}

	for _, p := range []string{dir, nested, file} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat_t not available for %s", p)
		}
		if int(st.Uid) != ContentOwnerUID || int(st.Gid) != ContentOwnerGID {
			t.Errorf("%s owner = %d:%d, want %d:%d", p, st.Uid, st.Gid, ContentOwnerUID, ContentOwnerGID)
		}
	}
}
