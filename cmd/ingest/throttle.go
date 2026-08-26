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
	"log"
	"os/exec"
)

// throttledCommand builds the command to run binary with args, wrapped
// in `ionice -c2 -n7` (best-effort/idle I/O priority) and `nice -n 19`
// (lowest CPU priority) when those tools are on PATH. This keeps
// qemu-img convert and partclone - the two exec-backed steps that can
// each write a whole disk's worth of bytes - from starving whatever else
// is sharing the node's disk (see internal/ingest.NewThrottledWriter for
// the write-rate cap this package applies to the file copies it performs
// itself instead of shelling out).
//
// A missing ionice or nice is logged and skipped rather than failing the
// pipeline: it degrades to running binary unthrottled, not to a failed
// ingest.
func throttledCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := append([]string{binary}, args...)

	if _, err := exec.LookPath("nice"); err == nil {
		cmd = append([]string{"nice", "-n", "19"}, cmd...)
	} else {
		log.Printf("kezio-ingest: nice not found on PATH, running %s at default CPU priority", binary)
	}
	if _, err := exec.LookPath("ionice"); err == nil {
		cmd = append([]string{"ionice", "-c2", "-n7"}, cmd...)
	} else {
		log.Printf("kezio-ingest: ionice not found on PATH, running %s without I/O priority throttling", binary)
	}

	//nolint:gosec // cmd[0] is "ionice", "nice", or binary itself - all resolved from a fixed set, never external input
	return exec.CommandContext(ctx, cmd[0], cmd[1:]...)
}
