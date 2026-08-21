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

// This file implements the finalize-shaped builtin post hook steps:
// "efibootmgr", "install-removable-fallback", and "growLastPartition".
// Unlike the reference this package was ported from, none of these run
// unconditionally - a DeployPlan carries no per-slot role, so this
// package has no way to find "the ESP partition" or "the disk to grow"
// on its own. Each of these three builtins instead takes its target
// through ResolvedHookStep.Params, which the operator's PostHook step
// fills in from what it already knows about the image's own layout.
package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

// efiRemovableLoaderPath is the UEFI spec's own fallback removable-media
// bootloader path for one CPU architecture - the exact file firmware
// falls back to on its own, no NVRAM boot entry involved at all, whenever
// nothing in BootOrder succeeds. ensureEFIBootEntry always registers its
// NVRAM entry against this same path rather than a distro-specific
// \EFI\<distro>\... one, since this package never opens or inspects the
// deployed file system and so has no way to know which path an arbitrary
// image's own installer used.
//
// This is kezio's documented image contract: a bootable Image must
// already carry its fallback bootloader at this path on its ESP. Nothing
// here checks for it - an image missing it will get an NVRAM entry that
// still fails to boot the moment that entry is lost. The
// "install-removable-fallback" builtin below copies a shim/grub file into
// place for a golden image that does not already carry one.
var efiRemovableLoaderPath = map[string]string{
	"amd64": `\EFI\BOOT\BOOTX64.EFI`,  // x86_64
	"arm64": `\EFI\BOOT\BOOTAA64.EFI`, // aarch64
}

// efiRemovableLoaderPathForArch looks goarch (a Go GOARCH value - the
// deploying machine's own, since kezio-agent runs on the machine it is
// deploying) up in efiRemovableLoaderPath. kezio supports exactly two
// architectures, x86_64 and aarch64; any other GOARCH is refused
// explicitly rather than silently defaulting to the x86_64 path.
func efiRemovableLoaderPathForArch(goarch string) (string, error) {
	path, ok := efiRemovableLoaderPath[goarch]
	if !ok {
		return "", fmt.Errorf("unsupported architecture %q for UEFI boot entries; kezio supports x86_64 and aarch64 only", goarch)
	}
	return path, nil
}

// efibootLabelPrefix, plus the deploying machine's identity, is the
// stable label the "efibootmgr" builtin gives every boot entry it
// creates: stable across repeated deploys of the same machine
// (ensureEFIBootEntry deletes a prior entry carrying this exact label
// before creating a fresh one), so a redeploy never accumulates a
// duplicate NVRAM entry.
const efibootLabelPrefix = "kezio:"

// efibootLabel returns plan.MachineName's label, falling back to
// plan.RunName when MachineName is empty (a plan built before that field
// existed) - at the cost of the redeploy-dedup property efibootLabelPrefix's
// doc comment describes, since RunName is different on every DeployRun.
func efibootLabel(plan *agentapi.DeployPlan) string {
	name := plan.MachineName
	if name == "" {
		name = plan.RunName
	}
	return efibootLabelPrefix + name
}

// finalizeDisk returns params["disk"], defaulting to plan.TargetDisk -
// the disk every finalize-shaped builtin in this file targets unless a
// PostHook step overrides it.
func finalizeDisk(plan *agentapi.DeployPlan, params map[string]string) string {
	if disk := params["disk"]; disk != "" {
		return disk
	}
	return plan.TargetDisk
}

// requirePartition parses params["part"] as a positive partition number.
// There is no sensible default for "which partition is the ESP" without
// per-slot role on the wire (see this file's own doc comment), so a
// missing or invalid value is refused rather than guessed.
func requirePartition(params map[string]string) (int32, error) {
	raw := params["part"]
	if raw == "" {
		return 0, fmt.Errorf(`params["part"] is required (the ESP's partition number)`)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf(`params["part"] = %q is not a positive partition number`, raw)
	}
	return int32(n), nil //nolint:gosec // bounded by strconv.Atoi and the CRD's own MaxItems on slots
}

// runBuiltinEfibootmgr creates (or replaces) the UEFI boot entry for the
// ESP named by params (see finalizeDisk/requirePartition).
func runBuiltinEfibootmgr(ctx context.Context, e *Executor, plan *agentapi.DeployPlan, params map[string]string) error {
	part, err := requirePartition(params)
	if err != nil {
		return fmt.Errorf("efibootmgr: %w", err)
	}
	return e.ensureEFIBootEntry(ctx, plan, finalizeDisk(plan, params), part)
}

// ensureEFIBootEntry creates a UEFI NVRAM boot entry pointing at
// partition part of disk and the deploying machine's own architecture's
// fallback loader path (efiRemovableLoaderPathForArch of runtime.GOARCH -
// kezio-agent runs on the machine it is deploying, so its own build
// architecture is the target's), first removing any prior entry this
// same machine created - matched by label, not boot number, since
// efibootmgr assigns those itself and they are not stable across
// firmware or across which entries currently exist. This only ever
// touches firmware NVRAM through efibootmgr, never the ESP's own file
// system content.
func (e *Executor) ensureEFIBootEntry(ctx context.Context, plan *agentapi.DeployPlan, disk string, part int32) error {
	loaderPath, err := efiRemovableLoaderPathForArch(runtime.GOARCH)
	if err != nil {
		return err
	}

	label := efibootLabel(plan)

	out, err := e.Runner.Run(ctx, nil, "efibootmgr")
	if err != nil {
		return fmt.Errorf("listing existing UEFI boot entries: %w", err)
	}
	for _, num := range parseEFIBootEntries(string(out), label) {
		e.log("removing prior UEFI boot entry Boot%s (label %q)", num, label)
		if _, err := e.Runner.Run(ctx, nil, "efibootmgr", "--bootnum", num, "--delete-bootnum"); err != nil {
			return fmt.Errorf("deleting prior UEFI boot entry Boot%s (label %q): %w", num, label, err)
		}
	}

	e.log("creating UEFI boot entry %q on %s partition %d, loader %s", label, disk, part, loaderPath)
	if _, err := e.Runner.Run(ctx, nil, "efibootmgr",
		"--create",
		"--disk", disk,
		"--part", strconv.Itoa(int(part)),
		"--loader", loaderPath,
		"--label", label,
	); err != nil {
		return fmt.Errorf("creating UEFI boot entry: %w", err)
	}
	return nil
}

// parseEFIBootEntries scans efibootmgr's no-argument listing output
// (lines shaped "BootXXXX* <label>", the "*" marking the active default
// only present when set) for entries whose label is exactly label,
// returning each match's 4 hex digit boot number - the identifier
// "--bootnum"/"--delete-bootnum" needs to remove it.
func parseEFIBootEntries(listing, label string) []string {
	var nums []string
	for line := range strings.SplitSeq(listing, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Boot") || len(line) < 8 {
			continue
		}
		num := line[4:8]
		if _, err := strconv.ParseUint(num, 16, 16); err != nil {
			continue // not "BootXXXX...": some other line efibootmgr printed
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line[8:], "*"))
		if rest == label {
			nums = append(nums, num)
		}
	}
	return nums
}

// removableFallbackCandidates are the file names
// installRemovableFallback looks for under any other EFI/<name>/
// directory on the ESP - shim first (so a secure-boot image keeps
// chaining through it to grub), grub itself as the fallback for a
// non-shim image.
var removableFallbackCandidates = []string{"shimx64.efi", "grubx64.efi"}

// runBuiltinInstallRemovableFallback mounts the ESP named by params and
// makes sure it carries a bootloader at the image contract's fallback
// path (efiRemovableLoaderPath), copying one in from
// removableFallbackCandidates when it is missing.
func runBuiltinInstallRemovableFallback(ctx context.Context, e *Executor, plan *agentapi.DeployPlan, params map[string]string) error {
	part, err := requirePartition(params)
	if err != nil {
		return fmt.Errorf("install-removable-fallback: %w", err)
	}
	disk := finalizeDisk(plan, params)
	return e.installRemovableFallback(ctx, disk, DevicePartitionPath(disk, part))
}

// installRemovableFallback mounts espDevice (an ESP partition of disk)
// and makes sure it carries a bootloader at the deploying machine's own
// architecture's fallback path (efiRemovableLoaderPathForArch of
// runtime.GOARCH), installing one from whichever EFI/<name>/ directory
// holds a removableFallbackCandidates match when it does not already.
func (e *Executor) installRemovableFallback(ctx context.Context, disk, espDevice string) error {
	loaderPath, err := efiRemovableLoaderPathForArch(runtime.GOARCH)
	if err != nil {
		return err
	}
	// loaderPath is the NVRAM-style path efibootmgr takes ("\EFI\BOOT\...");
	// relPath is the same path as a POSIX-style relative path under an ESP
	// mount, the form this function needs to check for and, if absent,
	// create the file at.
	relPath := filepath.FromSlash(strings.ReplaceAll(strings.TrimPrefix(loaderPath, `\`), `\`, "/"))

	mountpoint, err := os.MkdirTemp("", "kezio-esp-")
	if err != nil {
		return fmt.Errorf("creating ESP mountpoint: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), hookCleanupTimeout)
		e.unmountAll(cleanupCtx, []string{mountpoint})
		cancel()
		if err := os.Remove(mountpoint); err != nil && !os.IsNotExist(err) {
			e.log("removing ESP mountpoint %s: %v", mountpoint, err)
		}
	}()

	// -t vfat is explicit rather than left to mount's own autoprobe: the
	// UEFI spec requires the ESP to carry a FAT file system, but a bare
	// mount call lets the kernel guess a type from whatever superblocks
	// it tries first, which can pick something else entirely when the
	// device's content happens to look ambiguous to autoprobe.
	if _, err := e.Runner.Run(ctx, nil, "mount", "-t", "vfat", espDevice, mountpoint); err != nil {
		return fmt.Errorf("mounting ESP %s (disk %s) at %s: %w", espDevice, disk, mountpoint, err)
	}

	installed, source, err := ensureRemovableFallbackFile(mountpoint, relPath)
	if err != nil {
		return fmt.Errorf("ensuring removable fallback bootloader: %w", err)
	}
	if installed {
		e.log("installed removable fallback bootloader on %s from %s", espDevice, source)
	}
	return nil
}

// ensureRemovableFallbackFile is installRemovableFallback's pure part:
// given root (an already-mounted ESP) and relPath (the ESP-relative
// fallback path to ensure), it reports whether relPath is already
// present, installing a copy from the first removableFallbackCandidates
// match under any other EFI/<name>/ directory when it is not. source
// names whichever file it copied from ("" when nothing needed copying).
func ensureRemovableFallbackFile(root, relPath string) (installed bool, source string, err error) {
	fallback := filepath.Join(root, relPath)
	if _, statErr := os.Stat(fallback); statErr == nil {
		return false, "", nil
	} else if !os.IsNotExist(statErr) {
		return false, "", fmt.Errorf("checking %s: %w", fallback, statErr)
	}

	src, err := findESPBootloader(root)
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		return false, "", fmt.Errorf("creating %s: %w", filepath.Dir(fallback), err)
	}
	if err := copyFile(src, fallback); err != nil {
		return false, "", fmt.Errorf("copying %s to %s: %w", src, fallback, err)
	}
	return true, src, nil
}

// findESPBootloader looks for removableFallbackCandidates under every
// EFI/<name>/ directory of root other than EFI/BOOT itself (that is the
// fallback's destination, never a source), returning the first match in
// directory-then-candidate order.
func findESPBootloader(root string) (string, error) {
	efiDir := filepath.Join(root, "EFI")
	entries, err := os.ReadDir(efiDir)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", efiDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.EqualFold(entry.Name(), "BOOT") {
			continue
		}
		for _, candidate := range removableFallbackCandidates {
			path := filepath.Join(efiDir, entry.Name(), candidate)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no bootloader (%s) found under any EFI/<name>/ directory in %s", strings.Join(removableFallbackCandidates, ", "), efiDir)
}

// copyFile copies src to dst, creating dst world-readable - firmware does
// not check the execute bit, and everything already on an ESP is
// similarly non-executable from the host OS's point of view.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// runBuiltinGrowLastPartition grows one disk plan's last partition (and
// its file system, when this package knows how to resize it and knows
// its type). params:
//
//   - "disk": which disk to grow, defaulting to plan.TargetDisk.
//   - "partition": which partition number to grow, defaulting to the
//     last slot's number in that disk's own plan - only resolvable when
//     "disk" is (or defaults to) plan.TargetDisk, since only the OS
//     disk's slot list is directly reachable from the top-level plan.
//   - "fsType": the file system to resize, when the target slot's own
//     kind does not already carry one on the wire (a torrent slot's file
//     system type is not part of DeployTorrent) - omit to grow the
//     partition only, leaving its file system at its original size.
func runBuiltinGrowLastPartition(ctx context.Context, e *Executor, plan *agentapi.DeployPlan, params map[string]string) error {
	disk := finalizeDisk(plan, params)

	var partition int32
	var device, fsType string
	if raw := params["partition"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return fmt.Errorf(`growLastPartition: params["partition"] = %q is not a positive partition number`, raw)
		}
		partition = int32(n) //nolint:gosec // bounded above
		device = DevicePartitionPath(disk, partition)
	} else {
		if disk != plan.TargetDisk {
			return fmt.Errorf(`growLastPartition: disk %q has no default partition; pass params["partition"] explicitly`, disk)
		}
		if len(plan.Slots) == 0 {
			return fmt.Errorf("growLastPartition: plan has no OS disk slots")
		}
		last := plan.Slots[len(plan.Slots)-1]
		partition = last.Number
		device = last.Device
		if last.Mkfs != nil {
			fsType = last.Mkfs.Filesystem
		}
	}
	if v := params["fsType"]; v != "" {
		fsType = v
	}

	return e.growLastPartition(ctx, disk, partition, device, fsType)
}

// growLastPartition runs growpart against partition on disk, then - when
// fsType names a file system this package knows how to resize online -
// resizes it to fill the grown partition. An empty or unresizable fsType
// is not an error: the partition itself is grown; the file system inside
// it (if any) is left at its original size, which is safe (it just
// wastes the extra space) rather than guessing at a tool that might not
// exist in the live image.
func (e *Executor) growLastPartition(ctx context.Context, disk string, partition int32, device, fsType string) error {
	e.log("growing partition %s (number %d) on %s", device, partition, disk)
	if _, err := e.Runner.Run(ctx, nil, "growpart", disk, strconv.Itoa(int(partition))); err != nil {
		return fmt.Errorf("growing partition %d on %s: %w", partition, disk, err)
	}

	resizeCmd, ok := fsResizeCommand(fsType, device)
	if !ok {
		return nil
	}
	e.log("growing file system on %s: %s", device, strings.Join(resizeCmd, " "))
	if _, err := e.Runner.Run(ctx, nil, resizeCmd[0], resizeCmd[1:]...); err != nil {
		return fmt.Errorf("growing file system on %s: %w", device, err)
	}
	return nil
}

// fsResizeCommand returns the command that grows device's file system to
// fill its (already grown) partition, for the file system types this
// package knows how to resize online. ok is false for any other fsType
// (empty, or one this package has no resize tool for), telling
// growLastPartition to leave the file system alone.
func fsResizeCommand(fsType, device string) (cmd []string, ok bool) {
	switch fsType {
	case "ext2", "ext3", "ext4":
		return []string{"resize2fs", device}, true
	case "xfs":
		return []string{"xfs_growfs", device}, true
	case "btrfs":
		return []string{"btrfs", "filesystem", "resize", "max", device}, true
	default:
		return nil, false
	}
}
