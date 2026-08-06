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
	"io"
	"os"
	"path/filepath"
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

// removableFallbackDir and removableFallbackName are bootloaderFallbackPath
// spelled as a POSIX-style relative path under an ESP mount, the form
// ensureRemovableFallback needs to check for and, if absent, create the
// file at. Firmware falls back to this exact path on its own - no NVRAM
// boot entry involved at all - whenever nothing in BootOrder succeeds,
// which is why ensureRemovableFallback exists alongside
// ensureEFIBootEntry rather than instead of it: it is the boot path that
// survives an NVRAM boot entry not surviving.
const (
	removableFallbackDir  = "EFI/BOOT"
	removableFallbackName = "BOOTX64.EFI"
)

// espBootloaderCandidates are the file names ensureRemovableFallback
// looks for under any other EFI/<name>/ directory on the ESP when
// removableFallbackName is missing - shim first (so a secure-boot image
// keeps chaining through it to grub), grub itself as the fallback for a
// non-shim image. These are the same two names kezio-bootd's own netboot
// chain fetches (cmd/bootd/main.go), so an image built for kezio's PXE
// flow already carries one of them somewhere under EFI/.
var espBootloaderCandidates = []string{"shimx64.efi", "grubx64.efi"}

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
// image only) a UEFI boot entry plus the removable-media fallback
// bootloader that entry does not depend on surviving. See the package
// doc comment for why this never chroots or runs update-initramfs.
func (e *Executor) finalize(ctx context.Context, plan *agentapi.DeployPlan, plans []agentapi.ImageDeployPlan) error {
	for _, ip := range plans {
		if err := e.growLastPartition(ctx, ip, false); err != nil {
			return err
		}

		for _, p := range ip.Partitions {
			if p.Role != keziov1alpha1.PartitionRoleESP {
				continue
			}
			if err := e.ensureEFIBootEntry(ctx, plan.MachineName, ip, p); err != nil {
				return err
			}
			if err := e.ensureRemovableFallback(ctx, ip, p); err != nil {
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
// This only ever touches firmware NVRAM through efibootmgr, never the
// ESP's own file system content - ensureRemovableFallback is the one
// finalize step that does open it.
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

// ensureRemovableFallback mounts esp (an ESP-role partition of ip.Disk)
// and makes sure it carries a bootloader at the UEFI spec's fixed
// removable-media path, EFI/BOOT/BOOTX64.EFI, installing one from
// whichever EFI/<name>/ directory holds an espBootloaderCandidates match
// when it does not already. Real hardware and virtualized firmware alike
// can lose an NVRAM boot entry a prior deploy created - a factory reset,
// a battery-backed CMOS finally dying, a hypervisor's EFI variable store
// not surviving a reboot the way its persistent-disk-backed one is
// supposed to - and the removable path is the one BdsDxe still finds on
// its own once BootOrder has nothing left to try, with no boot entry
// involved at all. Running this in addition to ensureEFIBootEntry, never
// instead of it, keeps the labelled NVRAM entry as the normal path (it
// survives other machines' own entries coming and going, unlike the
// single shared removable path) while this covers the case that entry
// does not survive.
func (e *Executor) ensureRemovableFallback(ctx context.Context, ip agentapi.ImageDeployPlan, esp agentapi.PlanPartition) error {
	mountpoint, err := os.MkdirTemp("", "kezio-esp-")
	if err != nil {
		return fmt.Errorf("image %s: creating ESP mountpoint: %w", ip.ImageRef.Name, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), hookCleanupTimeout)
		e.unmountAll(cleanupCtx, []string{mountpoint})
		cancel()
		if err := os.Remove(mountpoint); err != nil && !os.IsNotExist(err) {
			e.log("removing ESP mountpoint %s: %v", mountpoint, err)
		}
	}()

	if _, err := e.Runner.Run(ctx, nil, "mount", esp.Device, mountpoint); err != nil {
		return fmt.Errorf("image %s: mounting ESP %s at %s: %w", ip.ImageRef.Name, esp.Device, mountpoint, err)
	}

	installed, source, err := ensureRemovableFallbackFile(mountpoint)
	if err != nil {
		return fmt.Errorf("image %s: ensuring removable fallback bootloader: %w", ip.ImageRef.Name, err)
	}
	if installed {
		e.log("installed removable fallback bootloader on %s from %s", esp.Device, source)
	}
	return nil
}

// ensureRemovableFallbackFile is ensureRemovableFallback's pure part:
// given root (an already-mounted ESP), it reports whether
// EFI/BOOT/BOOTX64.EFI is already present, installing a copy from the
// first espBootloaderCandidates match under any other EFI/<name>/
// directory when it is not. source names whichever file it copied from
// ("" when nothing needed copying). Kept separate from
// ensureRemovableFallback so this decision is unit-testable against a
// plain temp directory, with no mount or Runner involved.
func ensureRemovableFallbackFile(root string) (installed bool, source string, err error) {
	fallback := filepath.Join(root, filepath.FromSlash(removableFallbackDir), removableFallbackName)
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

// findESPBootloader looks for espBootloaderCandidates under every
// EFI/<name>/ directory of root other than EFI/BOOT itself (that is the
// fallback's destination, never a source), returning the first match in
// directory-then-candidate order. A deployed image ordinarily carries
// exactly one EFI/<distro>/ directory, so "first match" is not an
// arbitrary choice in practice.
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
		for _, candidate := range espBootloaderCandidates {
			path := filepath.Join(efiDir, entry.Name(), candidate)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no bootloader (%s) found under any EFI/<name>/ directory in %s", strings.Join(espBootloaderCandidates, ", "), efiDir)
}

// copyFile copies src to dst, creating dst world-readable - firmware
// does not check the execute bit, and everything already on an ESP is
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

// growLastPartition grows ip's last partition and, when it has a file
// system finalize knows how to resize, that file system too - filling
// whatever extra space ip.Disk has beyond what ip.SfdiskJSON's layout
// itself asks for. It is a no-op unless ip.GrowLastPartition is true or
// force is true; see ip.GrowLastPartition's field comment for why every
// plan the controller builds today leaves it false, making the
// force=false path unreachable in production until a later work item
// wires a spec field to it. force is set by the "growLastPartition"
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
