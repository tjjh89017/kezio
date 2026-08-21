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
	"path/filepath"
	"slices"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// hookCleanupTimeout bounds mount/unmount cleanup work done after a
// chrootScript step's own timeout has already elapsed (or the step
// panicked) - see runChrootScriptStep. It is independent of the step's
// own TimeoutSeconds so cleanup is not itself cut short by the deadline
// that just fired.
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
// keziov1alpha2.PostHookDefaultTimeoutSeconds for a non-positive value -
// defensive only: the manager always resolves ResolvedHookStep's
// timeout from a step's own EffectiveTimeoutSeconds(), which already
// never returns zero.
func effectiveHookTimeout(step agentapi.ResolvedHookStep) time.Duration {
	seconds := step.TimeoutSeconds
	if seconds <= 0 {
		seconds = keziov1alpha2.PostHookDefaultTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// runHookStep dispatches step to its type's runner.
func (e *Executor) runHookStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	switch step.Type {
	case agentapi.HookStepTypeBuiltin:
		return e.runBuiltinStep(ctx, plan, step)
	case agentapi.HookStepTypeScript:
		return e.runScriptStep(ctx, step)
	case agentapi.HookStepTypeChrootScript:
		return e.runChrootScriptStep(ctx, plan, step)
	default:
		return fmt.Errorf("unknown post hook step type %q", step.Type)
	}
}

// runScriptStep writes step.Content to a temp file in the live OS and
// runs it with /bin/sh -e - nothing mounted, no chroot. The script is
// removed once it returns, whether it succeeded, failed, or its context
// expired.
func (e *Executor) runScriptStep(ctx context.Context, step agentapi.ResolvedHookStep) error {
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

	if _, err := e.Runner.Run(ctx, nil, "/bin/sh", "-e", path); err != nil {
		return fmt.Errorf("running script: %w", err)
	}
	return nil
}

// chrootBindMounts is the ordered list of live-OS directories
// runChrootScriptStep bind-mounts into the chroot. Mounted in this
// order; unmounted in the reverse order.
var chrootBindMounts = []string{"proc", "sys", "dev"}

// chrootRootSlot returns the slot runChrootScriptStep mounts as "the
// deployed root file system": plan.Slots' last entry (kezio's own layout
// convention puts the OS root last, the same partition
// growLastPartition's own default targets - see that function's doc
// comment), which must carry restored content (Torrent set) for a chroot
// to make sense. A blank slot (mkfs/swap) in last position means the
// plan has no root file system this step can chroot into.
func chrootRootSlot(plan *agentapi.DeployPlan) (agentapi.DeploySlot, error) {
	if len(plan.Slots) == 0 {
		return agentapi.DeploySlot{}, fmt.Errorf("plan has no OS disk slots to chroot into")
	}
	last := plan.Slots[len(plan.Slots)-1]
	if last.Torrent == nil {
		return agentapi.DeploySlot{}, fmt.Errorf("plan's last OS disk slot (number %d) has no restored content to chroot into", last.Number)
	}
	return last, nil
}

// runChrootScriptStep mounts the OS image's root partition
// (chrootRootSlot) at a fresh temp mountpoint, bind-mounts /proc, /sys,
// /dev under it, writes step.Content inside as a script, and runs it via
// chroot + /bin/sh -e. Every mount made here is unmounted (in reverse
// order) before this method returns, including when the script fails,
// times out, or this method itself panics.
//
// Unlike the reference this was ported from, the root slot's own file
// system type is not on the wire (DeployTorrent carries none), so the
// mount below cannot pass an explicit -t the way the ESP mount in
// finalize.go does; it relies on the kernel's own file system
// autodetection.
func (e *Executor) runChrootScriptStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	root, err := chrootRootSlot(plan)
	if err != nil {
		return err
	}

	mountpoint, err := os.MkdirTemp("", "kezio-posthook-root-")
	if err != nil {
		return fmt.Errorf("creating chroot mountpoint: %w", err)
	}

	// mounted tracks every successful mount, in mount order, so the
	// deferred cleanup below can unmount in exactly the reverse order -
	// including a partial set, when a bind mount partway through this
	// function failed. Capacity is the root mount plus every
	// chrootBindMounts entry, the most this ever holds.
	mounted := make([]string, 0, 1+len(chrootBindMounts))
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), hookCleanupTimeout)
		e.unmountAll(cleanupCtx, mounted)
		cancel()
		if err := os.Remove(mountpoint); err != nil && !os.IsNotExist(err) {
			e.log("removing chroot mountpoint %s: %v", mountpoint, err)
		}
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	if _, err := e.Runner.Run(ctx, nil, "mount", root.Device, mountpoint); err != nil {
		return fmt.Errorf("mounting root partition %s at %s: %w", root.Device, mountpoint, err)
	}
	mounted = append(mounted, mountpoint)

	for _, sub := range chrootBindMounts {
		target := filepath.Join(mountpoint, sub)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("creating chroot bind mount target %s: %w", target, err)
		}
		if _, err := e.Runner.Run(ctx, nil, "mount", "--bind", "/"+sub, target); err != nil {
			return fmt.Errorf("bind mounting /%s at %s: %w", sub, target, err)
		}
		mounted = append(mounted, target)
	}

	const chrootScriptPath = "/.kezio-posthook.sh"
	hostScriptPath := filepath.Join(mountpoint, chrootScriptPath)
	if err := os.WriteFile(hostScriptPath, []byte(step.Content), 0o700); err != nil {
		return fmt.Errorf("writing chroot script: %w", err)
	}
	defer func() { _ = os.Remove(hostScriptPath) }()

	if _, err := e.Runner.Run(ctx, nil, "chroot", mountpoint, "/bin/sh", "-e", chrootScriptPath); err != nil {
		return fmt.Errorf("running chroot script: %w", err)
	}
	return nil
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
// keziov1alpha2.BuiltinStep* constants) to its implementation. Every name
// the CRD's own +kubebuilder:validation:Enum accepts has an entry here;
// runBuiltinStep reports any other name as an error rather than a silent
// no-op, so a builtin the CRD accepts but this registry has not caught up
// to implementing fails loudly instead of pretending to have run.
var builtinRegistry = map[string]builtinFunc{
	keziov1alpha2.BuiltinStepMkswap:                   runBuiltinMkswap,
	keziov1alpha2.BuiltinStepEfibootmgr:               runBuiltinEfibootmgr,
	keziov1alpha2.BuiltinStepGrowLastPartition:        runBuiltinGrowLastPartition,
	keziov1alpha2.BuiltinStepInstallRemovableFallback: runBuiltinInstallRemovableFallback,
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
