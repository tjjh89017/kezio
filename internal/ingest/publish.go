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
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tjjh89017/kezio/internal/store"
)

// PublishConfig configures one run of the publish step: copying already
// validated partition content out of the ingest scratch root (where the
// main ingest run wrote and content-addressed it - see Run) into each
// partition's own destination directory, a distinct PVC mount the
// per-Image seeder Deployment will later mount read-only. This is a
// separate Job from the main ingest run because the destination PVCs do
// not exist yet when that run starts: their count and size are only
// known once ingest has reported which partitions it found (see
// internal/controller's reconcilePublishing).
type PublishConfig struct {
	// ScratchRoot is the ingest scratch volume's mounted root - the same
	// STORE_ROOT the main ingest run wrote into, mounted read-only for
	// this run.
	ScratchRoot string
	// Partitions lists which content to copy where.
	Partitions []PublishPartition
}

// PublishPartition names one partition's content (by info hash, as
// found under ScratchRoot's contents/ tree) and the destination
// directory to copy it into - that partition's own PVC mount root.
type PublishPartition struct {
	Number   int32
	InfoHash string
	DestDir  string
}

// PartitionMountPath returns the deterministic in-pod mount path for one
// partition's content volume. cmd/ingest (this publish step's writer)
// and internal/controller (the per-Image seeder Deployment's reader -
// see its own partitionMountPath, which delegates here) both use this
// one function, so the convention cannot drift between them.
func PartitionMountPath(number int32) string {
	return fmt.Sprintf("/content/%d", number)
}

// RunPublish copies every partition in cfg.Partitions from the ingest
// scratch root into its own destination directory, validating each copy
// against its torrent.info before considering it done. It never panics
// and never returns an error value, for the same reason Run does not
// (see Run's doc comment): cmd/ingest turns the outcome directly into a
// termination message and exit code.
func RunPublish(cfg PublishConfig) Result {
	if err := runPublish(cfg); err != nil {
		return FailureResult(err)
	}
	return Result{Success: true}
}

func runPublish(cfg PublishConfig) error {
	for _, p := range cfg.Partitions {
		if err := publishPartition(cfg.ScratchRoot, p); err != nil {
			return fmt.Errorf("partition %d: %w", p.Number, err)
		}
	}
	return nil
}

// publishPartition copies one partition's content from the scratch
// root's content-addressed store into destDir, then validates the copy
// against the torrent.info it just copied alongside it - so a truncated
// or otherwise corrupted copy is caught here rather than surfacing later
// as a leecher's hash-check failure.
func publishPartition(scratchRoot string, p PublishPartition) error {
	hash, err := store.ParseInfoHash(p.InfoHash)
	if err != nil {
		return fmt.Errorf("parse info hash: %w", err)
	}
	srcDir := store.ContentDir(scratchRoot, hash)

	info, err := store.LoadContentDirTorrentInfo(srcDir)
	if err != nil {
		return fmt.Errorf("load source torrent.info: %w", err)
	}

	if err := copyDir(srcDir, p.DestDir); err != nil {
		return fmt.Errorf("copy content: %w", err)
	}

	if err := store.ValidateContentDir(p.DestDir, info); err != nil {
		return fmt.Errorf("validate published content: %w", err)
	}
	return nil
}

// copyDir recursively copies src's regular files and directories into
// dst, creating dst if necessary. It follows no symlinks (the store's
// own layout never creates any - see internal/store's package doc
// comment), which keeps this from needing to reason about link cycles or
// escaping dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unexpected non-regular file %s", path)
		}
		return copyFile(path, target)
	})
}

// copyFile copies one regular file's contents from src to dst, creating
// dst's parent directory if necessary.
func copyFile(src, dst string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", dst, err)
	}

	in, err := os.Open(src) //nolint:gosec // src is a store-controlled scratch path
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // dst is a store-controlled destination path
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
	return nil
}
