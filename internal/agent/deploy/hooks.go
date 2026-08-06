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
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// hookCleanupTimeout bounds mount/unmount cleanup work done after a
// chrootScript step's own timeout has already elapsed (or the step
// panicked) - see runChrootScriptStep. It is independent of the step's
// own TimeoutSeconds so cleanup is not itself cut short by the deadline
// that just fired.
const hookCleanupTimeout = 30 * time.Second

// runHooks runs every hook in plan.Hooks, in order, and every step
// within each hook, in order. A step's non-zero exit or timeout aborts
// the whole deploy immediately - runHooks returns the first error
// without attempting any later step or hook, per PostHook's own
// documented rule that a failed step must not let the machine reboot
// into an unknown, half-configured OS.
//
// Never logs a step's Content: only its index and Type (and, for a
// builtin step, its Builtin name - never sensitive) are logged, so a
// step sourced from a Secret (ResolvedHookStep.FromSecret) never has its
// content reach the agent's own log output. This applies to every
// step, not only FromSecret ones, since a ConfigMap or inline script can
// carry sensitive content too and there is no way for the agent to tell.
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
// keziov1alpha1.PostHookDefaultTimeoutSeconds for a non-positive value -
// defensive only: internal/agentserver always resolves
// ResolvedHookStep.TimeoutSeconds from EffectiveTimeoutSeconds(), which
// already never returns zero.
func effectiveHookTimeout(step agentapi.ResolvedHookStep) time.Duration {
	seconds := step.TimeoutSeconds
	if seconds <= 0 {
		seconds = keziov1alpha1.PostHookDefaultTimeoutSeconds
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
// runs it with /bin/sh -e - nothing mounted, no chroot, matching
// PostHookScriptSource's "script runs in the live OS" doc comment. The
// script is removed once it returns, whether it succeeded, failed, or
// its context expired.
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
// runChrootScriptStep bind-mounts into the chroot, mirroring PostHook's
// ChrootScript doc comment ("mounts /proc, /sys, /dev"). Mounted in this
// order; unmounted in the reverse order.
var chrootBindMounts = []string{"proc", "sys", "dev"}

// rootPartition returns ip's single PartitionRoleData partition, the
// convention runChrootScriptStep uses to find "the deployed root file
// system" PostHook's ChrootScript doc comment names: kezio's own layout
// convention is one data-role partition per bootable image (the root
// file system), alongside an esp partition and an optional swap
// partition - see api/v1alpha1's PartitionRole* constants, none of which
// names a "root" role distinct from "data". ip carrying zero or more
// than one data-role partition is an image layout runChrootScriptStep
// cannot resolve a single chroot target from, so that is reported as an
// error rather than a guess.
func rootPartition(ip agentapi.ImageDeployPlan) (agentapi.PlanPartition, error) {
	var found []agentapi.PlanPartition
	for _, p := range ip.Partitions {
		if p.Role == keziov1alpha1.PartitionRoleData {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return agentapi.PlanPartition{}, fmt.Errorf("image %s has no data-role partition to chroot into", ip.ImageRef.Name)
	default:
		return agentapi.PlanPartition{}, fmt.Errorf("image %s has %d data-role partitions; chrootScript needs exactly one to identify the root file system", ip.ImageRef.Name, len(found))
	}
}

// runChrootScriptStep mounts the OS image's root partition (rootPartition)
// at a fresh temp mountpoint, bind-mounts /proc, /sys, /dev under it,
// writes step.Content inside as a script, and runs it via chroot + /bin/sh
// -e. Every mount made here is unmounted (in reverse order) before this
// method returns, including when the script fails, times out, or this
// method itself panics - the deferred cleanup runs in all three cases and
// re-panics only after attempting every unmount, so a step's failure can
// never leave the target mounted (see the package's security posture:
// cleanup must be robust even on failure).
//
// This is the only place in kezio-agent that opens the deployed OS
// image's root file system - see internal/agent/deploy's package doc
// comment for why finalize itself never does (ensureRemovableFallback,
// in finalize.go, opens the ESP, a distinct file system, for a
// different reason).
func (e *Executor) runChrootScriptStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	if plan.OS == nil {
		return fmt.Errorf("chrootScript post hook step requires a plan with an OS image")
	}
	root, err := rootPartition(*plan.OS)
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
	// function failed.
	var mounted []string
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
// mounted, first unmounted - undoing the bind mounts before the root
// mount they sit under). A plain unmount failure is retried once with
// --lazy (detach now, finish once nothing still references it); either
// way, unmountAll logs and continues on to the next path rather than
// stopping, so one stuck mount never prevents an attempt to clean up the
// rest.
func (e *Executor) unmountAll(ctx context.Context, mounted []string) {
	for i := len(mounted) - 1; i >= 0; i-- {
		target := mounted[i]
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
// receives the whole plan (not just one image plan) since a builtin like
// "efibootmgr" applies per-ESP-partition across every image in the plan,
// the same scope finalize itself uses.
type builtinFunc func(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error

// builtinRegistry maps a PostHookBuiltinStep.Name (see
// keziov1alpha1.BuiltinStep* constants) to its implementation. Every
// name the CRD's own +kubebuilder:validation:Enum accepts has an entry
// here; runBuiltinStep reports any other name as an error rather than a
// silent no-op, so a builtin the CRD accepts but this registry has not
// caught up to implementing fails loudly instead of pretending to have
// run.
var builtinRegistry = map[string]builtinFunc{
	keziov1alpha1.BuiltinStepMkswap:            runBuiltinMkswap,
	keziov1alpha1.BuiltinStepEfibootmgr:        runBuiltinEfibootmgr,
	keziov1alpha1.BuiltinStepGrowLastPartition: runBuiltinGrowLastPartition,
}

// runBuiltinStep looks step.Builtin up in builtinRegistry and runs it.
func (e *Executor) runBuiltinStep(ctx context.Context, plan *agentapi.DeployPlan, step agentapi.ResolvedHookStep) error {
	fn, ok := builtinRegistry[step.Builtin]
	if !ok {
		return fmt.Errorf("unknown builtin post hook step %q", step.Builtin)
	}
	return fn(ctx, e, plan)
}

// runBuiltinMkswap re-runs mkswap against every swap partition across
// every image plan - the same command applyPartition already runs
// automatically for a swap partition's content write, exposed as an
// explicit, idempotent hook step for a PostHook that wants to sequence
// it relative to its own other steps.
func runBuiltinMkswap(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error {
	for _, ip := range imagePlans(plan) {
		for _, p := range ip.Partitions {
			if classify(p) != kindSwap {
				continue
			}
			if err := e.runMkswap(ctx, ip, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// runBuiltinEfibootmgr creates (or replaces) the UEFI boot entry and
// removable-media fallback bootloader for every ESP-role partition
// across every image plan, the same actions finalize's own
// unconditional pass takes for every PartitionRoleESP partition it
// finds.
func runBuiltinEfibootmgr(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error {
	for _, ip := range imagePlans(plan) {
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

// runBuiltinGrowLastPartition grows every image plan's last partition
// (and its file system, when finalize knows how to resize it) - forced,
// unlike finalize's own pass, which only grows when
// ImageDeployPlan.GrowLastPartition is set. A PostHook step naming this
// builtin is itself the explicit request to grow, so it is not gated on
// that field.
func runBuiltinGrowLastPartition(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error {
	for _, ip := range imagePlans(plan) {
		if err := e.growLastPartition(ctx, ip, true); err != nil {
			return err
		}
	}
	return nil
}
