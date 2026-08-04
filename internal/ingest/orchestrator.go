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
	// files (the downloaded/staged source, the raw conversion,
	// per-partition slices, per-partition content before it is moved
	// into the store). The caller creates it and is responsible for
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
	// scratchDir is where publishContent lands a content directory on
	// the store's own filesystem before its final same-filesystem
	// rename into contents/ (see publishContent's doc comment). It is
	// keyed by ImageName, so a retried attempt after a crash reuses the
	// same path; clean it before this attempt starts (a prior crash may
	// have left a half-copied content behind) and after it ends either
	// way, so nothing under contents/ is ever left half-published and
	// nothing accumulates under .ingest/ across attempts.
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

	contentDir := filepath.Join(cfg.WorkDir, fmt.Sprintf("content-%d", part.Number))
	if err := os.MkdirAll(contentDir, 0o750); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("create content scratch dir: %w", err)
	}
	if err := deps.Partclone.Clone(ctx, fsInfo.FSType, slicePath, contentDir); err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("clone partition content: %w", err)
	}

	hash, usedBytes, err := publishContent(cfg.StoreRoot, cfg.ImageName, contentDir, part.Number)
	if err != nil {
		return store.ImageLayoutSlot{}, ResultPartition{}, fmt.Errorf("store partition content: %w", err)
	}
	slot.ContentInfoHash = hash.String()
	resultPart.InfoHash = hash.String()
	resultPart.UsedBytes = usedBytes

	return slot, resultPart, nil
}

// publishContent validates a partclone content directory that partclone
// wrote under the work dir (a fast, node-local emptyDir - see Config.WorkDir),
// computes its info hash, and publishes it into the store at its
// content-addressed path. If a content directory with the same hash
// already exists (this exact content was ingested before, by this Image
// or another one), the fresh copy is discarded instead: content is
// deduplicated by hash, per the design's partition content model.
//
// contentDir lives on the work volume, which is not guaranteed to share
// a filesystem with storeRoot (a Job pod's work emptyDir is node-local
// disk; the store is commonly a networked RWX PVC). A direct
// os.Rename(contentDir, dest) would then cross devices and fail with
// EXDEV. To keep the publish step atomic per content, this function
// instead copies contentDir onto the store's own filesystem first, at a
// scratch path under store.IngestScratchDir, and renames from there:
// that rename is always same-filesystem and so is atomic - a reader
// never observes a partially written contents/<hash> directory, and a
// crash between the copy and the rename leaves at worst an orphaned
// scratch directory, which the next attempt for this Image cleans up
// (see run's scratchDir handling).
//
// Partclone still writes its content output to the work dir rather than
// straight to a store-rooted scratch directory: partclone's writes
// during cloning are numerous small, effectively random-access extent
// writes, which a node-local disk absorbs far better than a networked
// filesystem. Landing the finished output with one sequential copy
// plays to a networked store's strength (bulk sequential transfer)
// instead of its weakness (small random I/O), at the cost of writing
// the payload's bytes twice on a Job pod whose work and store volumes
// happen to be the same underlying disk (as in this repo's CI, which
// uses a local-path PVC) - an acceptable trade given production stores
// are the networked case this is optimizing for.
func publishContent(storeRoot, imageName, contentDir string, partitionNumber int32) (store.InfoHash, int64, error) {
	torrentInfo, err := store.LoadContentDirTorrentInfo(contentDir)
	if err != nil {
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
		// already holds these exact bytes. Discard the work-dir copy
		// and skip publishing gracefully.
		if rmErr := os.RemoveAll(contentDir); rmErr != nil {
			return store.InfoHash{}, 0, fmt.Errorf("remove duplicate content scratch dir: %w", rmErr)
		}
		return hash, usedBytes, nil
	}

	scratchDest := filepath.Join(store.IngestScratchDir(storeRoot, imageName), fmt.Sprintf("content-%d", partitionNumber))
	if err := os.RemoveAll(scratchDest); err != nil {
		return store.InfoHash{}, 0, fmt.Errorf("clear content publish scratch dir: %w", err)
	}
	if err := copyContentDir(contentDir, scratchDest); err != nil {
		_ = os.RemoveAll(scratchDest)
		return store.InfoHash{}, 0, fmt.Errorf("copy content onto store filesystem: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		_ = os.RemoveAll(scratchDest)
		return store.InfoHash{}, 0, fmt.Errorf("create store contents dir: %w", err)
	}
	if err := os.Rename(scratchDest, dest); err != nil {
		// A concurrent publish of the same content (a different
		// partition or a different Image, racing this one) may have
		// landed dest between our Stat above and this Rename; content
		// addressing means that racer's bytes are identical to ours,
		// so treat a dest that now exists as success rather than an
		// error.
		if _, statErr := os.Stat(dest); statErr == nil {
			_ = os.RemoveAll(scratchDest)
			return hash, usedBytes, nil
		}
		_ = os.RemoveAll(scratchDest)
		return store.InfoHash{}, 0, fmt.Errorf("move content into store: %w", err)
	}

	if err := os.RemoveAll(contentDir); err != nil {
		return store.InfoHash{}, 0, fmt.Errorf("remove published content from work dir: %w", err)
	}
	return hash, usedBytes, nil
}

// copyContentDir copies the flat set of regular files a partclone
// content directory holds (torrent.info plus extent files - see
// store.ValidateContentDir) from src to a newly created dst, fsyncing
// each file and, once every file is written, the directory itself. The
// fsyncs make the copy durable before publishContent renames dst into
// the store: a crash right after the rename must not be able to expose
// a contents/<hash> directory whose files are not actually on disk yet.
func copyContentDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("%s: unexpected subdirectory %s", src, entry.Name())
		}
		if err := copyFileSynced(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}

	dir, err := os.Open(dst) //nolint:gosec // dst is an ingest-controlled scratch path
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	return nil
}

// copyFileSynced copies one regular file from src to dst and fsyncs dst
// before returning, so its bytes are durable ahead of copyContentDir's
// directory fsync.
func copyFileSynced(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // src is an ingest-controlled scratch path
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

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

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	return nil
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
