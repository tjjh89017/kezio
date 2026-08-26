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

// Command kezio-ingest is the binary the ImageImport and PartitionContent
// controllers run inside a Job pod, selected by INGEST_MODE: "ingest"
// (internal/controller's buildIngestJob) resolves an ImageImport's
// source, normalizes it, and captures its partition layout and content
// into a scratch work directory; "publish" (buildPublishJob) copies one
// partition's already-ingested content into its own PartitionContent PVC.
// See buildIngestJob's and buildPublishJob's doc comments
// (internal/controller/imageimport_ingest.go, partitioncontent_job.go)
// for the authoritative env var contract.
//
// Privilege requirements: depend on IMAGE_INGEST_ATTACH (see
// ingest.Config.AttachMode). Left unset or set to "nbd" - the default -
// this binary attaches the source image to a kernel nbd device with
// qemu-nbd (execAttacher) and needs CAP_SYS_ADMIN, access to /dev, and
// the "nbd" kernel module loaded on the node with max_part>0; see
// internal/controller's Job builder for the exact securityContext this
// grants, and docs/crd-reference.md for the node requirement. Set to
// "copy", every external tool this binary shells out to (qemu-img,
// sfdisk, blkid, partclone.<fs>) instead reads and writes plain files - a
// downloaded/staged source file, a converted raw disk file, and
// per-partition slice files this binary extracts with a plain Go file
// copy (see internal/ingest's package doc comment) - needing no elevated
// privilege at all; that mode exists for clusters that cannot run a
// privileged ingest Job. This binary also never talks to the Kubernetes
// API in either mode - internal/ingest.Run and RunPublish only touch
// mounted volumes (see their doc comments); the controllers map a
// successful Result onto Kubernetes objects themselves.
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
	"strconv"

	"github.com/tjjh89017/kezio/internal/imageservice"
	"github.com/tjjh89017/kezio/internal/ingest"
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

	result := boundResult(ingest.Run(context.Background(), cfg, deps))
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

// publishConfigFromEnv reads PARTITION_CONTENT_NAME and
// SOURCE_CONTENT_DIR - what buildPublishJob sets - into a PublishConfig
// naming exactly the one partition this publish Job runs for. No
// announce URL is read here: at publish time no Site is in scope, so
// there is no tracker to bake into anything (see ingest.PublishConfig's
// doc comment). DestDir is derived from PARTITION_CONTENT_NAME via
// ingest.ContentMountPath, the same convention buildPublishJob used to
// mount the content PVC, so this needs no separate "where do I write"
// input.
func publishConfigFromEnv() (ingest.PublishConfig, error) {
	contentName := os.Getenv("PARTITION_CONTENT_NAME")
	sourceDir := os.Getenv("SOURCE_CONTENT_DIR")
	if contentName == "" || sourceDir == "" {
		return ingest.PublishConfig{}, fmt.Errorf(
			"missing required environment: PARTITION_CONTENT_NAME=%q SOURCE_CONTENT_DIR=%q",
			contentName, sourceDir)
	}

	return ingest.PublishConfig{
		Partitions: []ingest.PublishPartition{{
			SourceDir: sourceDir,
			DestDir:   ingest.ContentMountPath(contentName),
		}},
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

	var bytesPerSec int64
	if v := os.Getenv("IO_BANDWIDTH_BYTES_PER_SEC"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			log.Printf("kezio-ingest: invalid IO_BANDWIDTH_BYTES_PER_SEC %q, running unthrottled", v)
		} else {
			bytesPerSec = n
		}
	}

	cfg := ingest.Config{
		SourceURL:              sourceURL,
		SourceFormat:           sourceFormat,
		SourceChecksum:         os.Getenv("SOURCE_CHECKSUM"),
		WorkDir:                workDir,
		IOBandwidthBytesPerSec: bytesPerSec,
		// IMAGE_INGEST_ATTACH unset behaves exactly like "nbd" - see
		// ingest.Config.usesAttach - so leaving cfg.AttachMode at its zero
		// value here already selects the right default; only "copy"
		// needs to be read explicitly at all.
		AttachMode: os.Getenv("IMAGE_INGEST_ATTACH"),
	}

	deps := ingest.Dependencies{
		Downloader: newHTTPDownloader(bytesPerSec),
		QemuImg:    execQemuImg{},
		Sfdisk:     execSfdisk{},
		Blkid:      execBlkid{},
		Partclone:  execPartclone{},
	}
	if cfg.AttachMode != ingest.AttachModeCopy {
		deps.Attacher = execAttacher{}
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

// boundResult turns a successful result that will not survive the
// container termination message cap into an explicit failure. The
// controller's only channel back from this Job is that message, and
// Kubernetes truncates it silently: a partition table large enough to
// push the payload over ingest.TerminationMessageLimit would otherwise
// reach the controller as unparseable JSON, reported as a corrupt result
// rather than as the size problem it is.
func boundResult(result ingest.Result) ingest.Result {
	if !result.Success {
		return result
	}
	data, err := ingest.MarshalResult(result)
	if err != nil {
		return ingest.FailureResult(err)
	}
	if len(data) > ingest.TerminationMessageLimit {
		return ingest.FailureResult(fmt.Errorf(
			"ingest result is %d bytes, over the %d-byte container termination message limit: "+
				"this disk's partition table dump is too large to hand back to the controller",
			len(data), ingest.TerminationMessageLimit))
	}
	return result
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
