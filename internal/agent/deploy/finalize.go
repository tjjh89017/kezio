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
	"strconv"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// bootloaderFallbackPath is the UEFI spec's own fallback removable-media
// bootloader path, independent of whatever distro-specific
// \EFI\<distro>\... path a bootloader installer would normally register
// for itself. finalize points every boot entry it creates at this path
// rather than trying to guess a distro-specific one: it never opens or
// inspects the deployed file system (see the package doc comment), so it
// has no way to know which path an arbitrary image's own installer used.
// Images kezio deploys must carry a bootloader at this path for the
// created entry to actually boot; image builders should ship or symlink
// one.
const bootloaderFallbackPath = `\EFI\BOOT\BOOTX64.EFI`

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
// grow of each image's last partition (and its file system), and a UEFI
// boot entry for whichever image plan carries an ESP-role partition
// (ordinarily the OS image only). See the package doc comment for why
// this never chroots or runs update-initramfs.
func (e *Executor) finalize(ctx context.Context, plan *agentapi.DeployPlan, plans []agentapi.ImageDeployPlan) error {
	for _, ip := range plans {
		if err := e.growLastPartition(ctx, ip); err != nil {
			return err
		}

		for _, p := range ip.Partitions {
			if p.Role != keziov1alpha1.PartitionRoleESP {
				continue
			}
			if err := e.ensureEFIBootEntry(ctx, plan.MachineName, ip, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureEFIBootEntry creates a UEFI NVRAM boot entry pointing at esp (an
// ESP-role partition of ip.Disk) and bootloaderFallbackPath, first
// removing any prior entry this same machine created - matched by label,
// not boot number, since efibootmgr assigns those itself and they are
// not stable across firmware or across which entries currently exist.
// This is finalize's only interaction with UEFI boot configuration: it
// only ever touches firmware NVRAM through efibootmgr, never the ESP's
// own file system content.
func (e *Executor) ensureEFIBootEntry(ctx context.Context, machineName string, ip agentapi.ImageDeployPlan, esp agentapi.PlanPartition) error {
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

	e.log("creating UEFI boot entry %q on %s partition %d, loader %s", label, ip.Disk, esp.Number, bootloaderFallbackPath)
	if _, err := e.Runner.Run(ctx, nil, "efibootmgr",
		"--create",
		"--disk", ip.Disk,
		"--part", strconv.Itoa(int(esp.Number)),
		"--loader", bootloaderFallbackPath,
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
// itself asks for. It is a no-op unless ip.GrowLastPartition is true;
// see that field's doc comment for why every plan the controller builds
// today leaves it false, making this path unreachable in production
// until a later work item wires a spec field to it.
func (e *Executor) growLastPartition(ctx context.Context, ip agentapi.ImageDeployPlan) error {
	if !ip.GrowLastPartition || len(ip.Partitions) == 0 {
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
// the agent doing its own reboot directly - a BMC-driven reboot is a
// later work item, not implemented here.
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
