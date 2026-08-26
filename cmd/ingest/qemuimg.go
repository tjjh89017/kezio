/*
Copyright 2026.

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
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/tjjh89017/kezio/internal/ingest"
)

// execQemuImg implements ingest.QemuImg by shelling out to the qemu-img
// binary. It runs entirely unprivileged: qemu-img reads and writes plain
// files, never a block device.
type execQemuImg struct{}

type qemuImgInfoOutput struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
}

func (execQemuImg) Info(ctx context.Context, path string) (ingest.QemuImgInfo, error) {
	//nolint:gosec // path is an ingest-controlled scratch/staging path, not user input
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path)
	out, err := cmd.Output()
	if err != nil {
		return ingest.QemuImgInfo{}, fmt.Errorf("qemu-img info %s: %w", path, err)
	}

	var parsed qemuImgInfoOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ingest.QemuImgInfo{}, fmt.Errorf("parse qemu-img info output for %s: %w", path, err)
	}
	return ingest.QemuImgInfo{Format: parsed.Format, VirtualSizeBytes: parsed.VirtualSize}, nil
}

func (execQemuImg) ConvertToRaw(ctx context.Context, src, srcFormat, dst string) error {
	// Converting a whole disk can write gigabytes; run it at low I/O/CPU
	// priority (see throttledCommand) so it does not starve the node's
	// disk for anything else sharing it.
	cmd := throttledCommand(ctx, "qemu-img", "convert", "-f", srcFormat, "-O", "raw", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert %s -> %s: %w: %s", src, dst, err, out)
	}
	return nil
}
