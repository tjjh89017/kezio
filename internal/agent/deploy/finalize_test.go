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
	"os"
	"path/filepath"
	"strings"
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// --- ensureRemovableFallbackFile / findESPBootloader: pure, real temp dirs ---

func TestEnsureRemovableFallbackFile_AlreadyPresentDoesNothing(t *testing.T) {
	root := t.TempDir()
	fallback := filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI")
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, source, err := ensureRemovableFallbackFile(root)
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile: %v", err)
	}
	if installed {
		t.Fatalf("installed = true, want false (fallback already present)")
	}
	if source != "" {
		t.Fatalf("source = %q, want empty", source)
	}

	got, err := os.ReadFile(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatalf("existing fallback content was overwritten: got %q", got)
	}
}

func TestEnsureRemovableFallbackFile_InstallsFromShimWhenMissing(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "EFI", "ubuntu", "shimx64.efi")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("shim contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, source, err := ensureRemovableFallbackFile(root)
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile: %v", err)
	}
	if !installed {
		t.Fatal("installed = false, want true (fallback was missing)")
	}
	if source != shim {
		t.Fatalf("source = %q, want %q", source, shim)
	}

	fallback := filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI")
	got, err := os.ReadFile(fallback)
	if err != nil {
		t.Fatalf("reading installed fallback: %v", err)
	}
	if string(got) != "shim contents" {
		t.Fatalf("installed fallback content = %q, want the shim's own content", got)
	}
}

func TestEnsureRemovableFallbackFile_FallsBackToGrubWhenNoShim(t *testing.T) {
	root := t.TempDir()
	grub := filepath.Join(root, "EFI", "debian", "grubx64.efi")
	if err := os.MkdirAll(filepath.Dir(grub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grub, []byte("grub contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, source, err := ensureRemovableFallbackFile(root)
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile: %v", err)
	}
	if !installed || source != grub {
		t.Fatalf("installed=%v source=%q, want true, %q", installed, source, grub)
	}
}

func TestEnsureRemovableFallbackFile_ErrorsWhenNoBootloaderAnywhere(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "EFI", "ubuntu"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ensureRemovableFallbackFile(root); err == nil {
		t.Fatal("ensureRemovableFallbackFile: want an error when no candidate bootloader exists anywhere on the ESP")
	}
}

func TestFindESPBootloader_NeverReadsUnderEFIBootItself(t *testing.T) {
	root := t.TempDir()
	// A shimx64.efi sitting under EFI/BOOT is the fallback's own
	// destination directory, never a source to copy from - if
	// findESPBootloader picked it up as a candidate, a corrupt or
	// half-written BOOTX64.EFI could get "copied" onto itself.
	boot := filepath.Join(root, "EFI", "BOOT", "shimx64.efi")
	if err := os.MkdirAll(filepath.Dir(boot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boot, []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := findESPBootloader(root); err == nil {
		t.Fatal("findESPBootloader found a candidate under EFI/BOOT itself; it must only look under other EFI/<name>/ directories")
	}
}

// --- ensureRemovableFallback: Executor-level wiring via the fake Runner ---

// finalizeFakeRunner is a minimal Runner for ensureRemovableFallback tests:
// it records every call and, on a "mount" call, invokes onMount (if set)
// with the real temp directory ensureRemovableFallback just asked it to
// mount at (the call's last argument), letting a test seed that
// directory's content before the function under test reads it back -
// standing in for whatever a real "mount -t vfat /dev/... /tmp/..." would
// have made visible.
type finalizeFakeRunner struct {
	calls   []string
	onMount func(target string)
	// failOn, when non-nil, is consulted for every call against the call's
	// full rendered line ("name arg0 arg1 ...") as a prefix match - the
	// mountpoint arg, a fresh temp dir each run, is never part of a key -
	// and its error returned instead of the call succeeding.
	failOn map[string]error
}

func (f *finalizeFakeRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	call := fmt.Sprintf("%s %s", name, strings.Join(args, " "))
	f.calls = append(f.calls, call)
	for key, err := range f.failOn {
		if strings.HasPrefix(call, key) {
			return nil, err
		}
	}
	if name == "mount" && len(args) >= 2 && f.onMount != nil {
		f.onMount(args[len(args)-1])
	}
	return nil, nil
}

func TestEnsureRemovableFallback_MountsESPInstallsAndUnmounts(t *testing.T) {
	var mountedTarget string
	runner := &finalizeFakeRunner{
		onMount: func(target string) {
			mountedTarget = target
			shim := filepath.Join(target, "EFI", "ubuntu", "shimx64.efi")
			if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(shim, []byte("shim"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	var logged strings.Builder
	e := &Executor{Runner: runner, Logf: func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	installed, source, err := e.ensureRemovableFallback(context.Background(), ip, esp)
	if err != nil {
		t.Fatalf("ensureRemovableFallback: %v", err)
	}
	if !installed {
		t.Fatal("installed = false, want true - the fallback was missing and a candidate was found")
	}
	if !strings.HasSuffix(source, filepath.Join("EFI", "ubuntu", "shimx64.efi")) {
		t.Fatalf("source = %q, want it to name the shim it copied from", source)
	}

	if mountedTarget == "" {
		t.Fatal("mount was never called")
	}
	if !strings.Contains(logged.String(), "installed removable fallback bootloader") {
		t.Fatalf("log output = %q, want it to mention the fallback was installed", logged.String())
	}
	if !strings.Contains(logged.String(), "shimx64.efi") {
		t.Fatalf("log output = %q, want it to name the shim it copied from", logged.String())
	}

	var mountCalls, umountCalls int
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "mount ") {
			mountCalls++
		}
		if strings.HasPrefix(c, "umount ") {
			umountCalls++
		}
	}
	if mountCalls != 1 || umountCalls != 1 {
		t.Fatalf("mount calls = %d, umount calls = %d, want exactly 1 each", mountCalls, umountCalls)
	}
	if runner.calls[0] != "mount -t vfat /dev/nvme0n1p1 "+mountedTarget {
		t.Fatalf("first call = %q, want an explicit-fstype mount of the ESP device", runner.calls[0])
	}
}

func TestEnsureRemovableFallback_AlreadyPresentInstallsNothing(t *testing.T) {
	runner := &finalizeFakeRunner{
		onMount: func(target string) {
			fallback := filepath.Join(target, "EFI", "BOOT")
			if err := os.MkdirAll(fallback, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fallback, "BOOTX64.EFI"), []byte("already there"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	var logged strings.Builder
	e := &Executor{Runner: runner, Logf: func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	installed, source, err := e.ensureRemovableFallback(context.Background(), ip, esp)
	if err != nil {
		t.Fatalf("ensureRemovableFallback: %v", err)
	}
	if installed {
		t.Fatal("installed = true, want false - the fallback was already present")
	}
	if source != "" {
		t.Fatalf("source = %q, want empty - nothing was copied", source)
	}
	if strings.Contains(logged.String(), "installed removable fallback bootloader") {
		t.Fatalf("log output = %q, want no install log line - the fallback was already present", logged.String())
	}
}

func TestEnsureRemovableFallback_NoBootloaderAnywhereFailsAndStillUnmounts(t *testing.T) {
	runner := &finalizeFakeRunner{
		onMount: func(target string) {
			if err := os.MkdirAll(filepath.Join(target, "EFI", "ubuntu"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	e := &Executor{Runner: runner}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	if _, _, err := e.ensureRemovableFallback(context.Background(), ip, esp); err == nil {
		t.Fatal("ensureRemovableFallback: want an error when the ESP carries no candidate bootloader")
	}

	var umountCalls int
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "umount ") {
			umountCalls++
		}
	}
	if umountCalls != 1 {
		t.Fatalf("umount calls = %d, want 1 - a failed install must still unmount the ESP", umountCalls)
	}
}

func TestEnsureRemovableFallback_MountFailurePropagatesAndSkipsUnmount(t *testing.T) {
	runner := &finalizeFakeRunner{
		failOn: map[string]error{"mount -t vfat /dev/nvme0n1p1": fmt.Errorf("no such device")},
	}
	e := &Executor{Runner: runner}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	_, _, err := e.ensureRemovableFallback(context.Background(), ip, esp)
	if err == nil || !strings.Contains(err.Error(), "mounting ESP") {
		t.Fatalf("ensureRemovableFallback error = %v, want it to name the mount failure", err)
	}
}
