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
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tjjh89017/kezio/internal/ingest"
)

// blkidNoSignatureExitCode is the exit status blkid uses to report "no
// recognizable file system signature found" - not a tool failure, just
// an unrecognized (or unformatted) partition, which Run treats as an
// unknown file system and falls back to partclone.dd for.
const blkidNoSignatureExitCode = 2

// execBlkid implements ingest.Blkid by shelling out to blkid against a
// partition slice file. Like qemu-img and sfdisk, blkid reads a regular
// file the same way it reads a block device, so this needs no privilege.
type execBlkid struct{}

func (execBlkid) Detect(ctx context.Context, path string) (ingest.FSInfo, error) {
	fsType, found, err := blkidValue(ctx, path, "TYPE")
	if err != nil {
		return ingest.FSInfo{}, err
	}
	if !found {
		return ingest.FSInfo{}, nil
	}

	uuid, _, err := blkidValue(ctx, path, "UUID")
	if err != nil {
		return ingest.FSInfo{}, err
	}
	return ingest.FSInfo{FSType: fsType, UUID: uuid}, nil
}

// blkidValue runs `blkid -o value -s <tag> <path>` and returns its
// single-line output. found is false (with no error) when blkid exits
// with blkidNoSignatureExitCode: that means "no file system found",
// which is an expected outcome, not a failure.
func blkidValue(ctx context.Context, path, tag string) (value string, found bool, err error) {
	//nolint:gosec // path is an ingest-controlled scratch path, not user input
	cmd := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", tag, path)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == blkidNoSignatureExitCode {
			return "", false, nil
		}
		return "", false, fmt.Errorf("blkid -s %s %s: %w", tag, path, err)
	}
	return strings.TrimSpace(string(out)), true, nil
}
