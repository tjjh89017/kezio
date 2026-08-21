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

func TestEnsureRemovableFallbackFile(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "debian"))
	mustWriteFile(t, filepath.Join(root, "EFI", "debian", "shimx64.efi"), "shim-bytes")

	installed, source, err := ensureRemovableFallbackFile(root, "EFI/BOOT/BOOTX64.EFI")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile: %v", err)
	}
	if !installed {
		t.Fatal("ensureRemovableFallbackFile: want installed=true when the fallback path is missing")
	}
	if !strings.HasSuffix(source, "shimx64.efi") {
		t.Errorf("source = %q, want it to name shimx64.efi", source)
	}

	// A second call finds the file already in place and does nothing.
	installed, _, err = ensureRemovableFallbackFile(root, "EFI/BOOT/BOOTX64.EFI")
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile (second call): %v", err)
	}
	if installed {
		t.Error("ensureRemovableFallbackFile: want installed=false once the fallback path already exists")
	}
}

func TestEnsureRemovableFallbackFile_NoCandidateIsAnError(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "EFI", "debian"))
	if _, _, err := ensureRemovableFallbackFile(root, "EFI/BOOT/BOOTX64.EFI"); err == nil {
		t.Fatal("ensureRemovableFallbackFile: want an error when no candidate bootloader exists")
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
