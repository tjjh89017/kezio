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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/store"
)

// DefaultWorkDir is the scratch directory path cmd/ingest falls back to
// when WORK_DIR is unset, and the path internal/controller's Job builder
// mounts the Job's work volume at. Both sides reference this one
// constant instead of each hard-coding "/work" so the binary's default
// and the Job template's mount can never drift apart.
const DefaultWorkDir = "/work"

// AttachModeNBD is Config.AttachMode's value selecting the nbd-attach
// path: the source image is connected to a kernel nbd device with
// qemu-nbd and every step below reads straight from that device (and its
// kernel-created partition device nodes) instead of copying bytes into
// scratch files. This is the default (an empty AttachMode behaves
// identically - see run()); AttachModeCopy is the opt-out.
const AttachModeNBD = "nbd"

// AttachModeCopy is Config.AttachMode's opt-out value: the original
// unprivileged pipeline, unchanged (see ensureRawDisk and
// extractPartition) - a raw disk file or conversion copy in WorkDir, and
// each partition sliced out with a plain file copy. Selected by clusters
// that cannot run a privileged ingest Job.
const AttachModeCopy = "copy"

// Config parametrizes one Run: everything about the source image being
// ingested and the work volume this Job's pod mounts.
type Config struct {
	// SourceURL is ImageImport.spec.source.url: an http(s):// URL or a
	// kezio-staged://<name> reference.
	SourceURL string
	// SourceFormat is the format the source is declared to be in. Run
	// rejects a source whose actual format (per qemu-img info) does not
	// match.
	SourceFormat string
	// SourceChecksum is ImageImport.spec.source.checksum ("<algorithm>:<hex
	// digest>"), or empty if none was given.
	SourceChecksum string
	// WorkDir is a scratch directory this run fills with temporary
	// files: the downloaded/staged source (deleted as soon as it is no
	// longer needed - see ensureRawDisk and run), at most one partition's
	// extracted slice at a time (deleted as soon as that partition is
	// done - see processPartition), and - unlike the shared-store-root
	// legacy layout - each partition's own content directory (extent
	// files plus torrent.info), since each content's own PVC does not
	// exist yet when Run starts (see publish.go). The caller creates
	// WorkDir and is responsible
	// for removing it after Run returns.
	WorkDir string
	// IOBandwidthBytesPerSec caps the write rate of the two file copies
	// this package performs itself (the source download and each
	// partition's slice extraction) - see newThrottledWriter. It does
	// not throttle qemu-img or partclone, which write directly; those
	// get a best-effort ionice/nice priority instead (see cmd/ingest). 0
	// or negative disables throttling.
	IOBandwidthBytesPerSec int64
	// AttachMode selects how Run gets from the downloaded/staged source
	// to something partclone can read: AttachModeNBD (also the default,
	// for an empty value) attaches the source with qemu-nbd and reads
	// straight from the kernel block device, writing no part-N.raw slice
	// or disk.raw conversion copy to WorkDir; AttachModeCopy is today's
	// unprivileged file-copy pipeline. See run().
	AttachMode string
}

// usesAttach reports whether cfg selects the nbd-attach path - true for
// AttachModeNBD and for an unset AttachMode, false only for
// AttachModeCopy.
func (cfg Config) usesAttach() bool {
	return cfg.AttachMode != AttachModeCopy
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
	// Attacher connects the source image to a kernel block device via
	// qemu-nbd. Required when Config.usesAttach() is true (the default);
	// nil is only valid for AttachModeCopy.
	Attacher Attacher
	// Statfs backs the work-directory space pre-flight check
	// (ensureScratchSpace) run before each partition's content is
	// cloned. nil means the real syscall.Statfs-backed implementation;
	// tests set it to simulate an arbitrary reported free-space figure.
	Statfs statfsFunc
}

// Attacher attaches a source disk image to a kernel block device with
// qemu-nbd, so Run can read partitions straight off a live device instead
// of copying bytes into scratch files (see AttachModeNBD). The
// exec-backed implementation lives in cmd/ingest; tests use a fake.
type Attacher interface {
	// Attach connects image (already known to be in the given format -
	// never left for qemu-nbd to probe) read-only to a free nbd device
	// and returns its path (e.g. "/dev/nbd0"). detach disconnects it;
	// the caller must call detach on every exit path, including error
	// paths, so a failed run never leaks a connected nbd device.
	Attach(ctx context.Context, image, format string) (dev string, detach func(), err error)
	// PartitionDevice returns the path of partition number num on dev
	// (e.g. "/dev/nbd0p1"), waiting a bounded time for the kernel to
	// create it. A node that never appears - most likely because the
	// nbd module was not loaded with max_part>0 - is reported as a clear
	// error rather than left to whatever cryptic failure partclone would
	// produce trying to open a missing device.
	PartitionDevice(ctx context.Context, dev string, num int) (string, error)
}

// Run executes the full ingest pipeline described in the package doc
// comment and returns a Result describing what happened. It never
// panics and never returns an error value: every failure becomes
// Result{Success: false, Error: ...}, because the only consumer
// (cmd/ingest) turns Run's outcome directly into the container's
// termination message and exit code. Run never touches the Kubernetes
// API: mapping a successful Result onto PartitionContent objects and an
// Image is the ImageImport controller's job.
func Run(ctx context.Context, cfg Config, deps Dependencies) Result {
	disk, err := run(ctx, cfg, deps)
	if err != nil {
		return FailureResult(err)
	}
	return Result{Success: true, Disk: disk}
}

func run(ctx context.Context, cfg Config, deps Dependencies) (*ResultDisk, error) {
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

	if cfg.usesAttach() {
		return runAttached(ctx, cfg, deps, sourcePath, info, staged)
	}

	rawPath, err := ensureRawDisk(ctx, cfg, deps, sourcePath, info.Format, staged)
	if err != nil {
		return nil, err
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

	partitions := make([]ResultPartition, 0, len(parsed.Partitions))
	for _, part := range parsed.Partitions {
		resultPart, err := processPartition(ctx, cfg, deps, rawPath, part)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", part.Number, err)
		}
		partitions = append(partitions, resultPart)
	}

	// rawPath is only read while extracting partition slices above;
	// nothing needs it once the last one is done. A staged raw source
	// (rawPath == sourcePath, no conversion ran) lives on the read-only
	// staging volume, not WorkDir, and is left alone here - staging's own
	// cleanup, below, is what removes it.
	if !staged || rawPath != sourcePath {
		if err := os.Remove(rawPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove %s: %w", rawPath, err)
		}
	}

	if staged && deps.StagedRemover != nil {
		// Best-effort: the content is already durably stored at this
		// point, so a cleanup failure must not fail an otherwise
		// successful ingest. A leftover staged upload is at worst
		// wasted staging space, not a correctness problem.
		_ = CleanupStagedSource(deps.StagedRemover, StagedNameFromURL(cfg.SourceURL))
	}

	compactSfdisk, err := compactJSON(sfdiskJSON)
	if err != nil {
		return nil, fmt.Errorf("compact partition table dump: %w", err)
	}

	return &ResultDisk{
		SizeBytes:      diskSize,
		PartitionTable: parsed.PartitionTable,
		SfdiskJSON:     compactSfdisk,
		Partitions:     partitions,
	}, nil
}

// runAttached is run()'s nbd-attach path (Config.usesAttach() true): it
// connects sourcePath to a kernel block device instead of converting it
// to a raw scratch file, dumps the partition table straight off that
// device, and clones each partition from its kernel-created partition
// device node. No disk.raw conversion copy and no part-N.raw slice ever
// exist in WorkDir - only each partition's own content-N output does.
func runAttached(ctx context.Context, cfg Config, deps Dependencies, sourcePath string, info QemuImgInfo, staged bool) (*ResultDisk, error) {
	dev, detach, err := deps.Attacher.Attach(ctx, sourcePath, info.Format)
	if err != nil {
		return nil, fmt.Errorf("attach source via nbd: %w", err)
	}
	defer detach()

	sfdiskJSON, err := deps.Sfdisk.Dump(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("dump partition table: %w", err)
	}
	parsed, err := ParseSfdiskJSON(sfdiskJSON)
	if err != nil {
		return nil, fmt.Errorf("parse partition table: %w", err)
	}

	partitions := make([]ResultPartition, 0, len(parsed.Partitions))
	for _, part := range parsed.Partitions {
		partDev, err := deps.Attacher.PartitionDevice(ctx, dev, int(part.Number))
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", part.Number, err)
		}
		resultPart, err := processPartitionDevice(ctx, cfg, deps, partDev, part)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", part.Number, err)
		}
		partitions = append(partitions, resultPart)
	}

	// Every partition has been read by now (partclone, or blkid alone
	// for swap) - the deferred detach above only runs once this function
	// returns, so it is safe to remove the downloaded source file here,
	// same as the copy path does once it no longer needs it.
	if !staged {
		if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove %s: %w", sourcePath, err)
		}
	}

	if staged && deps.StagedRemover != nil {
		_ = CleanupStagedSource(deps.StagedRemover, StagedNameFromURL(cfg.SourceURL))
	}

	compactSfdisk, err := compactJSON(sfdiskJSON)
	if err != nil {
		return nil, fmt.Errorf("compact partition table dump: %w", err)
	}

	return &ResultDisk{
		SizeBytes:      info.VirtualSizeBytes,
		PartitionTable: parsed.PartitionTable,
		SfdiskJSON:     compactSfdisk,
		Partitions:     partitions,
	}, nil
}

// compactJSON re-serializes data with no insignificant whitespace. The
// sfdisk dump travels back to the controller inside a container
// termination message, which Kubernetes caps at
// TerminationMessageLimit - sfdisk's own pretty-printed output spends a
// large part of that budget on indentation.
func compactJSON(data []byte) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ensureRawDisk returns the path to a raw disk image ingest can slice
// partitions out of: sourcePath itself, unchanged, when sourceFormat is
// already "raw" (a source this close to its final shape needs no
// conversion copy - doubling the ingest scratch volume's peak usage for
// nothing), or a freshly converted WorkDir/disk.raw file otherwise.
//
// When a conversion does run, sourcePath is removed immediately
// afterward if it is ingest's own downloaded copy (not staged): once
// disk.raw exists, source.img is fully redundant, and deleting it now -
// rather than waiting for every partition to be processed - keeps the
// scratch volume's peak usage from ever holding source.img, disk.raw,
// and a partition slice all at once.
func ensureRawDisk(ctx context.Context, cfg Config, deps Dependencies, sourcePath, sourceFormat string, staged bool) (string, error) {
	if strings.EqualFold(sourceFormat, "raw") {
		return sourcePath, nil
	}

	rawPath := filepath.Join(cfg.WorkDir, "disk.raw")
	if err := deps.QemuImg.ConvertToRaw(ctx, sourcePath, sourceFormat, rawPath); err != nil {
		return "", fmt.Errorf("convert source image to raw: %w", err)
	}

	if !staged {
		if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove converted source %s: %w", sourcePath, err)
		}
	}
	return rawPath, nil
}

// processPartition slices one partition out of the converted raw disk,
// detects its file system, classifies its role, and - for every role
// except swap - clones its content into its own scratch content
// directory under cfg.WorkDir. It returns the compact result summary for
// that partition.
func processPartition(ctx context.Context, cfg Config, deps Dependencies, rawPath string, part ParsedPartition) (ResultPartition, error) {
	slicePath := filepath.Join(cfg.WorkDir, fmt.Sprintf("part-%d.raw", part.Number))
	if err := extractPartition(rawPath, part.StartBytes, part.SizeBytes, slicePath, cfg.IOBandwidthBytesPerSec); err != nil {
		return ResultPartition{}, fmt.Errorf("extract partition slice: %w", err)
	}
	// The slice is a scratch copy of rawPath's own bytes: once this
	// partition's clone (or, for swap, its file system detection below)
	// has read it, nothing needs it again. Removing it here - rather
	// than at the end of Run - keeps at most one partition's slice on
	// the scratch volume at a time instead of one per partition.
	defer func() { _ = os.Remove(slicePath) }()

	return finishPartition(ctx, cfg, deps, slicePath, part)
}

// processPartitionDevice is runAttached's counterpart to processPartition:
// partDev is already a live partition device node (e.g. "/dev/nbd0p1"),
// so there is no slice to extract or clean up - blkid and partclone read
// it directly.
func processPartitionDevice(ctx context.Context, cfg Config, deps Dependencies, partDev string, part ParsedPartition) (ResultPartition, error) {
	return finishPartition(ctx, cfg, deps, partDev, part)
}

// finishPartition is the part of processing one partition that is
// identical whether its bytes come from a scratch slice file
// (processPartition) or a live nbd partition device node
// (processPartitionDevice): detect its file system, classify its role,
// and - for every role except swap - clone its content into its own
// scratch content directory under cfg.WorkDir.
func finishPartition(ctx context.Context, cfg Config, deps Dependencies, source string, part ParsedPartition) (ResultPartition, error) {
	fsInfo, err := deps.Blkid.Detect(ctx, source)
	if err != nil {
		return ResultPartition{}, fmt.Errorf("detect file system: %w", err)
	}

	role := classifyRole(part.TypeGUID, fsInfo.FSType)
	resultPart := ResultPartition{
		Number:    part.Number,
		Role:      role,
		FSType:    fsInfo.FSType,
		SizeBytes: part.SizeBytes,
		TypeGUID:  part.TypeGUID,
		PartUUID:  part.PartUUID,
	}

	if role == keziov1alpha3.PartitionRoleSwap {
		// Swap carries no content: the agent runs mkswap with the
		// recorded UUID at deploy time instead of restoring bytes.
		resultPart.FSType = ""
		resultPart.UUID = fsInfo.UUID
		return resultPart, nil
	}

	// Fail fast, before partclone writes any content byte into the work
	// directory, if it does not currently have enough available space
	// for this partition's content (see ensureScratchSpace for the
	// estimate this compares against). A clear error here feeds the
	// same failure path as every other step of this pipeline, rather
	// than partclone dying mid-write with a harder-to-diagnose ENOSPC.
	if err := ensureScratchSpace(deps.Statfs, cfg.WorkDir, part.SizeBytes); err != nil {
		return ResultPartition{}, fmt.Errorf("check ingest scratch space: %w", err)
	}

	contentDir := filepath.Join(cfg.WorkDir, fmt.Sprintf("content-%d", part.Number))
	if err := os.MkdirAll(contentDir, 0o750); err != nil {
		return ResultPartition{}, fmt.Errorf("create content scratch dir: %w", err)
	}
	if err := deps.Partclone.Clone(ctx, fsInfo.FSType, source, contentDir); err != nil {
		return ResultPartition{}, fmt.Errorf("clone partition content: %w", err)
	}

	usedBytes, lastExtentEnd, err := finalizeContent(contentDir)
	if err != nil {
		return ResultPartition{}, fmt.Errorf("finalize partition content: %w", err)
	}
	resultPart.UsedBytes = usedBytes
	resultPart.LastExtentEnd = lastExtentEnd
	resultPart.PieceLength = store.PieceSize

	return resultPart, nil
}

// finalizeContent nests partclone's flat extent-file output into
// contentDir's content/ data subdirectory (see store.NestExtentFiles) and
// validates the result against its own torrent.info. contentDir stays
// under cfg.WorkDir - not moved anywhere - since a content's own PVC does
// not exist until the controller has created its PartitionContent (see
// publish.go). It computes no info hash: the hash is observed once, by
// the publish step, from the bytes that actually reach the PVC.
func finalizeContent(contentDir string) (usedBytes, lastExtentEnd int64, err error) {
	torrentInfo, err := store.LoadContentDirTorrentInfo(contentDir)
	if err != nil {
		return 0, 0, err
	}
	// partclone -T writes torrent.info and every extent file flat,
	// directly inside contentDir (see internal/ingest.Partclone's doc
	// comment - it has no option to nest its own output). Move the
	// extent files into contentDir's content/ data subdirectory before
	// validating, so the directory matches what BuildInfoDict's torrent
	// file entries resolve to on a real BitTorrent v1 client (see
	// store.ContentDataDir).
	if err := store.NestExtentFiles(contentDir, torrentInfo); err != nil {
		return 0, 0, err
	}
	if err := store.ValidateContentDir(contentDir, torrentInfo); err != nil {
		return 0, 0, err
	}

	for _, e := range torrentInfo.Extents {
		usedBytes += int64(e.Length) //nolint:gosec // extent lengths are bounded by real partition sizes
		if end := int64(e.Offset + e.Length); end > lastExtentEnd {
			lastExtentEnd = end
		}
	}

	return usedBytes, lastExtentEnd, nil
}

// extractPartition copies the byte range [start, start+size) of src into
// a new file at dst. This is how ingest turns one slot of the converted
// raw disk into the standalone file partclone reads as its source: a
// plain Go file copy, needing no loop device, no nbd, no elevated
// privilege of any kind (see the package doc comment). bytesPerSec caps
// the write rate (see newThrottledWriter); 0 or negative leaves it
// unthrottled.
func extractPartition(src string, start, size int64, dst string, bytesPerSec int64) (err error) {
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

	if _, err := io.CopyN(NewThrottledWriter(out, bytesPerSec), in, size); err != nil {
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
