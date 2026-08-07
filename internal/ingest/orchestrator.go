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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/store"
)

// DefaultWorkDir is the scratch directory path cmd/ingest falls back to
// when WORK_DIR is unset, and the path
// internal/controller.buildIngestJob mounts the Job's work emptyDir at.
// Both sides reference this one constant instead of each hard-coding
// "/work" so the binary's default and the Job template's mount can never
// drift apart.
const DefaultWorkDir = "/work"

// Config parametrizes one Run: everything about the Image being ingested
// and the volumes this Job's pod mounts.
type Config struct {
	// ImageName is the Image resource's name; it keys the layout.json
	// this run writes (see internal/store.ImageDir) and, when the
	// source is staged, the upload to resolve and later remove.
	ImageName string
	// SourceURL is Image.spec.source.url: an http(s):// URL or a
	// kezio-staged://<name> reference.
	SourceURL string
	// SourceFormat is Image.spec.source.format, the format the source
	// is declared to be in. Run rejects a source whose actual format
	// (per qemu-img info) does not match.
	SourceFormat string
	// SourceChecksum is Image.spec.source.checksum ("<algorithm>:<hex
	// digest>"), or empty if none was given.
	SourceChecksum string
	// StoreRoot is the mounted store volume's root (see
	// internal/store's path scheme).
	StoreRoot string
	// WorkDir is a scratch directory this run may fill with temporary
	// files: the downloaded/staged source, the raw conversion, and
	// per-partition slices extracted from it. Its sizing need is
	// therefore source + converted raw (plus one partition slice at a
	// time) - partclone's content output no longer lands here, so it
	// does not also need room for a partition's worth of content (see
	// processPartition). The caller creates it and is responsible for
	// removing it after Run returns; a Job pod uses its container's
	// writable layer or an emptyDir, never the store or staging
	// volumes, so scratch files never leak into either.
	WorkDir string
}

// Dependencies are the small interfaces Run drives; cmd/ingest wires the
// exec-backed implementations, tests wire fakes. Staging and
// StagedRemover may be nil when SourceURL is not a kezio-staged:// URL
// (Run never calls them in that case).
type Dependencies struct {
	Downloader    Downloader
	Staging       StagingResolver
	StagedRemover StagedRemover
	QemuImg       QemuImg
	Sfdisk        Sfdisk
	Blkid         Blkid
	Partclone     Partclone
	// Statfs backs the store-scratch space pre-flight check
	// (ensureScratchSpace) run before each partition's content is
	// cloned. nil means the real syscall.Statfs-backed implementation;
	// tests set it to simulate an arbitrary reported free-space figure.
	Statfs statfsFunc
}

// Run executes the full ingest pipeline described in the package doc
// comment and returns a Result describing what happened. It never
// panics and never returns an error value: every failure becomes
// Result{Success: false, Error: ...}, because the only consumer
// (cmd/ingest) turns Run's outcome directly into the container's
// termination message and exit code.
func Run(ctx context.Context, cfg Config, deps Dependencies) Result {
	disk, err := run(ctx, cfg, deps)
	if err != nil {
		return FailureResult(err)
	}
	return Result{Success: true, Disk: disk}
}

func run(ctx context.Context, cfg Config, deps Dependencies) (*ResultDisk, error) {
	// scratchDir is where partclone writes each partition's content
	// directly, on the store's own filesystem, before publishContent's
	// final same-filesystem rename into contents/ (see processPartition
	// and publishContent). It is keyed by ImageName, so a retried
	// attempt after a crash reuses the same path; clean it before this
	// attempt starts (a prior crash may have left a half-written
	// content behind) and after it ends either way, so nothing under
	// contents/ is ever left half-published and nothing accumulates
	// under .ingest/ across attempts. The store volume needs headroom
	// for this - one in-flight partition's content plus whatever is
	// already published under contents/ - since content is written
	// here directly rather than assembled on the work volume first.
	scratchDir := store.IngestScratchDir(cfg.StoreRoot, cfg.ImageName)
	if err := os.RemoveAll(scratchDir); err != nil {
		return nil, fmt.Errorf("clean ingest scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()

	sourcePath, staged, err := ResolveSource(ctx, cfg.SourceURL, deps.Downloader, deps.Staging, filepath.Join(cfg.WorkDir, "source.img"))
	if err != nil {
		return nil, err
	}

	if err := VerifyChecksum(sourcePath, cfg.SourceChecksum); err != nil {
		return nil, err
	}

	info, err := deps.QemuImg.Info(ctx, sourcePath)
	if err != nil {
		return nil, fmt.Errorf("inspect source image: %w", err)
	}
	if !strings.EqualFold(info.Format, cfg.SourceFormat) {
		return nil, fmt.Errorf("source format mismatch: qemu-img reports %q, Image declares %q", info.Format, cfg.SourceFormat)
	}

	rawPath := filepath.Join(cfg.WorkDir, "disk.raw")
	if err := deps.QemuImg.ConvertToRaw(ctx, sourcePath, info.Format, rawPath); err != nil {
		return nil, fmt.Errorf("convert source image to raw: %w", err)
	}

	diskSize, err := fileSize(rawPath)
	if err != nil {
		return nil, fmt.Errorf("stat converted raw disk: %w", err)
	}

	sfdiskJSON, err := deps.Sfdisk.Dump(ctx, rawPath)
	if err != nil {
		return nil, fmt.Errorf("dump partition table: %w", err)
	}
	parsed, err := ParseSfdiskJSON(sfdiskJSON)
	if err != nil {
		return nil, fmt.Errorf("parse partition table: %w", err)
	}

	layout := &store.ImageLayout{
		SchemaVersion:  store.ImageLayoutSchemaVersion,
		PartitionTable: parsed.PartitionTable,
		DiskGUID:       parsed.DiskGUID,
		SizeBytes:      diskSize,
		SectorSize:     parsed.SectorSize,
	}
	var partitions []ResultPartition

	for _, part := range parsed.Partitions {
		slot, resultPart, err := processPartition(ctx, cfg, deps, rawPath, part)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", part.Number, err)
		}
		layout.Slots = append(layout.Slots, slot)
		partitions = append(partitions, resultPart)
	}

	if err := store.WriteImageLayout(cfg.StoreRoot, cfg.ImageName, layout); err != nil {
		return nil, fmt.Errorf("write image layout: %w", err)
	}

	if staged && deps.StagedRemover != nil {
		// Best-effort: the content is already durably stored at this
		// point, so a cleanup failure must not fail an otherwise
		// successful ingest. A leftover staged upload is at worst
		// wasted staging space, not a correctness problem.
		_ = CleanupStagedSource(deps.StagedRemover, StagedNameFromURL(cfg.SourceURL))
	}

	return &ResultDisk{
		SizeBytes:      diskSize,
		PartitionTable: parsed.PartitionTable,
		SfdiskJSON:     string(sfdiskJSON),
		Partitions:     partitions,
	}, nil
}

// processPartition slices one partition out of the converted raw disk,
// detects its file system, classifies its role, and - for every role
// except swap - clones its content into the store. It returns the
// layout slot and the compact result summary for that partition.
func processPartition(ctx context.Context, cfg Config, deps Dependencies, rawPath string, part ParsedPartition) (store.ImageLayoutSlot, ResultPartition, error) {
	slicePath := filepath.Join(cfg.WorkDir, fmt.Sprintf("part-%d.raw", part.Number))
	if err := extractPartition(rawPath, part.StartBytes, part.SizeBytes, slicePath); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("extract partition slice: %w", err)
	}

	fsInfo, err := deps.Blkid.Detect(ctx, slicePath)
	if err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("detect file system: %w", err)
	}

	role := classifyRole(part.TypeGUID, fsInfo.FSType)

	slot := store.ImageLayoutSlot{
		Number:     part.Number,
		StartBytes: part.StartBytes,
		SizeBytes:  part.SizeBytes,
		TypeGUID:   part.TypeGUID,
		PartUUID:   part.PartUUID,
		Role:       role,
		FSType:     fsInfo.FSType,
	}
	resultPart := ResultPartition{
		Number: part.Number,
		Role:   role,
		FSType: fsInfo.FSType,
	}

	if role == keziov1alpha1.PartitionRoleSwap {
		// Swap carries no content: the agent runs mkswap with the
		// recorded UUID at deploy time instead of restoring bytes.
		slot.FSType = ""
		resultPart.FSType = ""
		slot.SwapUUID = fsInfo.UUID
		resultPart.UUID = fsInfo.UUID
		return slot, resultPart, nil
	}

	// Fail fast, before partclone writes any content byte into store
	// scratch, if the store volume does not currently have enough
	// available space for this partition's content (see
	// ensureScratchSpace for the estimate this compares against). A
	// clear error here feeds the same failure path as every other step
	// of this pipeline, rather than partclone dying mid-write with a
	// harder-to-diagnose ENOSPC.
	if err := ensureScratchSpace(deps.Statfs, cfg.StoreRoot, part.SizeBytes); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("check store scratch space: %w", err)
	}

	// contentDir lives under the store's own per-Image ingest scratch
	// tree, not the work dir: partclone writes its extent files and
	// torrent.info straight onto the store filesystem, so publishing is
	// just a same-filesystem rename with no intermediate copy (see
	// publishContent).
	contentDir := filepath.Join(store.IngestScratchDir(cfg.StoreRoot, cfg.ImageName), fmt.Sprintf("content-%d", part.Number))
	if err := os.MkdirAll(contentDir, 0o750); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("create content scratch dir: %w", err)
	}
	if err := deps.Partclone.Clone(ctx, fsInfo.FSType, slicePath, contentDir); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("clone partition content: %w", err)
	}

	hash, usedBytes, err := publishContent(cfg.StoreRoot, contentDir)
	if err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("store partition content: %w", err)
	}
	slot.ContentInfoHash = hash.String()
	resultPart.InfoHash = hash.String()
	resultPart.UsedBytes = usedBytes

	return slot, resultPart, nil
}

// publishContent validates a partclone content directory that partclone
// wrote directly into the store's per-Image ingest scratch tree (see
// processPartition and store.IngestScratchDir - contentDir already
// lives on storeRoot's own filesystem, not the work dir), computes its
// info hash, and publishes it into the store at its content-addressed
// path. If a content directory with the same hash already exists (this
// exact content was ingested before, by this Image or another one), the
// scratch copy is discarded instead: content is deduplicated by hash,
// since a slot (a disk position) and its partition content (the
// immutable, content-addressed payload) are decoupled.
//
// Because contentDir is already on storeRoot's filesystem,
// os.Rename(contentDir, dest) is always a same-filesystem, atomic
// operation: a reader never observes a partially written
// contents/<hash> directory, and a crash between partclone finishing
// and this rename leaves at worst an orphaned scratch directory, which
// the next attempt for this Image cleans up (see run's scratchDir
// handling). This is also why partclone is pointed at the scratch
// directory directly rather than a work-dir path that would need
// copying onto storeRoot's filesystem first: partclone's -T extent
// output is sequential writes, which land on a networked store exactly
// as well as on local disk, so the copy step that direction would need
// is pure overhead - it would write every content byte twice for no
// benefit.
func publishContent(storeRoot, contentDir string) (store.InfoHash, int64, error) {
	torrentInfo, err := store.LoadContentDirTorrentInfo(contentDir)
	if err != nil {
		return store.InfoHash{}, 0, err
	}
	// partclone -T writes torrent.info and every extent file flat,
	// directly inside contentDir (see internal/ingest.Partclone's doc
	// comment - it has no option to nest its own output). Move the
	// extent files into contentDir's content/ data subdirectory before
	// validating and publishing, so the published layout matches what
	// BuildInfoDict's torrent file entries resolve to on a real
	// BitTorrent v1 client (see store.ContentDataDir).
	if err := store.NestExtentFiles(contentDir, torrentInfo); err != nil {
		return store.InfoHash{}, 0, err
	}
	if err := store.ValidateContentDir(contentDir, torrentInfo); err != nil {
		return store.InfoHash{}, 0, err
	}
	hash, err := store.ComputeInfoHash(torrentInfo)
	if err != nil {
		return store.InfoHash{}, 0, err
	}

	var usedBytes int64
	for _, e := range torrentInfo.Extents {
		usedBytes += int64(e.Length) //nolint:gosec // extent lengths are bounded by real partition sizes
	}

	dest := store.ContentDir(storeRoot, hash)
	if _, err := os.Stat(dest); err == nil {
		// Already published (by an earlier partition in this run, an
		// earlier run of this Image, or another Image entirely): the
		// hash is a content address, so an existing directory at dest
		// already holds these exact bytes. Discard the scratch copy
		// and skip publishing gracefully.
		if rmErr := os.RemoveAll(contentDir); rmErr != nil {
			return store.InfoHash{}, 0, fmt.Errorf("remove duplicate content scratch dir: %w", rmErr)
		}
		return hash, usedBytes, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return store.InfoHash{}, 0, fmt.Errorf("create store contents dir: %w", err)
	}
	if err := os.Rename(contentDir, dest); err != nil {
		// A concurrent publish of the same content (a different
		// partition or a different Image, racing this one) may have
		// landed dest between our Stat above and this Rename; content
		// addressing means that racer's bytes are identical to ours,
		// so treat a dest that now exists as success rather than an
		// error.
		if _, statErr := os.Stat(dest); statErr == nil {
			_ = os.RemoveAll(contentDir)
			return hash, usedBytes, nil
		}
		return store.InfoHash{}, 0, fmt.Errorf("move content into store: %w", err)
	}

	return hash, usedBytes, nil
}

// extractPartition copies the byte range [start, start+size) of src into
// a new file at dst. This is how ingest turns one slot of the converted
// raw disk into the standalone file partclone reads as its source: a
// plain Go file copy, needing no loop device, no nbd, no elevated
// privilege of any kind (see the package doc comment).
func extractPartition(src string, start, size int64, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // src is an ingest-controlled scratch path
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	if _, err := in.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s to %d: %w", src, start, err)
	}

	out, err := os.Create(dst) //nolint:gosec // dst is an ingest-controlled scratch path
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()

	if _, err := io.CopyN(out, in, size); err != nil {
		return fmt.Errorf("copy %d bytes from %s at %d: %w", size, src, start, err)
	}
	return nil
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
