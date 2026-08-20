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

// Command kezio-ingest is the binary the Image and PartitionContent
// controllers run inside a Job pod, selected by INGEST_MODE: "ingest"
// (internal/controller's buildIngestJob) resolves an Image's source,
// normalizes it, and captures its partition layout and content into a
// scratch work directory; "publish" (buildPublishJob) copies one
// partition's already-ingested content into its own PartitionContent PVC
// and builds its .torrent file. See buildIngestJob's and buildPublishJob's
// doc comments (internal/controller/image_ingest.go,
// partitioncontent_job.go) for the authoritative env var contract.
//
// Privilege requirements: none. Every external tool this binary shells
// out to (qemu-img, sfdisk, blkid, partclone.<fs>) reads and writes plain
// files - a downloaded/staged source file, a converted raw disk file,
// and per-partition slice files this binary extracts with a plain Go
// file copy (see internal/ingest's package doc comment). No nbd
// attach, no loop device, no CAP_SYS_ADMIN, no privileged container is
// needed. This binary also never talks to the Kubernetes API in either
// mode - internal/ingest.Run and RunPublish only touch mounted volumes
// (see their doc comments); the controllers map a successful Result onto
// PartitionContent objects themselves.
//
// Result handoff: this binary always writes an internal/ingest.Result as
// JSON to its container's termination message path (default
// /dev/termination-log, overridable with TERMINATION_MESSAGE_PATH, which
// must match the pod spec's terminationMessagePath) before exiting -
// success or failure alike - and exits 0 on success, 1 otherwise.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tjjh89017/kezio/internal/imageservice"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

const defaultTerminationMessagePath = "/dev/termination-log"

func main() {
	os.Exit(run())
}

// run dispatches to the ingest pipeline or the publish step, selected by
// INGEST_MODE ("publish" opts into the latter; anything else, including
// unset, runs the former). It returns the process exit code rather than
// calling os.Exit itself, so it stays testable in the same package.
func run() int {
	if os.Getenv("INGEST_MODE") == "publish" {
		return runPublish()
	}
	return runIngest()
}

// runIngest builds a Config/Dependencies pair from the environment and
// runs internal/ingest.Run.
func runIngest() int {
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

// runPublish builds a PublishConfig from the environment and runs
// internal/ingest.RunPublish.
func runPublish() int {
	cfg, err := publishConfigFromEnv()
	if err != nil {
		log.Printf("kezio-ingest publish: %v", err)
		writeResult(ingest.FailureResult(err))
		return 1
	}

	result := ingest.RunPublish(cfg)
	writeResult(result)
	if !result.Success {
		log.Printf("kezio-ingest publish: failed: %s", result.Error)
		return 1
	}
	return 0
}

// publishConfigFromEnv reads TRACKER_URL, PARTITION_CONTENT_HASH, and
// SOURCE_CONTENT_DIR - what buildPublishJob sets - into a PublishConfig
// naming exactly the one partition this publish Job runs for.
// DestDir is derived from PARTITION_CONTENT_HASH via
// ingest.ContentMountPath, the same convention buildPublishJob used to
// mount the content PVC, so this needs no separate "where do I write"
// input.
func publishConfigFromEnv() (ingest.PublishConfig, error) {
	trackerURL := os.Getenv("TRACKER_URL")
	hashStr := os.Getenv("PARTITION_CONTENT_HASH")
	sourceDir := os.Getenv("SOURCE_CONTENT_DIR")
	if trackerURL == "" || hashStr == "" || sourceDir == "" {
		return ingest.PublishConfig{}, fmt.Errorf(
			"missing required environment: TRACKER_URL=%q PARTITION_CONTENT_HASH=%q SOURCE_CONTENT_DIR=%q",
			trackerURL, hashStr, sourceDir)
	}

	hash, err := store.ParseInfoHash(hashStr)
	if err != nil {
		return ingest.PublishConfig{}, fmt.Errorf("invalid PARTITION_CONTENT_HASH %q: %w", hashStr, err)
	}

	return ingest.PublishConfig{
		Partitions: []ingest.PublishPartition{{
			SourceDir: sourceDir,
			DestDir:   ingest.ContentMountPath(hash),
		}},
		TrackerURL: trackerURL,
	}, nil
}

// buildFromEnv reads the environment variables buildIngestJob sets on
// the ingest Job's container and wires up the real, exec-backed
// Dependencies.
func buildFromEnv() (ingest.Config, ingest.Dependencies, error) {
	sourceURL := os.Getenv("SOURCE_URL")
	sourceFormat := os.Getenv("SOURCE_FORMAT")
	if sourceURL == "" || sourceFormat == "" {
		return ingest.Config{}, ingest.Dependencies{}, fmt.Errorf(
			"missing required environment: SOURCE_URL=%q SOURCE_FORMAT=%q", sourceURL, sourceFormat)
	}

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = ingest.DefaultWorkDir
	}
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return ingest.Config{}, ingest.Dependencies{}, fmt.Errorf("create work dir %s: %w", workDir, err)
	}

	cfg := ingest.Config{
		SourceURL:      sourceURL,
		SourceFormat:   sourceFormat,
		SourceChecksum: os.Getenv("SOURCE_CHECKSUM"),
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
