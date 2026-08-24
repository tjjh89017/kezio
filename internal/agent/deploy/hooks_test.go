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
	"slices"
	"strings"
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// shellCommand is the command a script step always runs through.
const shellCommand = "/bin/sh"

func newHooksExecutor() (*Executor, *fakeRunner) {
	runner := newFakeRunner()
	return &Executor{Runner: runner}, runner
}

func TestRunHooks_OrderAcrossHooksAndSteps(t *testing.T) {
	e, runner := newHooksExecutor()
	plan := basicPlan()
	plan.Hooks = []agentapi.ResolvedHook{
		{Name: "first", Steps: []agentapi.ResolvedHookStep{
			{Type: agentapi.HookStepTypeScript, Content: "echo one", TimeoutSeconds: 5},
		}},
		{Name: "second", Steps: []agentapi.ResolvedHookStep{
			{Type: agentapi.HookStepTypeScript, Content: "echo two", TimeoutSeconds: 5},
			{Type: agentapi.HookStepTypeScript, Content: "echo three", TimeoutSeconds: 5},
		}},
	}

	if err := e.runHooks(context.Background(), plan); err != nil {
		t.Fatalf("runHooks: %v", err)
	}

	var shCalls int
	for _, c := range runner.calls {
		if c.name == shellCommand {
			shCalls++
		}
	}
	if shCalls != 3 {
		t.Fatalf("ran %d script steps, want 3 (in order: first.0, second.0, second.1)", shCalls)
	}
}

func TestRunHooks_StepFailureAbortsRemainingSteps(t *testing.T) {
	e, runner := newHooksExecutor()

	plan := basicPlan()
	plan.Hooks = []agentapi.ResolvedHook{
		{Name: "h", Steps: []agentapi.ResolvedHookStep{
			{Type: agentapi.HookStepTypeBuiltin, Builtin: "does-not-exist", TimeoutSeconds: 5},
			{Type: agentapi.HookStepTypeScript, Content: "echo unreachable", TimeoutSeconds: 5},
		}},
	}

	err := e.runHooks(context.Background(), plan)
	if err == nil {
		t.Fatal("runHooks: want an error for an unknown builtin step")
	}
	if !strings.Contains(err.Error(), "unknown builtin") {
		t.Fatalf("runHooks error = %v, want it to name the unknown builtin", err)
	}
	for _, c := range runner.calls {
		if c.name == shellCommand {
			t.Errorf("the script step after a failing step still ran")
		}
	}
}

func TestRunHookStep_ScriptRunsInLiveOSNoMount(t *testing.T) {
	e, runner := newHooksExecutor()
	step := agentapi.ResolvedHookStep{Type: agentapi.HookStepTypeScript, Content: "echo hi", TimeoutSeconds: 5}

	if err := e.runHookStep(context.Background(), basicPlan(), step); err != nil {
		t.Fatalf("runHookStep: %v", err)
	}
	for _, c := range runner.calls {
		if c.name == "mount" || c.name == "chroot" {
			t.Errorf("a plain script step ran %q; want no mount/chroot", c.name)
		}
	}
}

func TestRunHookStep_ChrootScriptTypeIsRejected(t *testing.T) {
	e, runner := newHooksExecutor()
	step := agentapi.ResolvedHookStep{Type: "chrootScript", Content: "echo hi", TimeoutSeconds: 5}

	err := e.runHookStep(context.Background(), basicPlan(), step)
	if err == nil {
		t.Fatal("runHookStep: want an error for the removed chrootScript step type")
	}
	if !strings.Contains(err.Error(), "unknown post hook step type") {
		t.Fatalf("runHookStep error = %v, want it to report an unknown step type", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("a chrootScript step still ran commands: %v", runner.commandNames())
	}
}

func TestRunScriptStep_PassesPlanDevicesInTheEnvironment(t *testing.T) {
	e, runner := newHooksExecutor()
	plan := basicPlan()
	plan.DataImages = []agentapi.DeployDataImagePlan{{
		ImageRef:     keziov1alpha2.NameRef{Name: "data"},
		TargetDisk:   "/dev/sdb",
		SfdiskScript: fixtureSfdisk,
		Slots:        []agentapi.DeploySlot{{Number: 1, Device: "/dev/sdb1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}},
	}}
	step := agentapi.ResolvedHookStep{Type: agentapi.HookStepTypeScript, Content: "echo hi", TimeoutSeconds: 5}

	if err := e.runHookStep(context.Background(), plan, step); err != nil {
		t.Fatalf("runHookStep: %v", err)
	}

	var env []string
	for _, c := range runner.calls {
		if c.name == shellCommand {
			env = c.env
		}
	}
	for _, want := range []string{
		"KEZIO_TARGET_DISK=/dev/sda",
		"KEZIO_PARTITIONS=1 2 3",
		"KEZIO_PART_1=/dev/sda1",
		"KEZIO_PART_3=/dev/sda3",
		"KEZIO_DATA_DISKS=1",
		"KEZIO_DATA_DISK_1=/dev/sdb",
		"KEZIO_DATA_DISK_1_PARTITIONS=1",
		"KEZIO_DATA_DISK_1_PART_1=/dev/sdb1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("script step environment is missing %q; got %v", want, env)
		}
	}
}

func TestRunScriptStep_OmitsDiskNamesAPlanDoesNotCarry(t *testing.T) {
	e, runner := newHooksExecutor()
	plan := basicPlan()
	plan.TargetDisk = ""
	plan.Slots = nil
	step := agentapi.ResolvedHookStep{Type: agentapi.HookStepTypeScript, Content: "echo hi", TimeoutSeconds: 5}

	if err := e.runHookStep(context.Background(), plan, step); err != nil {
		t.Fatalf("runHookStep: %v", err)
	}

	for _, c := range runner.calls {
		if c.name != shellCommand {
			continue
		}
		for _, entry := range c.env {
			if strings.HasPrefix(entry, "KEZIO_TARGET_DISK=") || strings.HasPrefix(entry, "KEZIO_PART") {
				t.Errorf("a plan with no OS disk still set %q", entry)
			}
		}
	}
}

func TestRunBuiltinMkswap_RunsAgainstEverySwapSlotAcrossDisks(t *testing.T) {
	e, runner := newHooksExecutor()
	plan := basicPlan()
	plan.DataImages = []agentapi.DeployDataImagePlan{{
		ImageRef:     keziov1alpha2.NameRef{Name: "data"},
		TargetDisk:   "/dev/sdb",
		SfdiskScript: fixtureSfdisk,
		Slots:        []agentapi.DeploySlot{{Number: 1, Device: "/dev/sdb1", Swap: &agentapi.DeploySwap{UUID: "data-swap"}}},
	}}

	if err := runBuiltinMkswap(context.Background(), e, plan, nil); err != nil {
		t.Fatalf("runBuiltinMkswap: %v", err)
	}
	calls := runner.commandNames()
	if !slices.Contains(calls, "mkswap --uuid swap-uuid /dev/sda2") {
		t.Errorf("mkswap did not run against the OS disk's swap slot; calls: %v", calls)
	}
	if !slices.Contains(calls, "mkswap --uuid data-swap /dev/sdb1") {
		t.Errorf("mkswap did not run against the data image's swap slot; calls: %v", calls)
	}
}

func TestRunBuiltinStep_UnknownNameIsAnError(t *testing.T) {
	e, _ := newHooksExecutor()
	step := agentapi.ResolvedHookStep{Type: agentapi.HookStepTypeBuiltin, Builtin: "not-a-real-builtin", TimeoutSeconds: 5}
	if err := e.runBuiltinStep(context.Background(), basicPlan(), step); err == nil {
		t.Fatal("runBuiltinStep: want an error for an unregistered builtin name")
	}
}
