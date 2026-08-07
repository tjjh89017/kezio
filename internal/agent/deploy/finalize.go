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

package deploy

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// efiRemovableLoaderPath is the UEFI spec's own fallback removable-media
// bootloader path for one CPU architecture - the exact file firmware
// falls back to on its own, no NVRAM boot entry involved at all,
// whenever nothing in BootOrder succeeds. ensureEFIBootEntry always
// registers its NVRAM entry against this same path rather than a
// distro-specific \EFI\<distro>\... one: finalize never opens or
// inspects the deployed file system (see the package doc comment), so it
// has no way to know which path an arbitrary image's own installer used.
//
// This is kezio's documented image contract: a bootable Image must
// already carry its fallback bootloader at this path on its ESP.
// finalize does not check for it (see the package doc comment on why
// finalize adds no validation of its own) - an image missing it will get
// an NVRAM entry that still fails to boot the moment that entry is lost.
// The "install-removable-fallback" builtin post hook step (hooks.go)
// copies a shim/grub file into place for a golden image that does not
// already carry one.
//
// kezio supports exactly two architectures, x86_64 and aarch64; this is
// a fixed two-entry table, not an extensible registry, and aarch64 is
// intentionally unimplemented (see efiRemovableLoaderPathForArch).
var efiRemovableLoaderPath = map[string]string{
	"amd64": `\EFI\BOOT\BOOTX64.EFI`, // x86_64
	// "arm64" (aarch64) is deliberately absent: BOOTAA64.EFI support is
	// not implemented yet. efiRemovableLoaderPathForArch fails explicitly
	// for it rather than falling through to the x86_64 path.
}

// efiRemovableLoaderPathForArch looks goarch (a Go GOARCH value - the
// deploying machine's own, since kezio-agent runs on the machine it is
// deploying) up in efiRemovableLoaderPath, failing explicitly for
// aarch64 ("arm64") and any architecture outside kezio's two-architecture
// scope rather than silently defaulting to the x86_64 path.
func efiRemovableLoaderPathForArch(goarch string) (string, error) {
	path, ok := efiRemovableLoaderPath[goarch]
	if !ok {
		if goarch == "arm64" {
			return "", fmt.Errorf("UEFI boot entries for aarch64 are not implemented yet")
		}
		return "", fmt.Errorf("unsupported architecture %q for UEFI boot entries; kezio supports x86_64 and aarch64 only", goarch)
	}
	return path, nil
}

// efibootLabelPrefix, plus the deploying machine's name, is the stable
// label finalize gives every boot entry it creates: stable across
// repeated deploys of the same machine (ensureEFIBootEntry deletes a
// prior entry carrying this exact label before creating a fresh one), so
// a redeploy never accumulates a duplicate NVRAM entry.
const efibootLabelPrefix = "kezio:"

func efibootLabel(machineName string) string {
	return efibootLabelPrefix + machineName
}

// finalize runs every image plan's finalize actions once every
// partition across the whole DeployPlan is written or made: an optional
// grow of each image's last partition (and its file system), and for
// whichever image plan carries an ESP-role partition (ordinarily the OS
// image only) a UEFI boot entry pointing at the image contract's fallback
// bootloader path (efiRemovableLoaderPath). finalize itself never opens
// the ESP's file system - see the package doc comment for why this never
// chroots, runs update-initramfs, or otherwise inspects deployed content,
// and efiRemovableLoaderPath's doc comment for the fallback-bootloader
// contract this relies on the image to already satisfy.
//
// It returns a full "efibootmgr" listing taken right after the boot
// entry is created. Deploy's console carries none of the agent's own log
// output (the live environment's stdout does not reach the guest's
// serial console - see this package's doc comment), and the guest is
// gone the moment it reboots into the deployed disk, so this listing,
// threaded back to Execute's terminal progress report, is the only
// record of whether the boot entry actually landed that survives past
// the reboot this same call is about to trigger.
func (e *Executor) finalize(ctx context.Context, plan *agentapi.DeployPlan, plans []agentapi.ImageDeployPlan) (string, error) {
	var summary strings.Builder
	sawESP := false
	for _, ip := range plans {
		if err := e.growLastPartition(ctx, ip, false); err != nil {
			return summary.String(), err
		}

		for _, p := range ip.Partitions {
			if p.Role != keziov1alpha1.PartitionRoleESP {
				continue
			}
			sawESP = true
			if err := e.ensureEFIBootEntry(ctx, plan.MachineName, ip, p); err != nil {
				return summary.String(), err
			}
		}
	}
	if sawESP {
		summary.WriteString(e.efibootmgrSnapshot(ctx))
	}
	return summary.String(), nil
}

// efibootmgrSnapshot runs a plain "efibootmgr" listing and returns its
// output flattened to one line (newlines replaced with " | "), or a
// short placeholder naming the error - never fails finalize itself,
// since this call exists purely to make the resulting NVRAM state (which
// entries exist, what BootOrder and BootCurrent are) visible in the
// terminal progress report; a listing failure here must not turn into a
// deploy failure over an already-successful boot entry creation.
func (e *Executor) efibootmgrSnapshot(ctx context.Context) string {
	out, err := e.Runner.Run(ctx, nil, "efibootmgr")
	if err != nil {
		return fmt.Sprintf("efibootmgr listing after finalize failed: %v", err)
	}
	return strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | ")
}

// ensureEFIBootEntry creates a UEFI NVRAM boot entry pointing at esp (an
// ESP-role partition of ip.Disk) and the deploying machine's own
// architecture's fallback loader path (efiRemovableLoaderPathForArch of
// runtime.GOARCH - kezio-agent runs on the machine it is deploying, so
// its own build architecture is the target's), first removing any prior
// entry this same machine created - matched by label, not boot number,
// since efibootmgr assigns those itself and they are not stable across
// firmware or across which entries currently exist. This only ever
// touches firmware NVRAM through efibootmgr, never the ESP's own file
// system content.
func (e *Executor) ensureEFIBootEntry(ctx context.Context, machineName string, ip agentapi.ImageDeployPlan, esp agentapi.PlanPartition) error {
	loaderPath, err := efiRemovableLoaderPathForArch(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("image %s: %w", ip.ImageRef.Name, err)
	}

	label := efibootLabel(machineName)

	out, err := e.Runner.Run(ctx, nil, "efibootmgr")
	if err != nil {
		return fmt.Errorf("image %s: listing existing UEFI boot entries: %w", ip.ImageRef.Name, err)
	}
	for _, num := range parseEFIBootEntries(string(out), label) {
		e.log("removing prior UEFI boot entry Boot%s (label %q)", num, label)
		if _, err := e.Runner.Run(ctx, nil, "efibootmgr", "--bootnum", num, "--delete-bootnum"); err != nil {
			return fmt.Errorf("image %s: deleting prior UEFI boot entry Boot%s (label %q): %w", ip.ImageRef.Name, num, label, err)
		}
	}

	e.log("creating UEFI boot entry %q on %s partition %d, loader %s", label, ip.Disk, esp.Number, loaderPath)
	if _, err := e.Runner.Run(ctx, nil, "efibootmgr",
		"--create",
		"--disk", ip.Disk,
		"--part", strconv.Itoa(int(esp.Number)),
		"--loader", loaderPath,
		"--label", label,
	); err != nil {
		return fmt.Errorf("image %s: creating UEFI boot entry: %w", ip.ImageRef.Name, err)
	}
	return nil
}

// parseEFIBootEntries scans efibootmgr's no-argument listing output
// (lines shaped "BootXXXX* <label>", the "*" marking the active default
// only present when set) for entries whose label is exactly label,
// returning each match's 4 hex digit boot number - the identifier
// "--bootnum"/"--delete-bootnum" needs to remove it. A label appearing
// more than once (should not happen once ensureEFIBootEntry is the only
// writer, but a manually created duplicate is possible) returns every
// matching boot number, so every one of them gets removed.
func parseEFIBootEntries(listing, label string) []string {
	var nums []string
	for _, line := range strings.Split(listing, "\n") {
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

// growLastPartition grows ip's last partition and, when it has a file
// system finalize knows how to resize, that file system too - filling
// whatever extra space ip.Disk has beyond what ip.SfdiskJSON's layout
// itself asks for. It is a no-op unless ip.GrowLastPartition is true or
// force is true; see ip.GrowLastPartition's field comment for why every
// plan the controller builds today leaves it false, making the
// force=false path unreachable in production until a controller-side
// field starts setting it. force is set by the "growLastPartition"
// builtin post hook step (runHooks.go's builtin registry), which grows
// on explicit request regardless of ip.GrowLastPartition.
func (e *Executor) growLastPartition(ctx context.Context, ip agentapi.ImageDeployPlan, force bool) error {
	if (!force && !ip.GrowLastPartition) || len(ip.Partitions) == 0 {
		return nil
	}
	last := ip.Partitions[len(ip.Partitions)-1]

	e.log("growing last partition %s (number %d) on %s", last.Device, last.Number, ip.Disk)
	if _, err := e.Runner.Run(ctx, nil, "growpart", ip.Disk, strconv.Itoa(int(last.Number))); err != nil {
		return fmt.Errorf("image %s: growing partition %d on %s: %w", ip.ImageRef.Name, last.Number, ip.Disk, err)
	}

	resizeCmd, ok := fsResizeCommand(last.FSType, last.Device)
	if !ok {
		// No known resize tool for this file system (or none at all -
		// msr, swap): the partition itself is grown; the file system
		// inside it is left at its original size. That is safe - it
		// just wastes the extra space - rather than guessing at a tool
		// that might not exist in the live image.
		return nil
	}
	e.log("growing file system on %s: %s", last.Device, strings.Join(resizeCmd, " "))
	if _, err := e.Runner.Run(ctx, nil, resizeCmd[0], resizeCmd[1:]...); err != nil {
		return fmt.Errorf("image %s: growing file system on %s: %w", ip.ImageRef.Name, last.Device, err)
	}
	return nil
}

// fsResizeCommand returns the command that grows device's file system to
// fill its (already grown) partition, for the file system types finalize
// knows how to resize online. ok is false for any other fsType (msr, an
// empty FSType, or one finalize has no resize tool for), telling
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

// rebootOrPowerOff hands the machine off to the deployed OS once
// finalize and the terminal progress report are both done: systemctl
// reboot for keziov1alpha1.AfterDeployReboot (the default, including an
// empty AfterDeploy), systemctl poweroff for AfterDeployPowerOff. This is
// the agent doing its own reboot directly; a BMC-driven reboot is not
// implemented here.
func (e *Executor) rebootOrPowerOff(ctx context.Context, afterDeploy string) error {
	action := "reboot"
	if afterDeploy == keziov1alpha1.AfterDeployPowerOff {
		action = "poweroff"
	}

	e.log("deploy finalized; invoking systemctl %s", action)
	if _, err := e.Runner.Run(ctx, nil, "systemctl", action); err != nil {
		return fmt.Errorf("systemctl %s: %w", action, err)
	}
	return nil
}
