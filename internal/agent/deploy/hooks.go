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
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// hookCleanupTimeout bounds unmount cleanup work done after the step that
// made the mount has already failed or timed out. It is independent of
// the step's own TimeoutSeconds so cleanup is not itself cut short by the
// deadline that just fired.
const hookCleanupTimeout = 30 * time.Second

// runHooks runs every hook in plan.Hooks, in order, and every step within
// each hook, in order. A step's non-zero exit or timeout aborts the whole
// deploy immediately - runHooks returns the first error without
// attempting any later step or hook, so a failed step never lets the
// machine reboot into an unknown, half-configured OS.
//
// Never logs a step's Content: only its index and Type (and, for a
// builtin step, its Builtin name) are logged, so a step sourced from a
// Secret never has its content reach the agent's own log output. This
// applies to every step, not only ones marked as Secret-sourced, since a
// ConfigMap or inline script can carry sensitive content too and there is
// no way for the agent to tell them apart on the wire.
func (e *Executor) runHooks(ctx context.Context, plan *agentapi.DeployPlan) error {
	for _, hook := range plan.Hooks {
		for i, step := range hook.Steps {
			e.log("running post hook %q step %d/%d (%s)", hook.Name, i+1, len(hook.Steps), step.Type)

			stepCtx, cancel := context.WithTimeout(ctx, effectiveHookTimeout(step))
			err := e.runHookStep(stepCtx, plan, step)
			cancel()
			if err != nil {
				return fmt.Errorf("post hook %q step %d/%d (%s): %w", hook.Name, i+1, len(hook.Steps), step.Type, err)
			}
		}
	}
	return nil
}

// effectiveHookTimeout returns step's timeout, falling back to
// keziov1alpha3.PostHookDefaultTimeoutSeconds for a non-positive value -
// defensive only: the manager always resolves ResolvedHookStep's
// timeout from a step's own EffectiveTimeoutSeconds(), which already
// never returns zero.
func effectiveHookTimeout(step agentapi.ResolvedHookStep) time.Duration {
	seconds := step.TimeoutSeconds
	if seconds <= 0 {
		seconds = keziov1alpha3.PostHookDefaultTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// runHookStep dispatches step to its type's runner.
func (e *Executor) runHookStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	switch step.Type {
	case agentapi.HookStepTypeBuiltin:
		return e.runBuiltinStep(ctx, plan, step)
	case agentapi.HookStepTypeScript:
		return e.runScriptStep(ctx, plan, step)
	default:
		return fmt.Errorf("unknown post hook step type %q", step.Type)
	}
}

// runScriptStep writes step.Content to a temp file in the live OS and
// runs it with /bin/sh -e - nothing mounted. The script gets plan's
// device paths through its environment (scriptStepEnv), which is all a
// script that must reach the deployed content needs to mount the right
// device itself. The temp file is removed once the script returns,
// whether it succeeded, failed, or its context expired.
func (e *Executor) runScriptStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	f, err := os.CreateTemp("", "kezio-posthook-*.sh")
	if err != nil {
		return fmt.Errorf("creating script temp file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	_, writeErr := f.WriteString(step.Content)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("writing script temp file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("writing script temp file: %w", closeErr)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("making script temp file executable: %w", err)
	}

	if _, err := e.Runner.RunEnv(ctx, scriptStepEnv(plan), nil, "/bin/sh", "-e", path); err != nil {
		return fmt.Errorf("running script: %w", err)
	}
	return nil
}

// Environment variable names a script step gets its device paths under.
// This set is the documented contract a PostHook script may rely on (see
// keziov1alpha3.PostHookStep.Script); keep the names stable.
//
//   - KEZIO_TARGET_DISK is the OS image's target disk, absent when the
//     plan deploys no OS image.
//   - KEZIO_PARTITIONS lists that disk's partition numbers, separated by
//     spaces, in plan order.
//   - KEZIO_PART_<number> is one such partition's device path.
//   - KEZIO_DATA_DISKS lists the 1-based index of every data image disk,
//     separated by spaces, in plan order.
//   - KEZIO_DATA_DISK_<index> is one data image disk's device path, with
//     KEZIO_DATA_DISK_<index>_PARTITIONS and
//     KEZIO_DATA_DISK_<index>_PART_<number> as that disk's counterparts of
//     the two names above.
const (
	envTargetDisk = "KEZIO_TARGET_DISK"
	envPartitions = "KEZIO_PARTITIONS"
	envPartPrefix = "KEZIO_PART_"
	envDataDisks  = "KEZIO_DATA_DISKS"
	envDataPrefix = "KEZIO_DATA_DISK_"
)

// scriptStepEnv builds the "NAME=value" environment entries a script step
// runs with. A disk with no device path contributes nothing, so a script
// reading a name that is absent can tell "the plan has no such disk" from
// "the plan has one" with a plain shell test.
func scriptStepEnv(plan *agentapi.DeployPlan) []string {
	env := make([]string, 0, 2+len(plan.Slots)+2*len(plan.DataImages))
	if plan.TargetDisk != "" {
		env = append(env, envTargetDisk+"="+plan.TargetDisk)
		env = append(env, slotEnv(envPartitions, envPartPrefix, plan.Slots)...)
	}

	indexes := make([]string, 0, len(plan.DataImages))
	for i, di := range plan.DataImages {
		if di.TargetDisk == "" {
			continue
		}
		index := strconv.Itoa(i + 1)
		indexes = append(indexes, index)
		env = append(env, envDataPrefix+index+"="+di.TargetDisk)
		env = append(env, slotEnv(envDataPrefix+index+"_PARTITIONS", envDataPrefix+index+"_PART_", di.Slots)...)
	}
	if len(indexes) > 0 {
		env = append(env, envDataDisks+"="+strings.Join(indexes, " "))
	}
	return env
}

// slotEnv returns one disk's slot entries: listName carrying every
// partition number, then one devicePrefix+number entry per slot that has
// a resolved device path.
func slotEnv(listName, devicePrefix string, slots []agentapi.DeploySlot) []string {
	env := make([]string, 0, len(slots)+1)
	numbers := make([]string, 0, len(slots))
	for _, s := range slots {
		number := strconv.Itoa(int(s.Number))
		numbers = append(numbers, number)
		if s.Device != "" {
			env = append(env, devicePrefix+number+"="+s.Device)
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	return append(env, listName+"="+strings.Join(numbers, " "))
}

// unmountAll unmounts every path in mounted, in reverse order (last
// mounted, first unmounted). A plain unmount failure is retried once
// with --lazy; either way, unmountAll logs and continues on to the next
// path rather than stopping, so one stuck mount never prevents an
// attempt to clean up the rest.
func (e *Executor) unmountAll(ctx context.Context, mounted []string) {
	for _, target := range slices.Backward(mounted) {
		if _, err := e.Runner.Run(ctx, nil, "umount", target); err == nil {
			continue
		}
		e.log("unmounting %s failed; retrying with --lazy", target)
		if _, err := e.Runner.Run(ctx, nil, "umount", "--lazy", target); err != nil {
			e.log("lazy unmount of %s also failed: %v", target, err)
		}
	}
}

// builtinFunc is one builtin post hook step's implementation. Each
// receives the whole plan (not just one disk plan) since a builtin like
// "mkswap" applies across every disk plan, and params carries the
// step's own ResolvedHookStep.Params (nil-safe: an unset key reads as
// "").
type builtinFunc func(ctx context.Context, e *Executor, plan *agentapi.DeployPlan, params map[string]string) error

// builtinRegistry maps a PostHookBuiltinStep.Name (see
// keziov1alpha3.BuiltinStep* constants) to its implementation. Every name
// the CRD's own +kubebuilder:validation:Enum accepts has an entry here;
// runBuiltinStep reports any other name as an error rather than a silent
// no-op, so a builtin the CRD accepts but this registry has not caught up
// to implementing fails loudly instead of pretending to have run.
var builtinRegistry = map[string]builtinFunc{
	keziov1alpha3.BuiltinStepMkswap:                   runBuiltinMkswap,
	keziov1alpha3.BuiltinStepEfibootmgr:               runBuiltinEfibootmgr,
	keziov1alpha3.BuiltinStepGrowLastPartition:        runBuiltinGrowLastPartition,
	keziov1alpha3.BuiltinStepInstallRemovableFallback: runBuiltinInstallRemovableFallback,
}

// runBuiltinStep looks step.Builtin up in builtinRegistry and runs it.
func (e *Executor) runBuiltinStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	fn, ok := builtinRegistry[step.Builtin]
	if !ok {
		return fmt.Errorf("unknown builtin post hook step %q", step.Builtin)
	}
	return fn(ctx, e, plan, step.Params)
}

// runBuiltinMkswap re-runs mkswap against every swap slot across every
// disk plan - the same command writeSlot already runs automatically for
// a swap slot's content write, exposed as an explicit, idempotent hook
// step for a PostHook that wants to sequence it relative to its own
// other steps.
func runBuiltinMkswap(ctx context.Context, e *Executor, plan *agentapi.DeployPlan, _ map[string]string) error {
	for _, dp := range diskPlans(plan) {
		for _, s := range dp.slots {
			if s.Swap == nil {
				continue
			}
			if err := e.runMkswap(ctx, dp, s); err != nil {
				return err
			}
		}
	}
	return nil
}
