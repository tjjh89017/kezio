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
//
// Versioning: DeployPlan.SchemaVersion carries PlanSchemaVersion, bumped
// on any breaking change to DeployPlan's shape (the kind that would make
// an older agent decode a newer plan without error yet misread it - e.g.
// finding no torrent bytes and mkfs'ing a content partition instead of
// restoring it). The agent advertises its supported version on every GET
// .../next poll (AgentSchemaVersionHeader); internal/agentserver withholds
// a plan (ActionWait plus a MachineConditionAgentCompatible condition) for
// any absent or mismatched version, and internal/agent's own poll loop
// also refuses to execute a plan whose SchemaVersion it does not support -
// so a version mismatch never lands as a data-destroying misread on
// either end.
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

// PlanSchemaVersion is the current DeployPlan wire shape's version - see
// the package doc comment's "Versioning" section. Bump it, and update that
// paragraph, on the next breaking change to any type in this file.
const PlanSchemaVersion = 2

// AgentSchemaVersionHeader is the HTTP header kezio-agent sets on every
// GET .../next poll (internal/agent.Client.Next) carrying the
// PlanSchemaVersion value it supports, as a decimal integer.
// internal/agentserver's handleNext reads it before ever building a
// plan: a missing header (every agent built before this header existed)
// or a value that does not equal the server's own PlanSchemaVersion
// means the plan is withheld - see MachineConditionAgentCompatible's use
// in internal/agentserver.
const AgentSchemaVersionHeader = "X-Kezio-Agent-Schema-Version"

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
// is populated and ready to execute.
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
	// SchemaVersion is PlanSchemaVersion at the time internal/agentserver
	// built this plan. It is always set - never left at its zero value -
	// so an agent decoding a plan built before this field existed reads
	// 0, which never equals a real PlanSchemaVersion and so is refused
	// the same as any other unsupported version (see the package doc
	// comment's "Versioning" section).
	SchemaVersion int `json:"schemaVersion"`
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
	// MachineName is the Machine this plan deploys to. The agent's
	// finalize step (internal/agent/deploy) uses it to build a stable
	// UEFI boot entry label, so a later redeploy to the same machine can
	// find and replace its own prior entry instead of accumulating
	// duplicates.
	MachineName string `json:"machineName"`
	// Hooks is every PostHook attached to this deploy, fully resolved:
	// the OS Image's own postHookRefs first, then the Machine's own
	// postHookRefs, each in the order its spec lists them (see
	// keziov1alpha1.ImageSpec.PostHookRefs and
	// keziov1alpha1.MachineSpec.PostHookRefs' doc comments for why that
	// order). internal/agentserver builds every entry here - fetching
	// each PostHook CR plus any configMapRef/secretRef script content and
	// templating it with the merged params - so the agent
	// (internal/agent/deploy) can run every step without ever reading a
	// PostHook, ConfigMap, or Secret from the cluster itself.
	Hooks []ResolvedHook `json:"hooks,omitempty"`
}

// Hook step type values for ResolvedHookStep.Type, mirroring
// keziov1alpha1.PostHookStepType* (the CRD-side step kind constants).
const (
	// HookStepTypeBuiltin means Builtin names the step to run.
	HookStepTypeBuiltin = "builtin"
	// HookStepTypeScript means Content runs in the live OS, nothing
	// mounted.
	HookStepTypeScript = "script"
	// HookStepTypeChrootScript means Content runs inside a chroot of the
	// deployed OS image's root partition.
	HookStepTypeChrootScript = "chrootScript"
)

// ResolvedHook is one PostHook attached to a deploy, fully resolved: its
// steps in order, each already carrying whatever content it needs to run
// with no further cluster access.
type ResolvedHook struct {
	// Name is the PostHook's name, carried through for progress messages
	// and diagnostics only - the agent never looks it up.
	Name string `json:"name"`
	// Steps run in list order, mirroring the PostHook's own
	// spec.steps order.
	Steps []ResolvedHookStep `json:"steps"`
}

// ResolvedHookStep is one PostHook step, resolved for the agent to run
// with no cluster access: a script step's content is already fetched
// from its configMapRef/secretRef (or copied from an inline script) and
// templated with the deploy's merged params.
type ResolvedHookStep struct {
	// Type is one of the HookStepType* constants.
	Type string `json:"type"`
	// Builtin is the builtin step name (see keziov1alpha1.BuiltinStep*
	// constants, for example "mkswap"), set iff Type is
	// HookStepTypeBuiltin.
	Builtin string `json:"builtin,omitempty"`
	// Content is the already-templated script to run, set iff Type is
	// HookStepTypeScript or HookStepTypeChrootScript.
	Content string `json:"content,omitempty"`
	// FromSecret reports whether Content was sourced from a Secret
	// (keziov1alpha1.PostHookScriptSourceSecretRef). The agent must never
	// write Content to a log line - regardless of FromSecret - but this
	// flag lets it also skip echoing the step in any other
	// human-readable output that isn't already scrubbed the same way.
	FromSecret bool `json:"fromSecret,omitempty"`
	// TimeoutSeconds bounds how long the step may run before the agent
	// treats it as failed and aborts the deploy. Always positive: the
	// resolver copies it from the step's own
	// EffectiveTimeoutSeconds(), which already substitutes
	// keziov1alpha1.PostHookDefaultTimeoutSeconds for an unset value.
	TimeoutSeconds int32 `json:"timeoutSeconds"`
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
	// GrowLastPartition, when true, asks the agent's finalize step to
	// grow this image's last partition (and its file system, when
	// FSType names one the agent knows how to resize) to use whatever
	// extra space Disk has beyond the layout SfdiskJSON itself asks for.
	// Default false; nothing in the controller sets this true today, so
	// every plan built by internal/agentserver leaves partition sizes
	// untouched. The field exists so the agent side already has real,
	// tested behavior once a controller-side field starts setting it.
	GrowLastPartition bool `json:"growLastPartition,omitempty"`
}

// PlanPartition is one partition to restore, derived from one
// Image.status.partitions[] entry. Exactly one of the "what to put
// here" fields is populated, mirroring the classification
// internal/store.ImageLayoutSlot.IsBlank documents for the store's own
// per-slot records:
//
//   - SwapUUID set: run mkswap with this UUID (Role == "swap").
//   - InfoHash and TorrentURL set: restore this content over
//     BitTorrent.
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
	// TorrentURL is where the agent fetches this partition's .torrent
	// file from: the per-Image seeder pod serving this Machine's site,
	// resolved to its own PodIP (see internal/agentserver's
	// buildPlanPartition) - never bytes inlined in the plan, since a
	// .torrent scales with piece count unlike the rest of this small,
	// cacheable JSON plan. Present iff InfoHash is; InfoHash set with this
	// empty is a plan-building error the controller must refuse to hand
	// out, never a value the agent should treat as blank.
	TorrentURL string `json:"torrentURL,omitempty"`
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
	// Step, when non-empty, is the whole-plan step this report reflects -
	// one of the DeployStep* constants below. It augments the
	// per-partition Partitions detail with a single overall status the
	// controller reflects verbatim as the ProvisioningProgress
	// condition's Reason (internal/agentserver's
	// setProvisioningProgressCondition), including the terminal
	// DeployStepRebootingToDisk value that reports the whole deploy
	// succeeded - see internal/agent/deploy.Executor.Execute, which sends
	// it as the last report before invoking systemctl reboot/poweroff.
	// Empty means "no overall step to report"; the controller falls back
	// to summarizing Partitions the way it always has.
	Step string `json:"step,omitempty"`
	// StepMessage augments Step with a short human-readable detail, when
	// one is more useful than the per-partition summary Step's message
	// otherwise falls back to (see summarizeProgress).
	StepMessage string `json:"stepMessage,omitempty"`
}

// DeployStep values for ProgressRequest.Step: the whole-plan sub-steps a
// running deploy passes through, reported alongside (never instead of)
// per-partition Partitions detail. Every one of these is reported before
// the terminal DeployStepRebootingToDisk value; only that terminal value
// means the deploy actually succeeded end to end.
const (
	// DeployStepPartitioning means every image plan's partition table is
	// being written and re-read (sfdisk, blockdev --rereadpt) - before any
	// individual partition's content or file system is touched.
	DeployStepPartitioning = "WritingPartitionTable"
	// DeployStepWritingContent means every partition is being made or
	// restored: mkfs/mkswap for blank and swap partitions, AddTorrent and
	// the seeding loop for content partitions. Per-partition PercentDone
	// in Partitions carries the finer-grained detail this step name does
	// not.
	DeployStepWritingContent = "WritingContent"
	// DeployStepRunningPostHook means every partition's content and file
	// system is in place and the agent is running the plan's Hooks, in
	// order - see internal/agent/deploy's package doc comment for where
	// this slots between content writing and finalize.
	DeployStepRunningPostHook = "RunningPostHook"
	// DeployStepFinalizing means every partition is done and the agent is
	// running finalize actions (efibootmgr, and growLastPartition when a
	// plan asks for it) - see internal/agent/deploy's package doc comment
	// for why finalize never chroots or runs update-initramfs.
	DeployStepFinalizing = "Finalizing"
	// DeployStepRebootingToDisk is the terminal report: finalize
	// completed, and the agent is about to invoke systemctl reboot or
	// poweroff (DeployPlan.AfterDeploy). Reaching this step is the
	// deploy's success signal - the controller's real, agent-driven
	// Deployer polls MachineConditionProvisioningProgress's Reason for
	// exactly this value to know the deployment finished.
	DeployStepRebootingToDisk = "RebootingToDisk"
	// DeployStepFailed is the terminal failure report: Execute is
	// returning a non-nil error before ever reaching
	// DeployStepRebootingToDisk (validation, a Runner command, a hook, or
	// finalize failed - see internal/agent/deploy.Executor.Execute's
	// defer). StepMessage carries the failing error's detail. The
	// controller's real, agent-driven Deployer treats this Reason as the
	// deployment having failed outright (Result.ErrorMessage), rather
	// than continuing to poll for RebootingToDisk.
	DeployStepFailed = "Failed"
)

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
