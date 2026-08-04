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

// Command kezio-ingest is the orchestrator binary the Image controller
// runs inside a Job pod: it resolves an Image's source, normalizes it,
// captures its partition layout and content into the store, and reports
// the outcome back to the controller.
//
// Privilege requirements: none. Every external tool this binary shells
// out to (qemu-img, sfdisk, blkid, partclone.<fs>) reads and writes plain
// files - a downloaded/staged source file, a converted raw disk file,
// and per-partition slice files this binary extracts with a plain Go
// file copy (see internal/ingest's package doc comment). No nbd
// attach, no loop device, no CAP_SYS_ADMIN, no privileged container is
// needed; the Job's pod can run as an ordinary unprivileged container
// with the store and (when the source is staged) staging volumes
// mounted read-write.
//
// Result handoff: this binary always writes an internal/ingest.Result as
// JSON to its container's termination message path (default
// /dev/termination-log, overridable with TERMINATION_MESSAGE_PATH, which
// must match the pod spec's terminationMessagePath) before exiting -
// success or failure alike - and exits 0 on success, 1 otherwise. The
// Image reconciler never mounts the store or staging volumes itself; it
// reads this file back from the completed Job's pod status instead. See
// internal/ingest.Result's doc comment for why (the 4KiB termination
// message cap) this carries a compact summary rather than the full
// layout.json.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tjjh89017/kezio/internal/imageservice"
	"github.com/tjjh89017/kezio/internal/ingest"
)

const defaultTerminationMessagePath = "/dev/termination-log"

func main() {
	os.Exit(run())
}

// run builds Config/Dependencies from the environment, executes the
// ingest pipeline, and writes the result. It returns the process exit
// code rather than calling os.Exit itself, so it stays testable in the
// same package.
func run() int {
	cfg, deps, err := buildFromEnv()
	if err != nil {
		log.Printf("kezio-ingest: %v", err)
		writeResult(ingest.FailureResult(err))
		return 1
	}

	result := ingest.Run(context.Background(), cfg, deps)
	writeResult(result)
	if !result.Success {
		log.Printf("kezio-ingest: failed: %s", result.Error)
		return 1
	}
	return 0
}

// buildFromEnv reads the environment variables the Image controller sets
// on the ingest Job's container and wires up the real, exec-backed
// Dependencies.
func buildFromEnv() (ingest.Config, ingest.Dependencies, error) {
	imageName := os.Getenv("IMAGE_NAME")
	sourceURL := os.Getenv("SOURCE_URL")
	sourceFormat := os.Getenv("SOURCE_FORMAT")
	storeRoot := os.Getenv("STORE_ROOT")
	if imageName == "" || sourceURL == "" || sourceFormat == "" || storeRoot == "" {
		return ingest.Config{}, ingest.Dependencies{}, fmt.Errorf(
			"missing required environment: IMAGE_NAME=%q SOURCE_URL=%q SOURCE_FORMAT=%q STORE_ROOT=%q",
			imageName, sourceURL, sourceFormat, storeRoot)
	}

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "/work"
	}
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return ingest.Config{}, ingest.Dependencies{}, fmt.Errorf("create work dir %s: %w", workDir, err)
	}

	cfg := ingest.Config{
		ImageName:      imageName,
		SourceURL:      sourceURL,
		SourceFormat:   sourceFormat,
		SourceChecksum: os.Getenv("SOURCE_CHECKSUM"),
		StoreRoot:      storeRoot,
		WorkDir:        workDir,
	}

	deps := ingest.Dependencies{
		Downloader: newHTTPDownloader(),
		QemuImg:    execQemuImg{},
		Sfdisk:     execSfdisk{},
		Blkid:      execBlkid{},
		Partclone:  execPartclone{},
	}

	if stagingRoot := os.Getenv("STAGING_ROOT"); stagingRoot != "" {
		staging, err := imageservice.NewStaging(stagingRoot)
		if err != nil {
			return ingest.Config{}, ingest.Dependencies{}, fmt.Errorf("open staging volume at %s: %w", stagingRoot, err)
		}
		deps.Staging = staging
		deps.StagedRemover = staging
	}

	return cfg, deps, nil
}

// writeResult marshals result and writes it to the container termination
// message path, logging (rather than failing the process over) any
// error: the exit code already reflects success/failure on its own, and
// a write failure here must not mask whatever ingest itself reported.
func writeResult(result ingest.Result) {
	data, err := ingest.MarshalResult(result)
	if err != nil {
		log.Printf("kezio-ingest: marshal result: %v", err)
		return
	}

	path := os.Getenv("TERMINATION_MESSAGE_PATH")
	if path == "" {
		path = defaultTerminationMessagePath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		log.Printf("kezio-ingest: create termination message dir: %v", err)
		return
	}
	//nolint:gosec // termination message file is world-readable by kubelet design
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("kezio-ingest: write termination message: %v", err)
	}
}
