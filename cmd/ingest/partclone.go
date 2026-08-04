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

package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// ddFallbackBinary is the partclone binary used for a file system it
// does not have a dedicated backend for: it clones every sector rather
// than just used blocks, at the cost of no space savings (see the
// package doc comment).
const ddFallbackBinary = "partclone.dd"

// blkidToPartcloneFSType maps a handful of blkid TYPE values that do not
// match their partclone binary suffix one-for-one. Every other
// blkid-reported type is tried directly as "partclone.<type>" first
// (this covers ext2/ext3/ext4/xfs/btrfs/ntfs/exfat/f2fs/hfsplus/jfs/
// nilfs2/ufs/vfat, which all have a same-named partclone binary); if
// that binary is not on PATH, execPartclone falls back to
// ddFallbackBinary automatically, so this map only needs the exceptions.
var blkidToPartcloneFSType = map[string]string{
	"msdos": "fat",
}

// execPartclone implements ingest.Partclone by shelling out to the
// partclone.<fs> family of binaries (or partclone.dd) against a
// partition slice file - a plain regular file, not a block device (see
// the package doc comment on why ingest slices partitions out first
// instead of attaching the whole disk). This needs no elevated
// privilege: partclone opens a regular file exactly like a block
// device, so no privileged access is required.
type execPartclone struct{}

func (execPartclone) Clone(ctx context.Context, fsType, source, targetDir string) error {
	binary := partcloneBinary(fsType)

	// partclone defaults its logfile to /var/log/partclone.log and
	// always opens it (`open_log` in partclone.c), exiting immediately
	// if the open fails; the ingest container runs as nonroot uid
	// 65532, which cannot write to /var/log. -L/--logfile redirects it
	// next to targetDir, inside the already-writable work dir, named
	// after the partition's content directory so concurrent partitions
	// (if this ever parallelizes) cannot collide on one logfile.
	logPath := filepath.Join(filepath.Dir(targetDir), "partclone."+filepath.Base(targetDir)+".log")

	//nolint:gosec // binary is chosen from a fixed set below; source/targetDir/logPath are ingest-controlled scratch paths
	cmd := exec.CommandContext(ctx, binary, "-c", "-F", "-s", source, "-o", targetDir, "-T", "-L", logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s -c -s %s -o %s -L %s: %w: %s", binary, source, targetDir, logPath, err, out)
	}
	return nil
}

// partcloneBinary picks the partclone binary for fsType: the directly
// named one if it exists on PATH (after applying a known alias), or
// ddFallbackBinary otherwise. An empty fsType (blkid found no
// signature) goes straight to the fallback.
func partcloneBinary(fsType string) string {
	if fsType == "" {
		return ddFallbackBinary
	}

	suffix := fsType
	if alias, ok := blkidToPartcloneFSType[fsType]; ok {
		suffix = alias
	}

	candidate := "partclone." + suffix
	if _, err := exec.LookPath(candidate); err != nil {
		return ddFallbackBinary
	}
	return candidate
}
