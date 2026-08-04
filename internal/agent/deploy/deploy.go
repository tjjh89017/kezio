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

// Package deploy executes a DeployPlan (internal/agentapi) on the
// machine kezio-agent is running on: it writes each target disk's
// partition table, makes swap and blank file systems, drives a locally
// spawned ezio daemon in raw-device mode (AddTorrent's save_path is a
// partition device, never a -F content directory) to restore every
// content partition over BitTorrent, then finalizes the deploy
// (efibootmgr, and growing the last partition when a plan asks for it)
// and hands the machine off to the deployed OS (systemctl reboot or
// poweroff, per DeployPlan.AfterDeploy).
//
// Finalize deliberately never opens or modifies the file systems inside
// the deployed disks: no chroot, no update-initramfs. The content ezio
// restored is byte-identical to the source image, so every UUID it
// carries (root file system, swap - see mkswap --uuid) already matches
// whatever the image's own fstab and bootloader configuration expects;
// there is nothing to regenerate. efibootmgr only ever touches firmware
// NVRAM, never the ESP's own file system content.
//
// Between content writing and finalize, Execute runs plan.Hooks
// (runHooks, in hooks.go): every PostHook internal/agentserver resolved
// into the plan, already carrying each step's templated script content.
// This is the one place finalize's own "never chroots" rule does not
// apply - a chrootScript step deliberately mounts and chroots into the
// deployed root file system - but only on the cluster operator's own
// explicit request through a PostHook's steps, never automatically.
//

// This is the highest-blast-radius code in kezio: a wrong disk here
// destroys data with no undo. Every destructive command it issues names
// a device the DeployPlan itself supplied (never a glob, never a
// resolved-then-forgotten value), and Execute validates the entire plan
// - every disk exists and is large enough, every partition's device path
// is exactly what the plan's own naming convention predicts - before any
// command runs, so a single inconsistency anywhere in the plan aborts
// the whole deployment having written nothing at all rather than leaving
// some disks touched and others not.
//
// Every external command (sfdisk, partprobe, mkfs.*, mkswap, blockdev)
// and the ezio gRPC control plane are reached through the small Runner
// and EzioClient/EzioLauncher interfaces this package defines, not
// os/exec or seeder.Dial directly, so Execute's orchestration - which
// partitions get which command, in what order, when the stop policy
// pauses a torrent, when the local daemon shuts down - is unit-testable
// with fakes and needs no real devices or root. The exec-backed
// production implementations live in cmd/agent.
package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// DefaultPollInterval is how often Execute polls GetTorrentStatus while
// content partitions are seeding, when Executor.PollInterval is zero.
// This matches ezio's own reference client's update cadence
// (tmp/ezio/utils/ezio_cli.py's UPDATE_INTERVAL).
const DefaultPollInterval = 2 * time.Second

// EzioClient is the subset of *seeder.Client the Executor drives the
// local ezio daemon through.
type EzioClient interface {
	AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool, maxUploads, maxConnections int32) error
	GetTorrentStatus(ctx context.Context, hashes []string) (map[string]seeder.Torrent, error)
	PauseTorrent(ctx context.Context, hash string) error
	Shutdown(ctx context.Context) error
}

var _ EzioClient = (*seeder.Client)(nil)

// EzioHandle is a running local ezio daemon: a ready-to-use client
// against it, and a way to force it down as a backstop when the graceful
// Shutdown RPC either was never reached (an earlier step failed) or did
// not actually terminate the process.
type EzioHandle struct {
	Client EzioClient
	// Stop force-terminates the daemon process. Safe to call after a
	// graceful Shutdown already succeeded (killing an already-exited
	// process is a no-op error every caller here ignores).
	Stop func() error
}

// EzioLauncher starts a local ezio daemon in raw-device mode (no -F:
// AddTorrent's save_path targets a partition device directly - see the
// package doc comment) with the given tuning applied to its daemon CLI
// flags, and returns a handle to it. tuning may be nil, meaning every
// value takes the launcher's own built-in default.
type EzioLauncher interface {
	Launch(ctx context.Context, tuning *keziov1alpha1.MachineEzioTuning) (EzioHandle, error)
}

// ProgressReporter streams a deploy's progress - per-partition detail
// plus the whole-plan step (agentapi.ProgressRequest.Step) - to the
// controller. A failure to report is logged and otherwise ignored - a
// missed progress update does not itself justify failing an in-progress
// deployment.
type ProgressReporter interface {
	ReportProgress(ctx context.Context, req agentapi.ProgressRequest) error
}

// Executor runs a DeployPlan against the machine it executes on.
type Executor struct {
	// Runner executes sfdisk, partprobe, mkfs.*, mkswap, and blockdev.
	Runner Runner
	// Ezio launches the local ezio daemon used for every content
	// partition across the whole plan. Only consulted when the plan
	// actually has a content partition; a plan with only blank and swap
	// partitions never launches ezio at all.
	Ezio EzioLauncher
	// Progress reports partition progress as the deploy proceeds. Nil
	// means progress is not reported (tests only; production always
	// wires one).
	Progress ProgressReporter
	// Logf receives progress and diagnostic messages in fmt.Printf
	// style. Nil discards them.
	Logf func(format string, args ...any)
	// PollInterval is how often the seeding loop polls
	// GetTorrentStatus. Zero means DefaultPollInterval.
	PollInterval time.Duration
}

func (e *Executor) log(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Executor) pollInterval() time.Duration {
	if e.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return e.PollInterval
}

// partitionKind classifies a PlanPartition exactly as
// internal/store.ImageLayoutSlot.IsBlank and agentapi.PlanPartition's own
// doc comment do.
type partitionKind int

const (
	kindBlank partitionKind = iota
	kindSwap
	kindContent
)

func classify(p agentapi.PlanPartition) partitionKind {
	switch {
	case p.Role == keziov1alpha1.PartitionRoleSwap && p.SwapUUID != "":
		return kindSwap
	case p.InfoHash != "" && len(p.Torrent) > 0:
		return kindContent
	default:
		return kindBlank
	}
}

// imagePlans returns plan's image plans in deploy order: the OS image
// first (if any), then each dataImages entry in spec order - the same
// order buildDeployPlan (internal/agentserver) builds them in.
func imagePlans(plan *agentapi.DeployPlan) []agentapi.ImageDeployPlan {
	var plans []agentapi.ImageDeployPlan
	if plan.OS != nil {
		plans = append(plans, *plan.OS)
	}
	plans = append(plans, plan.DataImages...)
	return plans
}

// Execute runs every image plan in plan: partition tables, blank/swap
// partitions, and content partitions over BitTorrent, applying the
// deploy-side stop policy and shutting the local ezio daemon down once
// every content partition is done. It reports progress through
// e.Progress as it goes and returns once the whole plan has been
// applied, or the first error - which may leave some disks fully written
// and others untouched, but never a disk partially written by a command
// that was itself refused (see validate). Whenever it returns a non-nil
// error, the deferred reportFailure call below sends one last progress
// report carrying agentapi.DeployStepFailed with the error as
// StepMessage, before Execute itself returns: without it, the
// controller's real Deployer (which polls
// MachineConditionProvisioningProgress for either the terminal success
// step or a failure) would poll forever after a failed deploy that
// silently stopped reporting, instead of recognizing it failed.
func (e *Executor) Execute(ctx context.Context, plan *agentapi.DeployPlan) (err error) {
	plans := imagePlans(plan)
	if len(plans) == 0 {
		return nil
	}

	progress := newProgressTracker(plans)
	defer func() {
		if err != nil {
			e.reportFailure(ctx, progress, err)
		}
	}()

	if verr := e.validate(ctx, plans); verr != nil {
		return fmt.Errorf("refusing to deploy: %w", verr)
	}

	progress.setStep(agentapi.DeployStepPartitioning)
	e.report(ctx, progress)

	var handle EzioHandle
	var ezioLaunched bool
	if progress.hasContent() {
		var err error
		handle, err = e.Ezio.Launch(ctx, plan.Ezio)
		if err != nil {
			return fmt.Errorf("launching local ezio daemon: %w", err)
		}
		ezioLaunched = true
		defer func() {
			if handle.Stop != nil {
				_ = handle.Stop()
			}
		}()
	}

	progress.setStep(agentapi.DeployStepWritingContent)
	for _, ip := range plans {
		if err := e.applyImagePlan(ctx, ip, handle, progress, plan.Ezio); err != nil {
			return err
		}
	}

	if ezioLaunched {
		if err := e.seedUntilStopped(ctx, handle.Client, progress); err != nil {
			return err
		}
		if err := handle.Client.Shutdown(ctx); err != nil {
			// The stop policy already confirmed every torrent is paused
			// and finished; a failed graceful Shutdown RPC is logged and
			// covered by handle.Stop's deferred force-stop rather than
			// failing an otherwise-complete deployment.
			e.log("shutting down local ezio daemon: %v (forcing it down instead)", err)
		}
	}

	// Hooks run once every partition across the whole plan is fully
	// written (content, mkfs/mkswap, and - for a content partition - the
	// torrent finished and paused above) and before finalize, so a
	// chrootScript step sees a complete, mountable root file system and
	// a script step (or a "growLastPartition"/"efibootmgr" builtin) can
	// still run before finalize's own unconditional actions.
	progress.setStep(agentapi.DeployStepRunningPostHook)
	e.report(ctx, progress)
	if err := e.runHooks(ctx, plan); err != nil {
		return fmt.Errorf("running post hooks: %w", err)
	}

	progress.setStep(agentapi.DeployStepFinalizing)
	e.report(ctx, progress)
	if err := e.finalize(ctx, plan, plans); err != nil {
		return fmt.Errorf("finalizing: %w", err)
	}

	// The terminal report: everything above completed, so this is the
	// deploy's success signal (see agentapi.DeployStepRebootingToDisk's
	// doc comment). It is sent before invoking systemctl below, since
	// nothing sent after that call is guaranteed to land.
	progress.setStep(agentapi.DeployStepRebootingToDisk)
	e.report(ctx, progress)

	if err := e.rebootOrPowerOff(ctx, plan.AfterDeploy); err != nil {
		return fmt.Errorf("after deploy action: %w", err)
	}
	return nil
}

// validate checks every image plan's disk and partition set before
// Execute issues a single destructive command against any of them: each
// disk is a plausible device path, no two image plans target the same
// disk, each disk reports (through Runner) a size at least as large as
// its partition table requires, and each partition's device path is
// exactly what agentapi.DevicePartitionPath predicts from its own disk
// and number - a mismatch here means the plan is internally
// inconsistent (a controller bug or transport corruption), and the
// agent trusts the plan's Disk/Device fields as the *only* allowed
// write targets specifically because this check holds.
func (e *Executor) validate(ctx context.Context, plans []agentapi.ImageDeployPlan) error {
	// Pure structural checks first, across every image plan, before a
	// single Runner call (even a read-only one) happens: a plan that
	// fails here is internally inconsistent, and reporting that costs
	// nothing that touches the machine at all.
	seenDisks := make(map[string]bool, len(plans))
	for _, ip := range plans {
		if !strings.HasPrefix(ip.Disk, "/dev/") {
			return fmt.Errorf("image %s: disk %q is not an absolute /dev/ path", ip.ImageRef.Name, ip.Disk)
		}
		if seenDisks[ip.Disk] {
			return fmt.Errorf("disk %s is targeted by more than one image in this plan", ip.Disk)
		}
		seenDisks[ip.Disk] = true

		for _, p := range ip.Partitions {
			want := agentapi.DevicePartitionPath(ip.Disk, p.Number)
			if p.Device != want {
				return fmt.Errorf("image %s partition %d: device %q does not match the expected path %q for disk %q",
					ip.ImageRef.Name, p.Number, p.Device, want, ip.Disk)
			}
		}
	}

	// Only once every image plan has passed the structural checks above
	// does validate touch the machine at all, and even here only through
	// a read-only size query (blockdev --getsize64) - still before any
	// destructive command runs against any disk in the plan.
	for _, ip := range plans {
		if err := e.checkDiskSize(ctx, ip); err != nil {
			return err
		}
	}
	return nil
}

// checkDiskSize sanity-checks that ip.Disk exists and reports a size at
// least as large as ip.SfdiskJSON's layout requires, via `blockdev
// --getsize64` - a command that itself fails if the device node is
// absent, so this single check covers both "the device exists" and "the
// device is large enough".
func (e *Executor) checkDiskSize(ctx context.Context, ip agentapi.ImageDeployPlan) error {
	minBytes, err := MinimumDiskBytes(ip.SfdiskJSON)
	if err != nil {
		return fmt.Errorf("image %s: %w", ip.ImageRef.Name, err)
	}

	out, err := e.Runner.Run(ctx, nil, "blockdev", "--getsize64", ip.Disk)
	if err != nil {
		return fmt.Errorf("image %s: checking disk %s exists and is readable: %w", ip.ImageRef.Name, ip.Disk, err)
	}
	actual, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return fmt.Errorf("image %s: parsing size of disk %s: %w", ip.ImageRef.Name, ip.Disk, err)
	}
	if actual < minBytes {
		return fmt.Errorf("image %s: disk %s is %d bytes, smaller than the %d bytes its layout requires",
			ip.ImageRef.Name, ip.Disk, actual, minBytes)
	}
	return nil
}

// applyImagePlan writes ip's partition table, re-reads it, and then
// makes every blank/swap partition and adds every content partition's
// torrent to handle.Client (a zero EzioHandle when ip has no content
// partitions - applyImagePlan never dereferences handle.Client in that
// case).
func (e *Executor) applyImagePlan(ctx context.Context, ip agentapi.ImageDeployPlan, handle EzioHandle, progress *progressTracker, tuning *keziov1alpha1.MachineEzioTuning) error {
	script, err := SfdiskScript(ip.SfdiskJSON, ip.Disk)
	if err != nil {
		return fmt.Errorf("image %s: %w", ip.ImageRef.Name, err)
	}

	for _, p := range ip.Partitions {
		progress.setPhase(ip.Disk, p.Number, agentapi.PartitionPhasePartitioning, 0)
	}
	e.report(ctx, progress)

	e.log("writing partition table to %s (image %s)", ip.Disk, ip.ImageRef.Name)
	if _, err := e.Runner.Run(ctx, []byte(script), "sfdisk", ip.Disk); err != nil {
		return fmt.Errorf("image %s: writing partition table to %s: %w", ip.ImageRef.Name, ip.Disk, err)
	}
	if _, err := e.Runner.Run(ctx, nil, "partprobe", ip.Disk); err != nil {
		return fmt.Errorf("image %s: re-reading partition table on %s: %w", ip.ImageRef.Name, ip.Disk, err)
	}

	for _, p := range ip.Partitions {
		if err := e.applyPartition(ctx, ip, p, handle, progress, tuning); err != nil {
			return err
		}
	}
	return nil
}

// applyPartition applies one partition's content: mkswap for a swap
// partition, mkfs for a blank partition with a file system to create (a
// blank partition with no FSType, for example an msr partition, is left
// unformatted), or AddTorrent for a content partition. tuning is the
// plan's already-resolved EZIO tuning (cluster default merged with any
// Machine.spec.ezio override, see internal/agentserver.buildDeployPlan);
// it may be nil, in which case AddTorrent falls back to
// seeder.DefaultMaxUploads/DefaultMaxConnections.
func (e *Executor) applyPartition(ctx context.Context, ip agentapi.ImageDeployPlan, p agentapi.PlanPartition, handle EzioHandle, progress *progressTracker, tuning *keziov1alpha1.MachineEzioTuning) error {
	switch classify(p) {
	case kindSwap:
		if err := e.runMkswap(ctx, ip, p); err != nil {
			return err
		}
		progress.setPhase(ip.Disk, p.Number, agentapi.PartitionPhaseDone, 100)

	case kindContent:
		e.log("adding torrent for %s (info hash %s)", p.Device, p.InfoHash)
		maxUploads := seeder.ResolveMaxUploads(tuning)
		maxConnections := seeder.ResolveMaxConnections(tuning)
		if err := handle.Client.AddTorrent(ctx, p.Torrent, p.Device, false, maxUploads, maxConnections); err != nil {
			return fmt.Errorf("image %s partition %d: AddTorrent %s: %w", ip.ImageRef.Name, p.Number, p.Device, err)
		}
		progress.setPhase(ip.Disk, p.Number, agentapi.PartitionPhaseSeeding, 0)

	default: // kindBlank
		if p.FSType == "" {
			progress.setPhase(ip.Disk, p.Number, agentapi.PartitionPhaseDone, 100)
			break
		}
		e.log("mkfs.%s %s", p.FSType, p.Device)
		if _, err := e.Runner.Run(ctx, nil, "mkfs."+p.FSType, p.Device); err != nil {
			return fmt.Errorf("image %s partition %d: mkfs.%s %s: %w", ip.ImageRef.Name, p.Number, p.FSType, p.Device, err)
		}
		progress.setPhase(ip.Disk, p.Number, agentapi.PartitionPhaseDone, 100)
	}

	e.report(ctx, progress)
	return nil
}

// runMkswap runs mkswap against p (a swap partition, p.SwapUUID set),
// restoring its source UUID exactly. Extracted from applyPartition's own
// kindSwap case so the "mkswap" builtin post hook step (runHooks.go) can
// run the same command against every swap partition in plan, on
// explicit request, without duplicating it.
func (e *Executor) runMkswap(ctx context.Context, ip agentapi.ImageDeployPlan, p agentapi.PlanPartition) error {
	e.log("mkswap %s (uuid %s)", p.Device, p.SwapUUID)
	if _, err := e.Runner.Run(ctx, nil, "mkswap", "--uuid", p.SwapUUID, p.Device); err != nil {
		return fmt.Errorf("image %s partition %d: mkswap %s: %w", ip.ImageRef.Name, p.Number, p.Device, err)
	}
	return nil
}

// seedUntilStopped polls client for status until every content partition
// tracked in progress is finished and paused (allStopped), pausing each
// torrent as soon as shouldPauseTorrent says so and reporting the latest
// percent on every tick. It stops early - returning an error - only if
// ctx is cancelled; a single failed poll or pause RPC is logged and
// retried on the next tick, the same as internal/agent's own poll loop
// treats a transient controller-communication failure.
func (e *Executor) seedUntilStopped(ctx context.Context, client EzioClient, progress *progressTracker) error {
	hashes := progress.torrentHashes()
	if len(hashes) == 0 {
		return nil
	}

	ticker := time.NewTicker(e.pollInterval())
	defer ticker.Stop()

	for {
		statuses, err := client.GetTorrentStatus(ctx, hashes)
		if err != nil {
			e.log("polling torrent status failed: %v", err)
		} else {
			for hash, t := range statuses {
				progress.setTorrentStatus(hash, t)
				if shouldPauseTorrent(t) {
					if err := client.PauseTorrent(ctx, hash); err != nil {
						e.log("pausing torrent %s: %v", hash, err)
					}
				}
			}
			e.report(ctx, progress)
			if allStopped(statuses) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// report posts progress's current snapshot through e.Progress, logging
// (not failing) a delivery error - see ProgressReporter's doc comment.
func (e *Executor) report(ctx context.Context, progress *progressTracker) {
	if e.Progress == nil {
		return
	}
	if err := e.Progress.ReportProgress(ctx, progress.snapshot()); err != nil {
		e.log("reporting progress failed: %v", err)
	}
}

// reportFailure posts progress's current snapshot with Step forced to
// agentapi.DeployStepFailed and StepMessage set to cause's error text -
// Execute's terminal report on any failing return (see Execute's doc
// comment). It uses ctx as given even when the failure was itself a
// context cancellation: a best-effort delivery attempt costs nothing
// more than the report call already made throughout Execute, and
// ReportProgress's own error is logged, not propagated, the same as
// report above.
func (e *Executor) reportFailure(ctx context.Context, progress *progressTracker, cause error) {
	if e.Progress == nil {
		return
	}
	progress.setStep(agentapi.DeployStepFailed)
	req := progress.snapshot()
	req.StepMessage = cause.Error()
	if err := e.Progress.ReportProgress(ctx, req); err != nil {
		e.log("reporting failure step failed: %v", err)
	}
}
