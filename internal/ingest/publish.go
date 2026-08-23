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

// ContentMountRoot is the directory a PartitionContent's own PVC is
// mounted at inside the publish step's pod. This is the data-plane
// contract the publish step, the seeder, and the e2e lane all share: a
// content's PVC mount path is derived from its info hash by name alone
// (see ContentMountPath), with no extra wiring needed to find it.
const ContentMountRoot = "/content"

// ContentMountPath returns the path a content's own PVC
// (store.PVCName(hash)) is mounted at inside a pod that has that PVC
// mounted under ContentMountRoot.
func ContentMountPath(hash store.InfoHash) string {
	return filepath.Join(ContentMountRoot, store.PVCName(hash))
}

// PublishConfig configures one run of the publish step: copying one
// partition's already-validated content out of the ingest scratch work
// directory Run filled (see processPartition) into that content's own
// PVC - mounted at ContentMountPath for the production case. This is a
// separate step from Run because a content's own PVC does not exist
// until the controller has created the PartitionContent object Run's
// Result named (see Result's doc comment).
//
// No announce URL is built or written here: at publish time no Site is
// in scope, so there is no tracker this content could correctly
// announce to. The seeder builds a .torrent from torrent.info (which
// this step does copy into the PVC) at serve time, once its own Site's
// tracker URL is known.
type PublishConfig struct {
	// Partitions lists which scratch content to publish where.
	Partitions []PublishPartition
}

// PublishPartition names one partition's scratch content directory (as
// Run left it under its WorkDir) and the destination directory to
// publish it into - that content's own PVC mount root.
type PublishPartition struct {
	Number    int32
	SourceDir string
	DestDir   string
}

// RunPublish copies every partition in cfg.Partitions from its scratch
// content directory into its own destination PVC, validating each copy
// against its torrent.info before considering it done. It never panics
// and never returns an error value,
// for the same reason Run does not (see Run's doc comment): cmd/ingest
// turns the outcome directly into a termination message and exit code.
func RunPublish(cfg PublishConfig) Result {
	if err := runPublish(cfg); err != nil {
		return FailureResult(err)
	}
	return Result{Success: true}
}

func runPublish(cfg PublishConfig) error {
	for _, p := range cfg.Partitions {
		if err := publishPartition(p); err != nil {
			return fmt.Errorf("partition %d: %w", p.Number, err)
		}
	}
	return nil
}

// publishPartition copies one partition's content from its scratch
// source directory into destDir, then validates the copy against the
// torrent.info it just copied alongside it - so a truncated or otherwise
// corrupted copy is caught here rather than surfacing later as a
// leecher's hash-check failure.
func publishPartition(p PublishPartition) error {
	info, err := store.LoadContentDirTorrentInfo(p.SourceDir)
	if err != nil {
		return fmt.Errorf("load source torrent.info: %w", err)
	}

	if err := copyDir(p.SourceDir, p.DestDir); err != nil {
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

	in, err := os.Open(src) //nolint:gosec // src is an ingest-controlled scratch path
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // dst is an ingest-controlled destination path
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
