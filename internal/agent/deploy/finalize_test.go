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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errMountFailed = errors.New("mount: no such device")

func TestEfiRemovableLoaderPathForArch(t *testing.T) {
	cases := []struct {
		arch    string
		want    string
		wantErr bool
	}{
		{"amd64", `\EFI\BOOT\BOOTX64.EFI`, false},
		{"arm64", `\EFI\BOOT\BOOTAA64.EFI`, false},
		{"riscv64", "", true},
	}
	for _, c := range cases {
		path, err := efiRemovableLoaderPathForArch(c.arch)
		if c.wantErr {
			if err == nil {
				t.Errorf("efiRemovableLoaderPathForArch(%q) = %q, nil; want an error", c.arch, path)
			}
			continue
		}
		if err != nil {
			t.Errorf("efiRemovableLoaderPathForArch(%q): %v", c.arch, err)
		}
		if path != c.want {
			t.Errorf("efiRemovableLoaderPathForArch(%q) = %q, want %q", c.arch, path, c.want)
		}
	}
}

func TestEfibootLabel_PrefersMachineNameOverRunName(t *testing.T) {
	plan := basicPlan()
	if got, want := efibootLabel(plan), "kezio:node-01"; got != want {
		t.Errorf("efibootLabel = %q, want %q", got, want)
	}
	plan.MachineName = ""
	if got, want := efibootLabel(plan), "kezio:"+plan.RunName; got != want {
		t.Errorf("efibootLabel with no MachineName = %q, want %q", got, want)
	}
}

func TestParseEFIBootEntries(t *testing.T) {
	listing := "BootCurrent: 0001\n" +
		"BootOrder: 0001,0002,0003\n" +
		"Boot0001* Linux Boot Manager\n" +
		"Boot0002* kezio:node-01\n" +
		"Boot0003  kezio:node-01\n" // no "*": not the active default, still matches by label

	got := parseEFIBootEntries(listing, "kezio:node-01")
	want := []string{"0002", "0003"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseEFIBootEntries = %v, want %v", got, want)
	}
}

func TestRunBuiltinEfibootmgr_RemovesPriorEntryThenCreates(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["efibootmgr "] = []byte("Boot0007* kezio:node-01\n")
	e := &Executor{Runner: runner}

	if err := runBuiltinEfibootmgr(context.Background(), e, basicPlan(), map[string]string{"part": "1"}); err != nil {
		t.Fatalf("runBuiltinEfibootmgr: %v", err)
	}

	calls := runner.commandNames()
	wantDelete := "efibootmgr --bootnum 0007 --delete-bootnum"
	found := false
	for _, c := range calls {
		if c == wantDelete {
			found = true
		}
	}
	if !found {
		t.Errorf("did not delete the prior same-label entry; calls: %v", calls)
	}
	lastCall := calls[len(calls)-1]
	if !strings.HasPrefix(lastCall, "efibootmgr --create --disk /dev/sda --part 1 --loader ") {
		t.Errorf("did not create the new boot entry as the final call; last call: %q", lastCall)
	}
	if !strings.Contains(lastCall, "--label kezio:node-01") {
		t.Errorf("created entry missing the machine's label; call: %q", lastCall)
	}
}

func TestRunBuiltinEfibootmgr_RequiresPartitionParam(t *testing.T) {
	e := &Executor{Runner: newFakeRunner()}
	if err := runBuiltinEfibootmgr(context.Background(), e, basicPlan(), nil); err == nil {
		t.Fatal(`runBuiltinEfibootmgr: want an error when params["part"] is missing`)
	}
}

func TestFinalizeDisk_DefaultsToTargetDisk(t *testing.T) {
	plan := basicPlan()
	if got := finalizeDisk(plan, nil); got != plan.TargetDisk {
		t.Errorf("finalizeDisk(nil) = %q, want plan.TargetDisk %q", got, plan.TargetDisk)
	}
	if got := finalizeDisk(plan, map[string]string{"disk": "/dev/sdb"}); got != "/dev/sdb" {
		t.Errorf("finalizeDisk override = %q, want /dev/sdb", got)
	}
}

func TestRunBuiltinGrowLastPartition_DefaultsToLastOSSlot(t *testing.T) {
	runner := newFakeRunner()
	e := &Executor{Runner: runner}

	if err := runBuiltinGrowLastPartition(context.Background(), e, basicPlan(), nil); err != nil {
		t.Fatalf("runBuiltinGrowLastPartition: %v", err)
	}
	calls := runner.commandNames()
	if calls[0] != "growpart /dev/sda 3" {
		t.Errorf("growpart call = %q, want it to target the last slot (number 3); calls: %v", calls[0], calls)
	}
}

func TestRunBuiltinGrowLastPartition_ExplicitPartitionAndFsType(t *testing.T) {
	runner := newFakeRunner()
	e := &Executor{Runner: runner}

	params := map[string]string{"partition": "1", "fsType": "ext4"}
	if err := runBuiltinGrowLastPartition(context.Background(), e, basicPlan(), params); err != nil {
		t.Fatalf("runBuiltinGrowLastPartition: %v", err)
	}
	calls := runner.commandNames()
	if calls[0] != "growpart /dev/sda 1" {
		t.Errorf("growpart call = %q, want partition 1; calls: %v", calls[0], calls)
	}
	if calls[1] != "resize2fs /dev/sda1" {
		t.Errorf("resize call = %q, want resize2fs /dev/sda1; calls: %v", calls[1], calls)
	}
}

func TestFsResizeCommand(t *testing.T) {
	cases := []struct {
		fsType string
		want   []string
		ok     bool
	}{
		{"ext4", []string{"resize2fs", "/dev/sda1"}, true},
		{"xfs", []string{"xfs_growfs", "/dev/sda1"}, true},
		{"btrfs", []string{"btrfs", "filesystem", "resize", "max", "/dev/sda1"}, true},
		{"vfat", nil, false},
		{"", nil, false},
	}
	for _, c := range cases {
		cmd, ok := fsResizeCommand(c.fsType, "/dev/sda1")
		if ok != c.ok {
			t.Errorf("fsResizeCommand(%q): ok = %v, want %v", c.fsType, ok, c.ok)
		}
		if ok && strings.Join(cmd, " ") != strings.Join(c.want, " ") {
			t.Errorf("fsResizeCommand(%q) = %v, want %v", c.fsType, cmd, c.want)
		}
	}
}

func TestRunBuiltinGrowLastPartition_UnknownFsTypeSkipsResize(t *testing.T) {
	runner := newFakeRunner()
	e := &Executor{Runner: runner}

	if err := runBuiltinGrowLastPartition(context.Background(), e, basicPlan(), nil); err != nil {
		t.Fatalf("runBuiltinGrowLastPartition: %v", err)
	}
	// basicPlan's last slot is a torrent slot: no fsType is known on the
	// wire, so only growpart should have run.
	if len(runner.calls) != 1 {
		t.Errorf("calls = %v, want exactly one growpart call and no resize", runner.commandNames())
	}
}

func TestRunBuiltinGrowLastPartition_OtherDiskRequiresExplicitPartition(t *testing.T) {
	e := &Executor{Runner: newFakeRunner()}
	plan := basicPlan()
	params := map[string]string{"disk": "/dev/sdb"}
	if err := runBuiltinGrowLastPartition(context.Background(), e, plan, params); err == nil {
		t.Fatal(`runBuiltinGrowLastPartition: want an error when "disk" overrides away from TargetDisk with no explicit "partition"`)
	}
}

func TestRunBuiltinInstallRemovableFallback_RequiresPartitionParam(t *testing.T) {
	e := &Executor{Runner: newFakeRunner()}
	if err := runBuiltinInstallRemovableFallback(context.Background(), e, basicPlan(), nil); err == nil {
		t.Fatal(`runBuiltinInstallRemovableFallback: want an error when params["part"] is missing`)
	}
}

func TestInstallRemovableFallback_MountFailurePropagates(t *testing.T) {
	runner := newFakeRunner()
	runner.errPrefixes["mount -t vfat /dev/sda1 "] = errMountFailed
	e := &Executor{Runner: runner}

	err := e.installRemovableFallback(context.Background(), "/dev/sda", "/dev/sda1")
	if err == nil || !strings.Contains(err.Error(), "mounting ESP") {
		t.Fatalf("installRemovableFallback error = %v, want a mount failure wrapped with context", err)
	}
}

func TestEspLoaderNames(t *testing.T) {
	cases := []struct {
		arch      string
		wantShim  string
		wantGrub  string
		wantError bool
	}{
		{arch: "amd64", wantShim: "shimx64.efi", wantGrub: "grubx64.efi"},
		{arch: "arm64", wantShim: "shimaa64.efi", wantGrub: "grubaa64.efi"},
		{arch: "riscv64", wantError: true},
	}
	for _, c := range cases {
		shim, grub, err := espLoaderNames(c.arch)
		if c.wantError {
			if err == nil {
				t.Errorf("espLoaderNames(%q) = %q, %q, nil; want an error", c.arch, shim, grub)
			}
			continue
		}
		if err != nil {
			t.Errorf("espLoaderNames(%q): %v", c.arch, err)
			continue
		}
		if shim != c.wantShim || grub != c.wantGrub {
			t.Errorf("espLoaderNames(%q) = %q, %q; want %q, %q", c.arch, shim, grub, c.wantShim, c.wantGrub)
		}
	}
}

// TestEnsureRemovableFallbackChain_InstallsShimAndItsSecondStage: a shim
// alone at the fallback path cannot boot anything. shim only ever opens
// its second stage from its own directory, so both files must land in
// EFI/BOOT together.
func TestEnsureRemovableFallbackChain_InstallsShimAndItsSecondStage(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "ubuntu"))
	mustWriteFile(t, filepath.Join(root, "EFI", "ubuntu", "shimx64.efi"), "shim-bytes")
	mustWriteFile(t, filepath.Join(root, "EFI", "ubuntu", "grubx64.efi"), "grub-bytes")

	installed, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackChain: %v", err)
	}
	wantInstalled := []string{"EFI/BOOT/BOOTX64.EFI", "EFI/BOOT/grubx64.efi"}
	if strings.Join(installed, ",") != strings.Join(wantInstalled, ",") {
		t.Errorf("installed = %v, want %v", installed, wantInstalled)
	}
	mustHaveContent(t, filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), "shim-bytes")
	mustHaveContent(t, filepath.Join(root, "EFI", "BOOT", "grubx64.efi"), "grub-bytes")
}

// TestEnsureRemovableFallbackChain_CompletesAnImageShippedShim is the case
// every Ubuntu and Debian cloud image lands in: the ESP already carries a
// shim copy at the fallback path, but no second stage beside it, so the
// firmware (or a chainloader) starts a shim that immediately fails to open
// EFI/BOOT/grubx64.efi. Finding the fallback path occupied must not end
// the step.
func TestEnsureRemovableFallbackChain_CompletesAnImageShippedShim(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "BOOT"))
	mustWriteFile(t, filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), "shim-bytes")
	mustMkdirAll(t, filepath.Join(root, "EFI", "ubuntu"))
	mustWriteFile(t, filepath.Join(root, "EFI", "ubuntu", "grubx64.efi"), "grub-bytes")

	installed, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackChain: %v", err)
	}
	if strings.Join(installed, ",") != "EFI/BOOT/grubx64.efi" {
		t.Errorf("installed = %v, want only the second stage", installed)
	}
	mustHaveContent(t, filepath.Join(root, "EFI", "BOOT", "grubx64.efi"), "grub-bytes")
}

// TestEnsureRemovableFallbackChain_AlreadyCompleteChangesNothing keeps the
// step a no-op on an ESP that already boots.
func TestEnsureRemovableFallbackChain_AlreadyCompleteChangesNothing(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "BOOT"))
	mustWriteFile(t, filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), "shim-bytes")
	mustWriteFile(t, filepath.Join(root, "EFI", "BOOT", "grubx64.efi"), "grub-bytes")

	installed, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackChain: %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("installed = %v, want nothing on an already complete chain", installed)
	}
}

// TestEnsureRemovableFallbackChain_GrubFallbackNeedsNoSecondStage: a GRUB
// copied to the fallback path loads its own config, so a missing
// grubx64.efi beside it is not a defect.
func TestEnsureRemovableFallbackChain_GrubFallbackNeedsNoSecondStage(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "ubuntu"))
	mustWriteFile(t, filepath.Join(root, "EFI", "ubuntu", "grubx64.efi"), "grub-bytes")

	installed, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackChain: %v", err)
	}
	if strings.Join(installed, ",") != "EFI/BOOT/BOOTX64.EFI" {
		t.Errorf("installed = %v, want only the fallback loader", installed)
	}
}

// TestEnsureRemovableFallbackChain_ShimWithoutGrubIsAnError: installing a
// shim this step knows has no second stage anywhere on the ESP would leave
// a machine that fails in firmware minutes later with no explanation. Say
// so while the deploy can still report it.
func TestEnsureRemovableFallbackChain_ShimWithoutGrubIsAnError(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "ubuntu"))
	mustWriteFile(t, filepath.Join(root, "EFI", "ubuntu", "shimx64.efi"), "shim-bytes")

	_, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi")
	if err == nil {
		t.Fatal("ensureRemovableFallbackChain: want an error when the installed shim has no second stage to load")
	}
	if !strings.Contains(err.Error(), "grubx64.efi") {
		t.Errorf("error = %v, want it to name the missing second stage", err)
	}
}

func TestEnsureRemovableFallbackChain_NoCandidateIsAnError(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "ubuntu"))
	if _, err := ensureRemovableFallbackChain(root, "EFI/BOOT/BOOTX64.EFI", "shimx64.efi", "grubx64.efi"); err == nil {
		t.Fatal("ensureRemovableFallbackChain: want an error when no candidate bootloader exists")
	}
}

// TestEspBootDirListing reports what the firmware will actually find at
// the fallback path, so a deploy record states it instead of leaving a
// later boot failure with nothing to read.
func TestEspBootDirListing(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "BOOT"))
	mustWriteFile(t, filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"), "shim-bytes")
	mustWriteFile(t, filepath.Join(root, "EFI", "BOOT", "grubx64.efi"), "grub-bytes")

	got, err := espBootDirListing(root, "EFI/BOOT")
	if err != nil {
		t.Fatalf("espBootDirListing: %v", err)
	}
	if strings.Join(got, ",") != "BOOTX64.EFI,grubx64.efi" {
		t.Errorf("espBootDirListing = %v, want the EFI/BOOT file names in order", got)
	}
}

func mustHaveContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
