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

package deploy

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// DefaultPollInterval is how often Execute polls GetTorrentStatus while
// content slots are seeding, when Executor.PollInterval is zero.
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
	// graceful Shutdown already succeeded.
	Stop func() error
}

// EzioLauncher starts a local ezio daemon in raw-device mode (AddTorrent's
// save_path targets a partition device directly, never a -F content
// directory) and returns a handle to it.
type EzioLauncher interface {
	Launch(ctx context.Context) (EzioHandle, error)
}

// TorrentFetcher fetches a torrent slot's .torrent file bytes from a
// DeployTorrent.URL. The seeder pod that serves it may still be coming up
// when a deploy reaches this slot, so a production implementation retries
// transiently rather than failing on the first attempt.
type TorrentFetcher interface {
	FetchTorrent(ctx context.Context, url string) ([]byte, error)
}

// Executor runs a DeployPlan against the machine it executes on.
type Executor struct {
	// Runner executes sfdisk, blockdev, mkfs.*, and mkswap.
	Runner Runner
	// Ezio launches the local ezio daemon used for every torrent slot
	// across the whole plan. Only consulted when the plan actually has a
	// torrent slot; a plan with only mkfs and swap slots never launches
	// ezio at all.
	Ezio EzioLauncher
	// Fetcher fetches each torrent slot's .torrent file from its
	// DeployTorrent.URL. Only consulted under the same condition as Ezio.
	Fetcher TorrentFetcher
	// Progress reports step progress as the deploy proceeds. Nil means
	// progress is not reported (tests only; production always wires
	// one).
	Progress Reporter
	// Logf receives progress and diagnostic messages in fmt.Printf
	// style. Nil discards them.
	Logf func(format string, args ...any)
	// Console, when non-nil, additionally receives finalize's boot
	// summary written directly, once, right after the terminal progress
	// report. That report's Message is the primary channel; this is a
	// belt-and-braces echo, since the report travels over a network
	// request that can silently lose it, and a failed report is only
	// logged, never worth failing an otherwise-complete deploy over. Nil
	// skips the echo entirely.
	Console io.Writer
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

// diskPlan is one disk's plan within a DeployPlan, normalizing the plan's
// top-level TargetDisk/SfdiskScript/Slots (the OS image) and each
// DataImages entry into a single shape Execute's disk-level helpers share.
type diskPlan struct {
	// label identifies this disk plan in error messages and logs: "OS
	// image", or "dataImages[<i>] <imageRef>" for a data image.
	label        string
	disk         string
	sfdiskScript string
	slots        []agentapi.DeploySlot
}

// diskPlans returns plan's disk plans in deploy order: the OS image
// first, then each dataImages entry in spec order.
func diskPlans(plan *agentapi.DeployPlan) []diskPlan {
	plans := []diskPlan{{
		label:        "OS image",
		disk:         plan.TargetDisk,
		sfdiskScript: plan.SfdiskScript,
		slots:        plan.Slots,
	}}
	for i, di := range plan.DataImages {
		plans = append(plans, diskPlan{
			label:        fmt.Sprintf("dataImages[%d] %s", i, di.ImageRef.Name),
			disk:         di.TargetDisk,
			sfdiskScript: di.SfdiskScript,
			slots:        di.Slots,
		})
	}
	return plans
}

// Execute runs plan end to end: validates it, writes every disk's
// partition table, makes/restores every slot, runs the plan's resolved
// hooks in order (including any finalize-shaped builtin steps the plan
// carries - see the package doc comment for why this package hard-codes
// none of its own), and hands the machine off to the deployed OS. It
// reports progress through e.Progress as it goes.
//
// Whenever it returns a non-nil error, the deferred call below sends one
// last progress report carrying agentapi.ProgressStateFailed with the
// error as Message: without it, the controller's real Deployer (which
// polls the DeployRun's phase for either the terminal success phase or a
// failure) would poll forever after a failed deploy that silently
// stopped reporting.
func (e *Executor) Execute(ctx context.Context, plan *agentapi.DeployPlan) (err error) {
	if verr := plan.Validate(); verr != nil {
		return fmt.Errorf("invalid plan: %w", verr)
	}

	step := keziov1alpha2.DeployRunPhasePartitioning
	defer func() {
		if err != nil {
			e.reportFailed(ctx, plan, step, err.Error())
		}
	}()

	plans := diskPlans(plan)
	if verr := e.validate(ctx, plans); verr != nil {
		return fmt.Errorf("refusing to deploy: %w", verr)
	}

	e.reportRunning(ctx, plan, step)
	for _, dp := range plans {
		if err := e.writePartitionTable(ctx, dp); err != nil {
			return err
		}
	}
	e.reportSucceeded(ctx, plan, step, "")

	step = keziov1alpha2.DeployRunPhaseWritingContent
	e.reportRunning(ctx, plan, step)

	hasTorrent := hasTorrentSlot(plans)
	var handle EzioHandle
	if hasTorrent {
		handle, err = e.Ezio.Launch(ctx)
		if err != nil {
			return fmt.Errorf("launching local ezio daemon: %w", err)
		}
		defer func() {
			if handle.Stop != nil {
				_ = handle.Stop()
			}
		}()
	}

	for _, dp := range plans {
		if err := e.writeSlots(ctx, dp, handle); err != nil {
			return err
		}
	}

	if hasTorrent {
		if err := e.seedUntilStopped(ctx, plan, handle.Client, plans); err != nil {
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
	e.reportSucceeded(ctx, plan, step, "")

	// Hooks run once every slot across the whole plan is fully written
	// and before the machine reboots, so a chrootScript step sees a
	// complete, mountable root file system and a script step (or a
	// builtin like "efibootmgr"/"growLastPartition") can still run.
	step = keziov1alpha2.DeployRunPhaseRunningPostHook
	e.reportRunning(ctx, plan, step)
	if err := e.runHooks(ctx, plan); err != nil {
		return fmt.Errorf("running post hooks: %w", err)
	}
	e.reportSucceeded(ctx, plan, step, "")

	// Finalizing has nothing of its own to do - every finalize-shaped
	// action (efibootmgr, growLastPartition, ...) already ran as a hook
	// step above, on the plan's own explicit request. This phase report
	// exists only to keep the DeployRun's phase sequence complete.
	step = keziov1alpha2.DeployRunPhaseFinalizing
	e.reportRunning(ctx, plan, step)
	e.reportSucceeded(ctx, plan, step, "")

	// The terminal report: everything above completed, so this is the
	// deploy's success signal. It is sent before invoking systemctl
	// below, since nothing sent after that call is guaranteed to land.
	step = keziov1alpha2.DeployRunPhaseSucceeded
	e.reportSucceeded(ctx, plan, step, "")
	e.writeConsole("deploy finalized")

	if err := e.rebootOrPowerOff(ctx, plan.AfterDeploy); err != nil {
		return fmt.Errorf("after deploy action: %w", err)
	}
	return nil
}

// hasTorrentSlot reports whether any disk plan has at least one torrent
// slot - the signal Execute uses to decide whether to launch a local
// ezio daemon at all.
func hasTorrentSlot(plans []diskPlan) bool {
	for _, dp := range plans {
		for _, s := range dp.slots {
			if s.Torrent != nil {
				return true
			}
		}
	}
	return false
}

// validate checks every disk plan's disk and slot set before Execute
// issues a single destructive command against any of them: each disk is
// a plausible device path, no two disk plans target the same disk, each
// disk reports (through Runner) a size at least as large as its
// partition table requires, and each slot's device path is exactly what
// agentapi.DevicePartitionPath predicts from its own disk and number - a
// mismatch here means the plan is internally inconsistent (a manager bug
// or transport corruption), and the agent trusts the plan's disk/device
// fields as the *only* allowed write targets specifically because this
// check holds.
func (e *Executor) validate(ctx context.Context, plans []diskPlan) error {
	seenDisks := make(map[string]bool, len(plans))
	for _, dp := range plans {
		if !strings.HasPrefix(dp.disk, "/dev/") {
			return fmt.Errorf("%s: disk %q is not an absolute /dev/ path", dp.label, dp.disk)
		}
		if seenDisks[dp.disk] {
			return fmt.Errorf("disk %s is targeted by more than one image in this plan", dp.disk)
		}
		seenDisks[dp.disk] = true

		for _, s := range dp.slots {
			want := DevicePartitionPath(dp.disk, s.Number)
			if s.Device != want {
				return fmt.Errorf("%s slot %d: device %q does not match the expected path %q for disk %q",
					dp.label, s.Number, s.Device, want, dp.disk)
			}
		}
	}

	// Only once every disk plan has passed the structural checks above
	// does validate touch the machine at all, and even here only through
	// a read-only size query (blockdev --getsize64) - still before any
	// destructive command runs against any disk in the plan.
	for _, dp := range plans {
		if err := e.checkDiskSize(ctx, dp); err != nil {
			return err
		}
	}
	return nil
}

// checkDiskSize sanity-checks that dp.disk exists and reports a size at
// least as large as dp.sfdiskScript's layout requires, via `blockdev
// --getsize64` - a command that itself fails if the device node is
// absent, so this single check covers both "the device exists" and "the
// device is large enough".
func (e *Executor) checkDiskSize(ctx context.Context, dp diskPlan) error {
	minBytes, err := MinimumDiskBytes(dp.sfdiskScript)
	if err != nil {
		return fmt.Errorf("%s: %w", dp.label, err)
	}

	out, err := e.Runner.Run(ctx, nil, "blockdev", "--getsize64", dp.disk)
	if err != nil {
		return fmt.Errorf("%s: checking disk %s exists and is readable: %w", dp.label, dp.disk, err)
	}
	actual, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: parsing size of disk %s: %w", dp.label, dp.disk, err)
	}
	if actual < minBytes {
		return fmt.Errorf("%s: disk %s is %d bytes, smaller than the %d bytes its layout requires",
			dp.label, dp.disk, actual, minBytes)
	}
	return nil
}

// writePartitionTable writes dp's partition table and re-reads it.
func (e *Executor) writePartitionTable(ctx context.Context, dp diskPlan) error {
	script, err := SfdiskScript(dp.sfdiskScript, dp.disk)
	if err != nil {
		return fmt.Errorf("%s: %w", dp.label, err)
	}

	e.log("writing partition table to %s (%s)", dp.disk, dp.label)
	if _, err := e.Runner.Run(ctx, []byte(script), "sfdisk", dp.disk); err != nil {
		return fmt.Errorf("%s: writing partition table to %s: %w", dp.label, dp.disk, err)
	}
	if _, err := e.Runner.Run(ctx, nil, "blockdev", "--rereadpt", dp.disk); err != nil {
		return fmt.Errorf("%s: re-reading partition table on %s: %w", dp.label, dp.disk, err)
	}
	return nil
}

// writeSlots makes/restores every slot in dp: mkswap for a Swap slot,
// mkfs for a Mkfs slot, or AddTorrent (against handle.Client) for a
// Torrent slot. handle is the zero EzioHandle when dp has no torrent
// slots - writeSlots never dereferences handle.Client in that case.
func (e *Executor) writeSlots(ctx context.Context, dp diskPlan, handle EzioHandle) error {
	for _, s := range dp.slots {
		if err := e.writeSlot(ctx, dp, s, handle); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) writeSlot(ctx context.Context, dp diskPlan, s agentapi.DeploySlot, handle EzioHandle) error {
	switch {
	case s.Swap != nil:
		return e.runMkswap(ctx, dp, s)

	case s.Mkfs != nil:
		return e.runMkfs(ctx, dp, s)

	case s.Torrent != nil:
		e.log("fetching torrent for %s (info hash %s) from %s", s.Device, s.Torrent.InfoHash, s.Torrent.URL)
		torrentBytes, err := e.Fetcher.FetchTorrent(ctx, s.Torrent.URL)
		if err != nil {
			return fmt.Errorf("%s slot %d: fetching torrent from %s: %w", dp.label, s.Number, s.Torrent.URL, err)
		}
		e.log("adding torrent for %s (info hash %s)", s.Device, s.Torrent.InfoHash)
		if err := handle.Client.AddTorrent(ctx, torrentBytes, s.Device, false, seeder.DefaultMaxUploads, seeder.DefaultMaxConnections); err != nil {
			return fmt.Errorf("%s slot %d: AddTorrent %s: %w", dp.label, s.Number, s.Device, err)
		}
		return nil

	default:
		// plan.Validate already rejects a slot with none of the three
		// set, so this is unreachable in practice.
		return fmt.Errorf("%s slot %d: no content kind set", dp.label, s.Number)
	}
}

// runMkfs creates s's file system, passing its label/UUID (when the plan
// set them) with the flag each of kezio's supported file systems takes
// for that: mkfs.xfs is the one exception, taking a UUID through
// "-m uuid=<value>" rather than a bare flag+value pair.
func (e *Executor) runMkfs(ctx context.Context, dp diskPlan, s agentapi.DeploySlot) error {
	fsType := s.Mkfs.Filesystem
	e.log("mkfs.%s %s", fsType, s.Device)

	var args []string
	if s.Mkfs.Label != "" {
		args = append(args, mkfsLabelFlag(fsType), s.Mkfs.Label)
	}
	if s.Mkfs.UUID != "" {
		args = append(args, mkfsUUIDArgs(fsType, s.Mkfs.UUID)...)
	}
	args = append(args, s.Device)

	if _, err := e.Runner.Run(ctx, nil, "mkfs."+fsType, args...); err != nil {
		return fmt.Errorf("%s slot %d: mkfs.%s %s: %w", dp.label, s.Number, fsType, s.Device, err)
	}
	return nil
}

// mkfsLabelFlag returns the flag mkfs.<fsType> takes for a volume label:
// "-n" for vfat, "-L" for every other file system kezio supports
// (ext2/3/4, xfs, btrfs).
func mkfsLabelFlag(fsType string) string {
	if fsType == "vfat" {
		return "-n"
	}
	return "-L"
}

// mkfsUUIDArgs returns the argument(s) mkfs.<fsType> takes for a file
// system UUID: mkfs.xfs alone takes it through "-m uuid=<value>" rather
// than a bare flag; mkfs.vfat's "-i" sets a 32-bit volume ID, the closest
// vfat has to a UUID; every other file system kezio supports takes "-U".
func mkfsUUIDArgs(fsType, uuid string) []string {
	switch fsType {
	case "xfs":
		return []string{"-m", "uuid=" + uuid}
	case "vfat":
		return []string{"-i", uuid}
	default:
		return []string{"-U", uuid}
	}
}

// runMkswap runs mkswap against s (a swap slot, s.Swap set), restoring
// its source UUID when one was given. Extracted from writeSlot so the
// "mkswap" builtin post hook step (hooks.go) can run the same command
// against every swap slot in the plan, on explicit request, without
// duplicating it.
func (e *Executor) runMkswap(ctx context.Context, dp diskPlan, s agentapi.DeploySlot) error {
	e.log("mkswap %s (uuid %s)", s.Device, s.Swap.UUID)
	args := []string{s.Device}
	if s.Swap.UUID != "" {
		args = append([]string{"--uuid", s.Swap.UUID}, args...)
	}
	if _, err := e.Runner.Run(ctx, nil, "mkswap", args...); err != nil {
		return fmt.Errorf("%s slot %d: mkswap %s: %w", dp.label, s.Number, s.Device, err)
	}
	return nil
}

// seedUntilStopped polls client for status until every torrent slot
// tracked across plans is finished and paused (allStopped), pausing each
// torrent as soon as shouldPauseTorrent says so and reporting the
// latest aggregate percent/bytes on every tick. It stops early -
// returning an error - only if ctx is cancelled; a single failed poll or
// pause RPC is logged and retried on the next tick.
func (e *Executor) seedUntilStopped(ctx context.Context, plan *agentapi.DeployPlan, client EzioClient, plans []diskPlan) error {
	hashes := torrentHashes(plans)
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
			var totalDone, total int64
			for hash, t := range statuses {
				totalDone += t.TotalDone
				total += t.Total
				if shouldPauseTorrent(t) {
					if err := client.PauseTorrent(ctx, hash); err != nil {
						e.log("pausing torrent %s: %v", hash, err)
					}
				}
			}
			e.reportProgress(ctx, plan, keziov1alpha2.DeployRunPhaseWritingContent, aggregatePercent(total, totalDone), totalDone)
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

// torrentHashes returns every torrent slot's info hash across plans, for
// GetTorrentStatus's hash filter.
func torrentHashes(plans []diskPlan) []string {
	var hashes []string
	for _, dp := range plans {
		for _, s := range dp.slots {
			if s.Torrent != nil {
				hashes = append(hashes, s.Torrent.InfoHash)
			}
		}
	}
	return hashes
}

// aggregatePercent returns done as a 0-100 percentage of total, clamped,
// or 0 when total is not yet known (0).
func aggregatePercent(total, done int64) int64 {
	if total <= 0 {
		return 0
	}
	percent := done * 100 / total
	switch {
	case percent < 0:
		return 0
	case percent > 100:
		return 100
	default:
		return percent
	}
}

// writeConsole best-effort echoes msg to e.Console. A nil Console (every
// test that does not set one, and any production build that failed to
// open /dev/console) makes this a no-op; a write error is logged, not
// propagated.
func (e *Executor) writeConsole(msg string) {
	if e.Console == nil {
		return
	}
	if _, err := fmt.Fprintf(e.Console, "kezio-agent: %s\n", msg); err != nil {
		e.log("writing to console failed: %v", err)
	}
}

// rebootOrPowerOff hands the machine off to the deployed OS once every
// hook has run and the terminal progress report is sent: systemctl
// reboot for keziov1alpha2.AfterDeployReboot (the default, including an
// empty AfterDeploy), systemctl poweroff for AfterDeployPowerOff.
func (e *Executor) rebootOrPowerOff(ctx context.Context, afterDeploy string) error {
	action := "reboot"
	if afterDeploy == keziov1alpha2.AfterDeployPowerOff {
		action = "poweroff"
	}

	e.log("deploy finalized; invoking systemctl %s", action)
	if _, err := e.Runner.Run(ctx, nil, "systemctl", action); err != nil {
		return fmt.Errorf("systemctl %s: %w", action, err)
	}
	return nil
}
