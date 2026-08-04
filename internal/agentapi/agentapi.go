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

// Package agentapi defines the wire protocol between kezio-agent (running
// in the live boot environment) and the controller's agent registration
// endpoint (internal/agentserver). It holds only request/response types
// and their JSON shape - no HTTP client, no HTTP server, no
// controller-runtime dependency - so the agent binary that imports it
// stays free of everything the manager process needs (client-go,
// controller-runtime, and their transitive weight) that a binary running
// in a minimal live environment has no use for.
package agentapi

import (
	"fmt"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// RegisterPath and NextPathPrefix are the HTTP routes the agent talks to,
// relative to the kezio.server= base URL the kernel cmdline carries (see
// internal/bootserver's renderNetBootConfig). NextPathPrefix is a prefix,
// not a full path: the machine name the agent registered as follows it,
// for example "/agent/machines/node-01/next".
const (
	RegisterPath   = "/agent/register"
	NextPathPrefix = "/agent/machines/"
	NextPathSuffix = "/next"
	// ProgressPathSuffix follows NextPathPrefix + <machine name>, the same
	// way NextPathSuffix does: "/agent/machines/node-01/progress".
	ProgressPathSuffix = "/progress"
)

// RegisterRequest is the POST /agent/register request body: the full
// hardware inventory the agent collected from the live OS. It reuses
// keziov1alpha1.MachineHardwareStatus directly as the wire shape - the
// same struct Machine.status.hardware stores - instead of a parallel DTO,
// since the two are the same data by construction and a second type would
// only be able to drift out of sync with the first.
type RegisterRequest struct {
	Hardware keziov1alpha1.MachineHardwareStatus `json:"hardware"`
}

// RegisterResponse is the POST /agent/register success response.
type RegisterResponse struct {
	// MachineName is the name of the Machine the presented token
	// resolved to. The agent uses it to build the GET .../next URL.
	MachineName string `json:"machineName"`
	// SessionToken is a fresh, short-lived credential the agent presents
	// (as a Bearer token, the same as the boot token at /agent/register)
	// on every subsequent GET .../next call. It is distinct from the
	// single-use boot token that authenticated this request: that token
	// is already consumed by the time this response is written (see
	// internal/agentserver's ingestRegistration), so polling needs its
	// own credential. Only its SHA-256 hash is ever persisted
	// server-side (Machine.status.agentSession.tokenHash); this value is
	// returned exactly once and never recoverable again.
	SessionToken string `json:"sessionToken"`
}

// NextResponse is the GET /agent/machines/<name>/next response body.
type NextResponse struct {
	// Action is ActionWait (nothing to do yet) or ActionDeploy (Plan is
	// populated).
	Action string `json:"action"`
	// Plan is the deployment plan, present iff Action == ActionDeploy.
	Plan *DeployPlan `json:"plan,omitempty"`
}

// ActionWait is the NextResponse.Action value when there is nothing for
// the agent to do yet: the Machine has not reached Provisioning with
// every target disk resolved and every referenced Image Ready.
const ActionWait = "wait"

// ActionDeploy is the NextResponse.Action value when NextResponse.Plan
// is populated and ready to execute. Parsing and logging the plan is as
// far as this work item goes - the executor that actually applies it
// (sfdisk, torrent restore, mkfs/mkswap, finalize) is a later work item.
const ActionDeploy = "deploy"

// ErrorResponse is the JSON body returned alongside any non-2xx
// response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// DeployPlan is everything kezio-agent needs to lay down every image a
// Machine's spec asks for onto its resolved target disks. The controller
// (internal/agentserver) builds it fresh on every GET .../next call - it
// is never persisted to the Machine object - once the Machine is
// Provisioning with every target disk resolved
// (status.provisioning.{image,dataImages[]}.targetDisk) and every
// referenced Image Ready.
type DeployPlan struct {
	// OS is the OS image's plan, present when the Machine has an
	// imageRef.
	OS *ImageDeployPlan `json:"os,omitempty"`
	// DataImages is one plan per spec.dataImages entry, in the same
	// order - multiple entries mean multiple disks, one image plan
	// each.
	DataImages []ImageDeployPlan `json:"dataImages,omitempty"`
	// Ezio carries the Machine's own ezio tuning overrides
	// (spec.ezio), verbatim. A nil field within it (or Ezio itself being
	// nil) means "use the leecher's built-in default": this plan only
	// ever carries the Machine's overrides, never a fully resolved
	// value, since the operator's cluster-wide default lives in the
	// leecher's own configuration, not in anything the controller
	// resolves.
	Ezio *keziov1alpha1.MachineEzioTuning `json:"ezio,omitempty"`
	// AfterDeploy is what the agent does once every image plan above has
	// been applied: keziov1alpha1.AfterDeployReboot or
	// AfterDeployPowerOff.
	AfterDeploy string `json:"afterDeploy"`
}

// ImageDeployPlan is one Image's deploy plan: the disk it targets, the
// partition table to lay down there, and what goes in each partition.
type ImageDeployPlan struct {
	// ImageRef names the Image this plan deploys.
	ImageRef keziov1alpha1.NameRef `json:"imageRef"`
	// Disk is the resolved target device path, for example
	// "/dev/nvme0n1" - copied verbatim from
	// Machine.status.provisioning.{image,dataImages[]}.targetDisk.
	Disk string `json:"disk"`
	// SfdiskJSON is the verbatim sfdisk JSON dump captured at ingest
	// (Image.status.disk.layoutRef ConfigMap, key "sfdisk.json"; see
	// internal/controller's ensureLayoutConfigMap). The agent applies it
	// to Disk (`sfdisk <Disk>`) to recreate the partition table - GPT/MBR
	// disk and partition type GUIDs included - before touching any
	// individual partition.
	SfdiskJSON string `json:"sfdiskJSON"`
	// Partitions is the ordered list of partitions to restore onto Disk,
	// one per Image.status.partitions[] entry, in the same order.
	Partitions []PlanPartition `json:"partitions"`
}

// PlanPartition is one partition to restore, derived from one
// Image.status.partitions[] entry. Exactly one of the "what to put
// here" fields is populated, mirroring the classification
// internal/store.ImageLayoutSlot.IsBlank documents for the store's own
// per-slot records:
//
//   - SwapUUID set: run mkswap with this UUID (Role == "swap").
//   - InfoHash and Torrent set: restore this content over BitTorrent.
//   - Neither set: a blank partition - mkfs with FSType, or leave
//     unformatted when FSType is also empty (for example an msr
//     partition with a blank content, though ingest itself never
//     produces one).
type PlanPartition struct {
	// Number is the partition number sfdisk assigns (1-based), matching
	// Image.status.partitions[].number and SfdiskJSON.
	Number int32 `json:"number"`
	// Device is the resolved partition device path once SfdiskJSON has
	// been applied to Disk, for example "/dev/nvme0n1p1" or "/dev/sda1"
	// (see DevicePartitionPath). The controller computes it up front so
	// the agent does not need its own copy of the kernel's disk-naming
	// convention.
	Device string `json:"device"`
	// Role is the OS-neutral partition role (see
	// keziov1alpha1.PartitionRole* constants).
	Role string `json:"role"`
	// FSType is the file system to expect (a content partition) or
	// create (a blank partition). Empty for a role with no file system
	// (msr) or a swap partition (which uses SwapUUID instead).
	FSType string `json:"fsType,omitempty"`
	// InfoHash is the BitTorrent v1 info hash of this partition's
	// content, present for a content partition.
	InfoHash string `json:"infoHash,omitempty"`
	// Torrent is the bencoded .torrent file bytes for InfoHash, built on
	// demand from the store's torrent.info with the configured tracker
	// URL (see internal/store.BuildTorrentFile). Present iff InfoHash
	// is.
	Torrent []byte `json:"torrent,omitempty"`
	// SwapUUID is the UUID to pass to mkswap, present for a swap
	// partition (Role == "swap").
	SwapUUID string `json:"swapUUID,omitempty"`
}

// ProgressRequest is the POST /agent/machines/<name>/progress request
// body: a snapshot of every partition the agent is currently executing a
// DeployPlan against, sent periodically while the deploy runs (see
// internal/agent/deploy). The controller records it as a lightweight
// status condition (agentserver's ProvisioningProgress condition) rather
// than a structured, per-partition status field - a snapshot this size
// would otherwise inflate every Machine's status object and generation
// history for information that only matters while a deployment is live.
type ProgressRequest struct {
	// Partitions is one entry per partition the current DeployPlan names,
	// across the OS image and every dataImages entry.
	Partitions []PartitionProgress `json:"partitions"`
}

// ProgressResponse is the POST /agent/machines/<name>/progress success
// response. It carries nothing today; its presence keeps the wire
// contract symmetric with every other endpoint (a decodable JSON body on
// success) and gives future fields (for example a server-requested
// report interval) somewhere to land without a breaking change.
type ProgressResponse struct{}

// Partition phase values for PartitionProgress.Phase.
const (
	// PartitionPhasePartitioning means the disk's partition table is
	// being written or re-read; no per-partition percent applies yet.
	PartitionPhasePartitioning = "partitioning"
	// PartitionPhaseFormatting means mkfs or mkswap is running against a
	// blank or swap partition.
	PartitionPhaseFormatting = "formatting"
	// PartitionPhaseSeeding means a content partition's torrent is
	// downloading (or already finished and being held open for the stop
	// policy's idle window).
	PartitionPhaseSeeding = "seeding"
	// PartitionPhaseDone means this partition's content is fully in
	// place: mkfs/mkswap completed, or the torrent reached 100% and the
	// stop policy paused it.
	PartitionPhaseDone = "done"
)

// PartitionProgress is one partition's current status within a running
// deploy.
type PartitionProgress struct {
	// Disk is the partition's target disk device path, for example
	// "/dev/nvme0n1" - matches ImageDeployPlan.Disk.
	Disk string `json:"disk"`
	// Number is the partition number, matching PlanPartition.Number.
	Number int32 `json:"number"`
	// Phase is one of the PartitionPhase* constants.
	Phase string `json:"phase"`
	// PercentDone is 0-100. For PartitionPhaseSeeding it is the
	// torrent's download progress; for every other phase it is 0 until
	// Phase becomes PartitionPhaseDone, at which point it is 100.
	PercentDone int32 `json:"percentDone"`
}

// DevicePartitionPath returns the kernel device path for partition
// number on disk, using the standard Linux partition-naming convention:
// a disk name ending in a digit (for example "nvme0n1", "mmcblk0",
// "loop0") gets a "p" infix before the partition number, so the number
// does not visually merge into the disk name's own trailing digit; every
// other disk name (for example "sda", "vda") does not.
func DevicePartitionPath(disk string, number int32) string {
	if disk == "" {
		return ""
	}
	sep := ""
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		sep = "p"
	}
	return fmt.Sprintf("%s%s%d", disk, sep, number)
}
