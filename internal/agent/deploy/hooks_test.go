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
	"sync"
	"testing"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// bootx64RelPath is the x86_64 ESP-relative fallback path
// installRemovableFallback and ensureRemovableFallbackFile tests exercise
// against - the POSIX-style relative form of
// efiRemovableLoaderPath["amd64"].
const bootx64RelPath = "EFI/BOOT/BOOTX64.EFI"

// hookFakeRunner is a Runner tailored for hooks_test.go: it records every
// call (name + args, in order - never stdin, since a chrootScript/script
// step's content is written to a file and never passed as an arg or
// stdin the fake could accidentally echo into a test failure message),
// can fail or block-until-context-done any call by command name, and
// otherwise succeeds with empty output - the same "succeed unless told
// otherwise" default fakeRunner (deploy_test.go) uses.
type hookFakeRunner struct {
	mu        sync.Mutex
	calls     []string
	errByName map[string]error
	blockName string

	// errByCall, when set, is consulted before errByName so a test can
	// fail a call based on its args - not just its command name - e.g.
	// distinguishing a plain "umount x" from "umount --lazy x". Returning
	// nil falls through to errByName.
	errByCall func(name string, args []string) error
}

func newHookFakeRunner() *hookFakeRunner {
	return &hookFakeRunner{errByName: map[string]error{}}
}

func (f *hookFakeRunner) Run(ctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s", name, strings.Join(args, " ")))
	f.mu.Unlock()

	if name == f.blockName {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.errByCall != nil {
		if err := f.errByCall(name, args); err != nil {
			return nil, err
		}
	}
	if err, ok := f.errByName[name]; ok {
		return nil, err
	}
	return nil, nil
}

func (f *hookFakeRunner) commandNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = strings.SplitN(c, " ", 2)[0]
	}
	return names
}

// planWithOneDataPartition builds a minimal DeployPlan whose OS image has
// exactly one data-role partition - the layout runChrootScriptStep's
// rootPartition convention expects - at device rootDevice.
func planWithOneDataPartition(rootDevice string, hooks []agentapi.ResolvedHook) *agentapi.DeployPlan {
	return &agentapi.DeployPlan{
		MachineName: "node-01",
		OS: &agentapi.ImageDeployPlan{
			ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
			Disk:     "/dev/nvme0n1",
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: rootDevice, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
			},
		},
		Hooks: hooks,
	}
}

func TestRunHooks_ScriptStepRunsTemplatedContentInLiveOS(t *testing.T) {
	runner := newHookFakeRunner()
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		Hooks: []agentapi.ResolvedHook{{
			Name: "notify",
			Steps: []agentapi.ResolvedHookStep{{
				Type:           agentapi.HookStepTypeScript,
				Content:        "echo hello\n",
				TimeoutSeconds: 5,
			}},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	names := runner.commandNames()
	if len(names) != 1 || names[0] != "/bin/sh" {
		t.Fatalf("commands = %v, want exactly one /bin/sh invocation (script runs in the live OS, nothing mounted)", names)
	}
	// No mount/chroot commands: a "script" step never touches the target.
	for _, n := range names {
		if n == "mount" || n == "chroot" || n == "umount" {
			t.Fatalf("script step ran %q; it must never mount or chroot", n)
		}
	}
}

func TestRunHooks_ScriptStepNonZeroExitAbortsDeploy(t *testing.T) {
	runner := newHookFakeRunner()
	runner.errByName["/bin/sh"] = fmt.Errorf("exit status 1")
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		Hooks: []agentapi.ResolvedHook{{
			Name:  "fails",
			Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeScript, Content: "false\n", TimeoutSeconds: 5}},
		}},
	}

	err := e.runHooks(context.Background(), plan)
	if err == nil {
		t.Fatal("runHooks returned no error for a script step that exited non-zero")
	}
}

func TestRunHooks_ChrootScriptMountsChrootsRunsUnmountsInOrder(t *testing.T) {
	runner := newHookFakeRunner()
	e := &Executor{Runner: runner}

	plan := planWithOneDataPartition("/dev/nvme0n1p1", []agentapi.ResolvedHook{{
		Name: "regen-initramfs",
		Steps: []agentapi.ResolvedHookStep{{
			Type:           agentapi.HookStepTypeChrootScript,
			Content:        "update-initramfs -u\n",
			TimeoutSeconds: 30,
		}},
	}})

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	names := runner.commandNames()
	// Expected shape: mount (root), mount --bind (proc, sys, dev), chroot,
	// then umount x4 in reverse order (dev, sys, proc, root).
	if len(names) != 9 {
		t.Fatalf("commands = %v, want 9 (1 root mount + 3 bind mounts + 1 chroot + 4 unmounts)", names)
	}
	if names[0] != "mount" {
		t.Fatalf("commands[0] = %q, want the root mount first", names[0])
	}
	for i := 1; i <= 3; i++ {
		if names[i] != "mount" {
			t.Fatalf("commands[%d] = %q, want a bind mount", i, names[i])
		}
	}
	if names[4] != "chroot" {
		t.Fatalf("commands[4] = %q, want chroot to run after every mount", names[4])
	}
	for i := 5; i <= 8; i++ {
		if names[i] != "umount" {
			t.Fatalf("commands[%d] = %q, want unmount after chroot returns", i, names[i])
		}
	}

	// Bind mount order is proc, sys, dev (chrootBindMounts); unmount must
	// undo it in the exact reverse order: dev, sys, proc, then the root
	// mount last.
	calls := runner.calls
	if !strings.Contains(calls[1], "/proc") || !strings.Contains(calls[2], "/sys") || !strings.Contains(calls[3], "/dev") {
		t.Fatalf("bind mount order = %v, want proc, sys, dev", calls[1:4])
	}
	if !strings.Contains(calls[5], "/dev") || !strings.Contains(calls[6], "/sys") || !strings.Contains(calls[7], "/proc") {
		t.Fatalf("unmount order = %v, want dev, sys, proc (reverse of mount order)", calls[5:8])
	}
	rootMountArgs := strings.Fields(calls[0])
	rootMountpoint := rootMountArgs[len(rootMountArgs)-1]
	if calls[8] != "umount "+rootMountpoint {
		t.Fatalf("last unmount = %q, want %q (the same mountpoint the root mount used)", calls[8], "umount "+rootMountpoint)
	}

	// The root mount must name its file system type explicitly (from
	// PlanPartition.FSType) rather than leaving it to mount's own
	// autoprobe - the same defect class that made the ESP mount in
	// finalize.go pick the wrong driver.
	if calls[0] != fmt.Sprintf("mount -t ext4 /dev/nvme0n1p1 %s", rootMountpoint) {
		t.Fatalf("root mount = %q, want an explicit -t ext4", calls[0])
	}
}

func TestRunHooks_ChrootScriptStepTimeoutAbortsDeploy(t *testing.T) {
	runner := newHookFakeRunner()
	runner.blockName = "chroot" // never returns on its own; only ctx cancellation ends it
	e := &Executor{Runner: runner}

	plan := planWithOneDataPartition("/dev/nvme0n1p1", []agentapi.ResolvedHook{{
		Name: "hangs",
		Steps: []agentapi.ResolvedHookStep{{
			Type:           agentapi.HookStepTypeChrootScript,
			Content:        "sleep infinity\n",
			TimeoutSeconds: 1,
		}},
	}})

	start := time.Now()
	err := e.runHooks(context.Background(), plan)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runHooks returned no error for a step that exceeded its timeout")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runHooks took %s to return after a 1s timeout; the step's context deadline should have killed it promptly", elapsed)
	}

	// Even though chroot itself hung, cleanup must still have run: the
	// root mount and every bind mount must each have a matching unmount
	// call (mount cleanup is never skipped on failure/timeout).
	names := runner.commandNames()
	mounts, umounts := 0, 0
	for _, n := range names {
		switch n {
		case "mount":
			mounts++
		case "umount":
			umounts++
		}
	}
	if mounts == 0 {
		t.Fatal("no mount calls recorded; test setup is wrong")
	}
	if umounts != mounts {
		t.Fatalf("umount calls = %d, want %d (one per mount - a timed-out chrootScript step must not leave the target mounted)", umounts, mounts)
	}
}

func TestRunHooks_ChrootScriptStepFailureStillUnmountsEverything(t *testing.T) {
	runner := newHookFakeRunner()
	runner.errByName["chroot"] = fmt.Errorf("script exited 1")
	e := &Executor{Runner: runner}

	plan := planWithOneDataPartition("/dev/nvme0n1p1", []agentapi.ResolvedHook{{
		Name:  "fails-inside-chroot",
		Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeChrootScript, Content: "false\n", TimeoutSeconds: 5}},
	}})

	err := e.runHooks(context.Background(), plan)
	if err == nil {
		t.Fatal("runHooks returned no error for a chrootScript step whose script exited non-zero")
	}

	names := runner.commandNames()
	mounts, umounts := 0, 0
	for _, n := range names {
		switch n {
		case "mount":
			mounts++
		case "umount":
			umounts++
		}
	}
	if mounts != 4 {
		t.Fatalf("mount calls = %d, want 4 (root + 3 binds)", mounts)
	}
	if umounts != 4 {
		t.Fatalf("umount calls = %d, want 4 - cleanup must unmount everything even though the chrootScript step itself failed", umounts)
	}
}

func TestRunHooks_SecretSourcedContentNeverLogged(t *testing.T) {
	runner := newHookFakeRunner()
	var logged strings.Builder
	e := &Executor{
		Runner: runner,
		Logf:   func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
	}

	const secretMarker = "S3CR3T-TOKEN-DO-NOT-LOG"
	plan := &agentapi.DeployPlan{
		Hooks: []agentapi.ResolvedHook{{
			Name: "join-monitoring",
			Steps: []agentapi.ResolvedHookStep{{
				Type:           agentapi.HookStepTypeScript,
				Content:        "curl -H 'token: " + secretMarker + "' https://monitor.example/join\n",
				FromSecret:     true,
				TimeoutSeconds: 5,
			}},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	if strings.Contains(logged.String(), secretMarker) {
		t.Fatalf("log output contains secret-sourced script content: %q", logged.String())
	}
}

func TestRunHooks_UnknownBuiltinIsAnError(t *testing.T) {
	runner := newHookFakeRunner()
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		Hooks: []agentapi.ResolvedHook{{
			Name:  "bogus",
			Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeBuiltin, Builtin: "does-not-exist", TimeoutSeconds: 5}},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err == nil {
		t.Fatal("runHooks returned no error for an unknown builtin step")
	}
}

func TestRunHooks_MkswapBuiltinRunsMkswapOnEverySwapPartition(t *testing.T) {
	runner := newHookFakeRunner()
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		MachineName: "node-01",
		OS: &agentapi.ImageDeployPlan{
			ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
			Disk:     "/dev/nvme0n1",
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleSwap, SwapUUID: "11111111-1111-1111-1111-111111111111"},
			},
		},
		Hooks: []agentapi.ResolvedHook{{
			Name:  "kezio-default-finalize",
			Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeBuiltin, Builtin: keziov1alpha1.BuiltinStepMkswap, TimeoutSeconds: 5}},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	found := false
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "mkswap ") && strings.Contains(c, "11111111-1111-1111-1111-111111111111") {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls = %v, want an mkswap call carrying the swap partition's UUID", runner.calls)
	}
}

func TestRunHooks_UnmountRetriesWithLazyOnPlainUmountFailure(t *testing.T) {
	runner := newHookFakeRunner()
	runner.errByCall = func(name string, args []string) error {
		if name != "umount" {
			return nil
		}
		for _, a := range args {
			if a == "--lazy" {
				return nil
			}
		}
		return fmt.Errorf("device is busy")
	}
	e := &Executor{Runner: runner}

	plan := planWithOneDataPartition("/dev/nvme0n1p2", []agentapi.ResolvedHook{{
		Name: "regen-initramfs",
		Steps: []agentapi.ResolvedHookStep{{
			Type:           agentapi.HookStepTypeChrootScript,
			Content:        "update-initramfs -u\n",
			TimeoutSeconds: 30,
		}},
	}})

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v (unmount failures must be logged and continued, never abort the deploy)", err)
	}

	// Every target that got a plain "umount <target>" that failed must
	// also have a follow-up "umount --lazy <target>".
	plainTargets := map[string]bool{}
	lazyTargets := map[string]bool{}
	for _, c := range runner.calls {
		fields := strings.Fields(c)
		if len(fields) < 2 || fields[0] != "umount" {
			continue
		}
		if fields[1] == "--lazy" {
			if len(fields) >= 3 {
				lazyTargets[fields[2]] = true
			}
			continue
		}
		plainTargets[fields[1]] = true
	}
	if len(plainTargets) == 0 {
		t.Fatal("no plain umount calls recorded; test setup is wrong")
	}
	for target := range plainTargets {
		if !lazyTargets[target] {
			t.Fatalf("target %q had a plain umount call but no follow-up umount --lazy call; calls = %v", target, runner.calls)
		}
	}
}

func TestRunHooks_UnmountLogsWhenLazyAlsoFails(t *testing.T) {
	runner := newHookFakeRunner()
	runner.errByCall = func(name string, args []string) error {
		if name != "umount" {
			return nil
		}
		return fmt.Errorf("device is busy")
	}
	var logged strings.Builder
	e := &Executor{
		Runner: runner,
		Logf:   func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
	}

	plan := planWithOneDataPartition("/dev/nvme0n1p1", []agentapi.ResolvedHook{{
		Name: "regen-initramfs",
		Steps: []agentapi.ResolvedHookStep{{
			Type:           agentapi.HookStepTypeChrootScript,
			Content:        "update-initramfs -u\n",
			TimeoutSeconds: 30,
		}},
	}})

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v (unmount failures must be logged and continued, never abort the deploy)", err)
	}

	if !strings.Contains(logged.String(), "also failed") {
		t.Fatalf("log output = %q, want it to mention that the lazy unmount also failed", logged.String())
	}
}

func TestRootPartition_ErrorsOnZeroDataPartitions(t *testing.T) {
	ip := agentapi.ImageDeployPlan{
		ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
		Disk:     "/dev/nvme0n1",
		Partitions: []agentapi.PlanPartition{
			{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP},
		},
	}

	_, err := rootPartition(ip)
	if err == nil {
		t.Fatal("rootPartition returned no error for an image plan with no data-role partition")
	}
	if !strings.Contains(err.Error(), "no data-role partition") {
		t.Fatalf("error = %q, want it to mention \"no data-role partition\"", err.Error())
	}
}

func TestRootPartition_ErrorsOnMultipleDataPartitions(t *testing.T) {
	ip := agentapi.ImageDeployPlan{
		ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
		Disk:     "/dev/nvme0n1",
		Partitions: []agentapi.PlanPartition{
			{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
			{Number: 2, Device: "/dev/nvme0n1p2", Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
		},
	}

	_, err := rootPartition(ip)
	if err == nil {
		t.Fatal("rootPartition returned no error for an image plan with two data-role partitions")
	}
	if !strings.Contains(err.Error(), "2 data-role partitions") {
		t.Fatalf("error = %q, want it to mention \"2 data-role partitions\"", err.Error())
	}
}

func TestRunHooks_StepsRunInOrderAcrossHooks(t *testing.T) {
	runner := newHookFakeRunner()
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		Hooks: []agentapi.ResolvedHook{
			{Name: "first", Steps: []agentapi.ResolvedHookStep{
				{Type: agentapi.HookStepTypeScript, Content: "echo one\n", TimeoutSeconds: 5},
				{Type: agentapi.HookStepTypeScript, Content: "echo two\n", TimeoutSeconds: 5},
			}},
			{Name: "second", Steps: []agentapi.ResolvedHookStep{
				{Type: agentapi.HookStepTypeScript, Content: "echo three\n", TimeoutSeconds: 5},
			}},
		},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	names := runner.commandNames()
	if len(names) != 3 {
		t.Fatalf("commands = %v, want 3 (/bin/sh once per step, across both hooks, in order)", names)
	}
}

// --- ensureRemovableFallbackFile / findESPBootloader: pure, real temp dirs ---

func TestEnsureRemovableFallbackFile_AlreadyPresentDoesNothing(t *testing.T) {
	root := t.TempDir()
	fallback := filepath.Join(root, filepath.FromSlash(bootx64RelPath))
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, source, err := ensureRemovableFallbackFile(root, bootx64RelPath)
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

	installed, source, err := ensureRemovableFallbackFile(root, bootx64RelPath)
	if err != nil {
		t.Fatalf("ensureRemovableFallbackFile: %v", err)
	}
	if !installed {
		t.Fatal("installed = false, want true (fallback was missing)")
	}
	if source != shim {
		t.Fatalf("source = %q, want %q", source, shim)
	}

	fallback := filepath.Join(root, filepath.FromSlash(bootx64RelPath))
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

	installed, source, err := ensureRemovableFallbackFile(root, bootx64RelPath)
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

	if _, _, err := ensureRemovableFallbackFile(root, bootx64RelPath); err == nil {
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

// --- installRemovableFallback: Executor-level wiring via a mount-seeding fake Runner ---

// mountSeedingFakeRunner is a minimal Runner for installRemovableFallback
// tests: it records every call and, on a "mount" call, invokes onMount (if
// set) with the real temp directory installRemovableFallback just asked it
// to mount at (the call's last argument), letting a test seed that
// directory's content before the function under test reads it back -
// standing in for whatever a real "mount -t vfat /dev/... /tmp/..." would
// have made visible.
type mountSeedingFakeRunner struct {
	calls   []string
	onMount func(target string)
	// failOn, when non-nil, is consulted for every call against the call's
	// full rendered line ("name arg0 arg1 ...") as a prefix match - the
	// mountpoint arg, a fresh temp dir each run, is never part of a key -
	// and its error returned instead of the call succeeding.
	failOn map[string]error
}

func (f *mountSeedingFakeRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
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

func TestInstallRemovableFallback_MountsESPInstallsAndUnmounts(t *testing.T) {
	var mountedTarget string
	runner := &mountSeedingFakeRunner{
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

	if err := e.installRemovableFallback(context.Background(), ip, esp); err != nil {
		t.Fatalf("installRemovableFallback: %v", err)
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

func TestInstallRemovableFallback_AlreadyPresentInstallsNothing(t *testing.T) {
	runner := &mountSeedingFakeRunner{
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

	if err := e.installRemovableFallback(context.Background(), ip, esp); err != nil {
		t.Fatalf("installRemovableFallback: %v", err)
	}
	if strings.Contains(logged.String(), "installed removable fallback bootloader") {
		t.Fatalf("log output = %q, want no install log line - the fallback was already present", logged.String())
	}
}

func TestInstallRemovableFallback_NoBootloaderAnywhereFailsAndStillUnmounts(t *testing.T) {
	runner := &mountSeedingFakeRunner{
		onMount: func(target string) {
			if err := os.MkdirAll(filepath.Join(target, "EFI", "ubuntu"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	e := &Executor{Runner: runner}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	if err := e.installRemovableFallback(context.Background(), ip, esp); err == nil {
		t.Fatal("installRemovableFallback: want an error when the ESP carries no candidate bootloader")
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

func TestInstallRemovableFallback_MountFailurePropagatesAndSkipsUnmount(t *testing.T) {
	runner := &mountSeedingFakeRunner{
		failOn: map[string]error{"mount -t vfat /dev/nvme0n1p1": fmt.Errorf("no such device")},
	}
	e := &Executor{Runner: runner}

	ip := agentapi.ImageDeployPlan{ImageRef: keziov1alpha1.NameRef{Name: "os-image"}, Disk: "/dev/nvme0n1"}
	esp := agentapi.PlanPartition{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP}

	err := e.installRemovableFallback(context.Background(), ip, esp)
	if err == nil || !strings.Contains(err.Error(), "mounting ESP") {
		t.Fatalf("installRemovableFallback error = %v, want it to name the mount failure", err)
	}
}

func TestRunBuiltinInstallRemovableFallback_RunsOnEveryESPPartition(t *testing.T) {
	runner := &mountSeedingFakeRunner{
		onMount: func(target string) {
			dir := filepath.Join(target, "EFI", "BOOT")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "BOOTX64.EFI"), []byte("stub"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	e := &Executor{Runner: runner}

	plan := &agentapi.DeployPlan{
		MachineName: "node-01",
		OS: &agentapi.ImageDeployPlan{
			ImageRef: keziov1alpha1.NameRef{Name: "os-image"},
			Disk:     "/dev/nvme0n1",
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: "/dev/nvme0n1p1", Role: keziov1alpha1.PartitionRoleESP},
			},
		},
		Hooks: []agentapi.ResolvedHook{{
			Name:  "install-fallback",
			Steps: []agentapi.ResolvedHookStep{{Type: agentapi.HookStepTypeBuiltin, Builtin: keziov1alpha1.BuiltinStepInstallRemovableFallback, TimeoutSeconds: 5}},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	var mountCalls int
	for _, c := range runner.calls {
		if strings.HasPrefix(c, "mount ") {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Fatalf("mount calls = %d, want 1 (one ESP-role partition)", mountCalls)
	}
}
