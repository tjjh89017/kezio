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

// Package agentapi defines the wire protocol between kezio-agent (running
// in the live boot environment) and the controller's agent registration
// endpoint (internal/agentserver). It holds only request/response types
// and their JSON shape - no HTTP client, no HTTP server, no
// controller-runtime dependency - so the agent binary that imports it
// stays free of everything the manager process needs (client-go,
// controller-runtime, and their transitive weight) that a binary running
// in a minimal live environment has no use for. The one exception is
// api/v1alpha2 itself: RegisterRequest reuses MachineHardwareSpec
// directly rather than a parallel DTO, since the two are the same data
// by construction and a second type would only be able to drift out of
// sync with the first; that package imports nothing beyond
// k8s.io/apimachinery's metav1, so it carries none of client-go's or
// controller-runtime's weight.
package agentapi

import (
	"fmt"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// RegisterPath, NextPath, and ProgressPath are the HTTP routes the agent
// talks to, relative to the kezio.server= base URL the kernel cmdline
// carries (see internal/bootserver's renderNetBootConfig). All three
// live under the "/agent/" prefix internal/bootd's proxy forwards to the
// in-cluster agent server - see that package's init check, which panics
// at process start if any constant here ever stops satisfying that.
const (
	RegisterPath = "/agent/register"
	NextPath     = "/agent/next"
	ProgressPath = "/agent/progress"
)

// AgentSchemaVersion is the current wire schema version for every type
// in this package, including DeployPlan. AgentSchemaVersionHeader
// carries it on every request the agent sends; internal/agentserver
// rejects any request whose header is missing or does not equal this
// value, on every route, so an agent build speaking an incompatible
// shape is refused before its request body is ever decoded rather than
// misreading (or being misread by) a plan it does not actually
// understand.
const AgentSchemaVersion = 3

// AgentSchemaVersionHeader is the HTTP header kezio-agent sets on every
// request it sends, carrying the AgentSchemaVersion value it was built
// against, as a decimal integer.
const AgentSchemaVersionHeader = "X-Kezio-Agent-Schema-Version"

// RegisterRequest is the POST /agent/register request body: the boot
// token authenticates the caller as the "Authorization: Bearer <token>"
// header (not a body field - see internal/bootserver's token, which the
// kernel cmdline embeds the same way), and Hardware is the full hardware
// inventory the agent collected from the live OS.
type RegisterRequest struct {
	// Hardware is the reported inventory, stored verbatim as the
	// same-name MachineHardware's spec.
	Hardware keziov1alpha2.MachineHardwareSpec `json:"hardware"`
}

// RegisterResponse is the POST /agent/register success response.
type RegisterResponse struct {
	// MachineName is the name of the Machine the presented token
	// resolved to.
	MachineName string `json:"machineName"`
	// SessionToken is a fresh credential the agent presents (as a Bearer
	// token, the same as the boot token at /agent/register) on every
	// subsequent GET/POST /agent/next call. It is distinct from the
	// single-use boot token that authenticated this request - that token
	// is already consumed by the time this response is written - so
	// polling needs its own credential. Only its SHA-256 hash is ever
	// persisted server-side (Machine.status.agentSession.tokenHash); this
	// value is returned exactly once and never recoverable again.
	SessionToken string `json:"sessionToken"`
	// SessionTTLSeconds bounds how long SessionToken is accepted on
	// GET/POST /agent/next, so the agent knows to re-register (it cannot
	// - the boot token that got it here is already consumed) well before
	// a long-running live session's session token would otherwise expire
	// out from under it.
	SessionTTLSeconds int64 `json:"sessionTTLSeconds"`
}

// NextResponse is the GET/POST /agent/next response body.
type NextResponse struct {
	// Action is ActionWait or ActionDeploy.
	Action string `json:"action"`
	// PollIntervalSeconds tells the agent how long to wait before its
	// next /agent/next call.
	PollIntervalSeconds int32 `json:"pollIntervalSeconds"`
	// Plan is the deployment plan, present iff Action == ActionDeploy.
	Plan *DeployPlan `json:"plan,omitempty"`
}

// ActionWait is the NextResponse.Action value: there is nothing for the
// agent to do yet.
const ActionWait = "wait"

// ActionDeploy is the NextResponse.Action value when NextResponse.Plan is
// populated and ready to execute.
const ActionDeploy = "deploy"

// ErrorResponse is the JSON body returned alongside any non-2xx
// response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// DeploySlot is one partition to write as part of a DeployPlan: exactly
// one of Torrent, Mkfs, or Swap is set, mirroring the mutual exclusion
// keziov1alpha2.ImageSlot's own content/fsType fields already enforce on
// the CR side.
type DeploySlot struct {
	// Number is the partition number (1-based, as sfdisk and the kernel
	// report it), matching the plan's SfdiskScript/DataImages[].SfdiskScript.
	Number int32 `json:"number"`
	// Device is the resolved partition device path once SfdiskScript has
	// been applied to the target disk, for example "/dev/nvme0n1p1" - the
	// manager computes it so the agent needs no copy of the kernel's
	// disk-naming convention.
	Device string `json:"device"`
	// Torrent restores this slot's content over BitTorrent.
	Torrent *DeployTorrent `json:"torrent,omitempty"`
	// Mkfs creates a fresh file system on this slot.
	Mkfs *DeployMkfs `json:"mkfs,omitempty"`
	// Swap runs mkswap on this slot.
	Swap *DeploySwap `json:"swap,omitempty"`
}

// DeployTorrent is a DeploySlot's content source: the manager has already
// resolved URL to the seeder Pod address serving this machine's site, so
// the agent fetches it directly with no further cluster lookup.
type DeployTorrent struct {
	// URL is where the agent fetches this slot's .torrent file from.
	URL string `json:"url"`
	// InfoHash is the BitTorrent v1 info hash of this slot's content.
	InfoHash string `json:"infoHash"`
}

// DeployMkfs is a DeploySlot's blank-file-system instruction.
type DeployMkfs struct {
	// Filesystem is the file system to create, for example "ext4".
	Filesystem string `json:"filesystem"`
	// Label is the file system label to set, if any.
	Label string `json:"label,omitempty"`
	// UUID is the file system UUID to set, if any.
	UUID string `json:"uuid,omitempty"`
}

// DeploySwap is a DeploySlot's mkswap instruction.
type DeploySwap struct {
	// UUID is the UUID to pass to mkswap, if any.
	UUID string `json:"uuid,omitempty"`
}

// DeployDataImagePlan is one non-OS image's plan within a DeployPlan,
// mirroring keziov1alpha2.MachineDataImage's per-image grouping so the
// wire shape and the DeployRun snapshot it is built from do not drift.
type DeployDataImagePlan struct {
	// ImageRef names the Image this plan deploys.
	ImageRef keziov1alpha2.NameRef `json:"imageRef"`
	// TargetDisk is the resolved device path this image writes to.
	TargetDisk string `json:"targetDisk"`
	// SfdiskScript is this image's sfdisk JSON dump, in the same format
	// keziov1alpha2.ImageDiskLayout.SfdiskJSON carries.
	SfdiskScript string `json:"sfdiskScript"`
	// Slots is the ordered list of partitions to write on TargetDisk.
	Slots []DeploySlot `json:"slots"`
}

// Hook step type values for ResolvedHookStep.Type, mirroring
// keziov1alpha2's PostHookStepType* constants (the CRD-side step kind
// constants).
const (
	// HookStepTypeBuiltin means Builtin names the step to run.
	HookStepTypeBuiltin = "builtin"
	// HookStepTypeScript means Content runs in the live OS, nothing
	// mounted.
	HookStepTypeScript = "script"
)

// ResolvedHook is one PostHook attached to a deploy, fully resolved: its
// steps in order, each already carrying whatever content it needs to run
// with no further cluster access.
type ResolvedHook struct {
	// Name is the PostHook's name, carried through for progress messages
	// and diagnostics only - the agent never looks it up.
	Name string `json:"name"`
	// Steps run in list order, mirroring the PostHook's own spec.steps
	// order.
	Steps []ResolvedHookStep `json:"steps"`
}

// ResolvedHookStep is one PostHook step, resolved for the agent to run
// with no cluster access: a script step's Content is already fetched from
// its configMapRef/secretRef (or copied from an inline script) and
// templated - never a ConfigMap or Secret reference on the wire, since the
// manager resolves those.
type ResolvedHookStep struct {
	// Type is one of the HookStepType* constants.
	Type string `json:"type"`
	// Builtin is the builtin step name, set iff Type is
	// HookStepTypeBuiltin.
	Builtin string `json:"builtin,omitempty"`
	// Params carries the builtin step's parameters, set iff Type is
	// HookStepTypeBuiltin.
	Params map[string]string `json:"params,omitempty"`
	// Content is the already-templated script to run, set iff Type is
	// HookStepTypeScript.
	Content string `json:"content,omitempty"`
	// OSFamily restricts this step to a target OS family; empty means it
	// applies regardless.
	OSFamily string `json:"osFamily,omitempty"`
	// TimeoutSeconds bounds how long the step may run before the agent
	// treats it as failed and aborts the deploy. Always positive.
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// DeployPlan is everything kezio-agent needs to lay down its target
// disk's partition table and content, run its resolved hooks, and hand
// the machine off. The manager (internal/agentserver) builds it fresh on
// every GET /agent/next call once the DeployRun it is attempting is
// resolved; it is never persisted to a CR.
type DeployPlan struct {
	// SchemaVersion is AgentSchemaVersion at the time the manager built
	// this plan. It is always set - never left at its zero value - so an
	// agent decoding a plan built before this field existed reads 0,
	// which never equals a real AgentSchemaVersion.
	SchemaVersion int32 `json:"schemaVersion"`
	// RunName is the DeployRun this plan executes, so progress the agent
	// reports (ProgressRequest) can be correlated back to it.
	RunName string `json:"runName"`
	// RunUID is the DeployRun's UID at the time this plan was built: a
	// same-name DeployRun created after this one is deleted is a
	// different run, and RunUID is what lets a stale progress report be
	// told apart from a current one.
	RunUID string `json:"runUID"`
	// MachineName is the Machine this plan deploys to. The agent's
	// finalize step (internal/agent/deploy) uses it to build a stable
	// UEFI boot entry label, so a later redeploy to the same machine can
	// find and replace its own prior entry instead of accumulating
	// duplicates. Empty on a plan built before this field existed - the
	// agent falls back to RunName in that case, at the cost of that
	// dedup property.
	MachineName string `json:"machineName,omitempty"`
	// TargetDisk is the OS image's resolved target device path, for
	// example "/dev/nvme0n1".
	TargetDisk string `json:"targetDisk"`
	// SfdiskScript is the OS image's sfdisk JSON dump, in the same format
	// keziov1alpha2.ImageDiskLayout.SfdiskJSON carries. The agent applies
	// it to TargetDisk (`sfdisk <TargetDisk>`) to recreate the partition
	// table before touching any individual slot.
	SfdiskScript string `json:"sfdiskScript"`
	// Slots is the ordered list of partitions to write on TargetDisk.
	Slots []DeploySlot `json:"slots"`
	// DataImages is one plan per Machine.spec.dataImages entry, in the
	// same order.
	DataImages []DeployDataImagePlan `json:"dataImages,omitempty"`
	// Hooks is every PostHook attached to this deploy, fully resolved, in
	// the order keziov1alpha2.MachineSpec.PostHookRefs' doc comment fixes
	// (the OS Image's own postHookRefs first, then the Machine's own).
	Hooks []ResolvedHook `json:"hooks,omitempty"`
	// AfterDeploy is what the agent does once every slot above has been
	// written and every hook has run: keziov1alpha2.AfterDeployReboot or
	// AfterDeployPowerOff.
	AfterDeploy string `json:"afterDeploy"`
	// MaxUploads is the ezio AddTorrent max_uploads value every torrent
	// slot in this plan uses, already resolved by the manager (built-in
	// default, then the operator's cluster-wide leecher default, then
	// machine.spec.ezio's override - see internal/seeder.ResolveMaxUploads).
	// Zero on a plan built before this field existed; the agent falls back
	// to seeder.DefaultMaxUploads in that case.
	MaxUploads int32 `json:"maxUploads,omitempty"`
	// MaxConnections is MaxUploads's max_connections counterpart.
	MaxConnections int32 `json:"maxConnections,omitempty"`
}

// Validate checks p's structural invariants: it never reaches into the
// cluster or the local machine, so it catches a malformed plan (a
// manager bug, wire corruption) before the agent issues a single
// destructive command against a real disk.
func (p DeployPlan) Validate() error {
	if p.SchemaVersion != AgentSchemaVersion {
		return fmt.Errorf("schemaVersion %d does not match the agent wire schema version %d", p.SchemaVersion, AgentSchemaVersion)
	}
	// TargetDisk/Slots are empty together exactly when the plan deploys no
	// OS image (a dataImages-only run, see keziov1alpha2.MachineSpec.
	// ImageRef's doc comment) - that is a legitimate plan, not a
	// malformed one, so only a plan with one set but not the other, or
	// with Slots itself malformed, is rejected here.
	if p.TargetDisk == "" {
		if len(p.Slots) != 0 {
			return fmt.Errorf("slots set with no targetDisk")
		}
	} else if err := validateSlots(p.Slots); err != nil {
		return fmt.Errorf("slots: %w", err)
	}
	for i, di := range p.DataImages {
		if di.TargetDisk == "" {
			return fmt.Errorf("dataImages[%d]: targetDisk is empty", i)
		}
		if err := validateSlots(di.Slots); err != nil {
			return fmt.Errorf("dataImages[%d]: slots: %w", i, err)
		}
	}
	for i, h := range p.Hooks {
		for j, s := range h.Steps {
			if err := validateHookStep(s); err != nil {
				return fmt.Errorf("hooks[%d] %q step[%d]: %w", i, h.Name, j, err)
			}
		}
	}
	return nil
}

// validateSlots checks the invariants every Slots list in a DeployPlan
// must hold: non-empty, unique partition numbers, and exactly one
// content kind per slot.
func validateSlots(slots []DeploySlot) error {
	if len(slots) == 0 {
		return fmt.Errorf("no slots")
	}
	seen := make(map[int32]bool, len(slots))
	for _, s := range slots {
		if seen[s.Number] {
			return fmt.Errorf("partition %d appears more than once", s.Number)
		}
		seen[s.Number] = true

		kinds := 0
		if s.Torrent != nil {
			kinds++
		}
		if s.Mkfs != nil {
			kinds++
		}
		if s.Swap != nil {
			kinds++
		}
		if kinds != 1 {
			return fmt.Errorf("partition %d must set exactly one of torrent, mkfs, or swap; got %d", s.Number, kinds)
		}
	}
	return nil
}

// validateHookStep checks one ResolvedHookStep is well-formed: Type
// selects which of Builtin/Content is required, and every step needs a
// positive timeout.
func validateHookStep(s ResolvedHookStep) error {
	switch s.Type {
	case HookStepTypeBuiltin:
		if s.Builtin == "" {
			return fmt.Errorf("builtin step has no builtin name")
		}
	case HookStepTypeScript:
		if s.Content == "" {
			return fmt.Errorf("%s step has no content", s.Type)
		}
	default:
		return fmt.Errorf("unknown step type %q", s.Type)
	}
	if s.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeoutSeconds must be positive")
	}
	return nil
}

// ProgressRequest is the POST /agent/progress request body: a periodic
// report of one deploy step's status, sent while a DeployPlan runs.
type ProgressRequest struct {
	// RunName is the DeployRun this report belongs to, matching
	// DeployPlan.RunName.
	RunName string `json:"runName"`
	// RunUID is the DeployRun's UID, matching DeployPlan.RunUID.
	RunUID string `json:"runUID"`
	// Step names the deploy sub-step this report reflects, for example
	// keziov1alpha2.DeployRunPhaseWritingContent.
	Step string `json:"step"`
	// State is one of the ProgressState* constants.
	State string `json:"state"`
	// Message is a short human-readable detail, present for a state that
	// benefits from one (in particular ProgressStateFailed).
	Message string `json:"message,omitempty"`
	// PercentDone is 0-100, present when Step reports fine-grained
	// progress (for example a partition's torrent download).
	PercentDone *int32 `json:"percentDone,omitempty"`
	// BytesDone is the number of bytes written so far, present under the
	// same condition as PercentDone.
	BytesDone *int64 `json:"bytesDone,omitempty"`
	// Timestamp is when the agent observed this report's state.
	Timestamp time.Time `json:"timestamp"`
}

// ProgressState values for ProgressRequest.State.
const (
	// ProgressStateRunning means Step is currently in progress.
	ProgressStateRunning = "running"
	// ProgressStateSucceeded means Step finished successfully.
	ProgressStateSucceeded = "succeeded"
	// ProgressStateFailed means Step failed; Message carries the detail.
	ProgressStateFailed = "failed"
)

// ProgressResponse is the POST /agent/progress response body.
type ProgressResponse struct {
	// Action is one of the ProgressAction* constants.
	Action string `json:"action"`
}

// ProgressAction values for ProgressResponse.Action.
const (
	// ProgressActionContinue means the agent keeps running its current
	// DeployPlan.
	ProgressActionContinue = "continue"
	// ProgressActionAbort means the agent cancels the run: it stops every
	// local torrent it started for this plan and reports one final
	// ProgressStateFailed step before giving up on the plan entirely.
	ProgressActionAbort = "abort"
)
