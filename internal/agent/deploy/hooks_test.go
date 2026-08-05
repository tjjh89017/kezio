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
	"strings"
	"sync"
	"testing"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

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
