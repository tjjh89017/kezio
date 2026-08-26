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
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nbdDeviceDir and nbdSysBlockDir are where the nbd kernel module exposes
// its devices: /dev/nbdN is the block device qemu-nbd connects to, and
// /sys/block/nbdN/pid exists only while something is connected to it (it
// holds the PID of the process that issued NBD_DO_IT) - the same signal
// `qemu-nbd -c` itself would use to pick a free device if asked to scan.
const (
	nbdDeviceDir   = "/dev"
	nbdSysBlockDir = "/sys/block"
)

// partitionDeviceWait bounds how long PartitionDevice waits for the
// kernel to create a partition device node after Attach connects -
// ordinarily near-instant once max_part>0, but not a hard guarantee.
const partitionDeviceWait = 10 * time.Second

// partitionDevicePollInterval is how often PartitionDevice re-checks for
// the device node while waiting.
const partitionDevicePollInterval = 100 * time.Millisecond

// execAttacher implements ingest.Attacher by shelling out to qemu-nbd.
// Unlike every other exec-backed dependency in this package, this one
// needs real privilege: CAP_SYS_ADMIN (qemu-nbd's NBD_SET_SOCK/NBD_DO_IT
// ioctls) and access to /dev's nbd device nodes, both only present when
// IMAGE_INGEST_ATTACH selects nbd (see internal/controller's Job
// builder, which grants them only for that mode).
type execAttacher struct{}

// Attach picks a free /dev/nbdN, connects image to it read-only with
// qemu-nbd - passing format explicitly so qemu-nbd never probes the
// image's format itself - and returns detach, which disconnects it.
func (execAttacher) Attach(ctx context.Context, image, format string) (string, func(), error) {
	dev, err := findFreeNBDDevice()
	if err != nil {
		return "", nil, err
	}

	//nolint:gosec // dev is chosen from /dev/nbd* by this process, not user input; image/format come from Config
	cmd := exec.CommandContext(ctx, "qemu-nbd", "--read-only", "--connect="+dev, "--format="+format, image)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("qemu-nbd --connect=%s --format=%s %s: %w: %s", dev, format, image, err, out)
	}

	detach := func() {
		out, err := exec.Command("qemu-nbd", "--disconnect", dev).CombinedOutput() //nolint:gosec // dev is our own chosen path
		if err != nil {
			log.Printf("kezio-ingest: qemu-nbd --disconnect %s: %v: %s", dev, err, out)
		}
	}

	// max_part>0 makes the kernel scan the partition table as part of
	// registering the device, but nudge it explicitly: cheap, idempotent,
	// and covers a kernel that raced the scan against qemu-nbd's own
	// connect completing.
	if out, err := exec.CommandContext(ctx, "blockdev", "--rereadpt", dev).CombinedOutput(); err != nil { //nolint:gosec // dev is our own chosen path
		log.Printf("kezio-ingest: blockdev --rereadpt %s: %v: %s", dev, err, out)
	}

	return dev, detach, nil
}

// PartitionDevice waits for /dev/<base of dev>p<num> to appear, polling
// at partitionDevicePollInterval up to partitionDeviceWait.
func (execAttacher) PartitionDevice(ctx context.Context, dev string, num int) (string, error) {
	path := fmt.Sprintf("%sp%d", dev, num)
	deadline := time.Now().Add(partitionDeviceWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf(
				"partition device %s did not appear within %s after attaching %s "+
					"(is the nbd kernel module loaded with max_part>0, e.g. modprobe nbd max_part=16?)",
				path, partitionDeviceWait, dev)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(partitionDevicePollInterval):
		}
	}
}

// findFreeNBDDevice returns the path of the lowest-numbered /dev/nbdN
// with no /sys/block/nbdN/pid - the kernel's signal that nothing is
// currently connected to it.
func findFreeNBDDevice() (string, error) {
	entries, err := filepath.Glob(filepath.Join(nbdDeviceDir, "nbd[0-9]*"))
	if err != nil {
		return "", fmt.Errorf("list %s/nbd*: %w", nbdDeviceDir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return nbdDeviceNumber(entries[i]) < nbdDeviceNumber(entries[j])
	})

	for _, dev := range entries {
		name := filepath.Base(dev)
		// A partition device (nbd0p1) never has its own pid file; only
		// the base device does, but filtering here is cheap and makes
		// the intent explicit regardless of what the glob matched.
		if strings.Contains(name, "p") {
			continue
		}
		pidPath := filepath.Join(nbdSysBlockDir, name, "pid")
		if _, err := os.Stat(pidPath); err != nil {
			return dev, nil
		}
	}
	return "", fmt.Errorf(
		"no free /dev/nbd* device found (is the nbd kernel module loaded? modprobe nbd max_part=16)")
}

// nbdDeviceNumber extracts N from a "/dev/nbdN" path, for sorting
// numerically rather than lexically (nbd10 must sort after nbd2).
func nbdDeviceNumber(dev string) int {
	name := filepath.Base(dev)
	digits := strings.TrimPrefix(name, "nbd")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return -1
	}
	return n
}
