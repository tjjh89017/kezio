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
	"runtime"
	"strings"
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
// comment for why finalize itself never does (installRemovableFallback,
// in this file, opens the ESP, a distinct file system, for a builtin
// step's own opt-in reason).
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

	// -t root.FSType is explicit rather than left to mount's own
	// autoprobe, for the same reason ensureRemovableFallback's ESP mount
	// in finalize.go is explicit: FSType is already known from the plan
	// (see PlanPartition.FSType's doc comment - populated for every
	// content partition, which a root partition always is), so there is
	// no reason to let autoprobe guess and risk picking the wrong file
	// system driver.
	var mountArgs []string
	if root.FSType != "" {
		mountArgs = append(mountArgs, "-t", root.FSType)
	}
	mountArgs = append(mountArgs, root.Device, mountpoint)
	if _, err := e.Runner.Run(ctx, nil, "mount", mountArgs...); err != nil {
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
	keziov1alpha1.BuiltinStepMkswap:                   runBuiltinMkswap,
	keziov1alpha1.BuiltinStepEfibootmgr:               runBuiltinEfibootmgr,
	keziov1alpha1.BuiltinStepGrowLastPartition:        runBuiltinGrowLastPartition,
	keziov1alpha1.BuiltinStepInstallRemovableFallback: runBuiltinInstallRemovableFallback,
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

// runBuiltinEfibootmgr creates (or replaces) the UEFI boot entry for
// every ESP-role partition across every image plan, the same action
// finalize's own unconditional pass takes for every PartitionRoleESP
// partition it finds.
func runBuiltinEfibootmgr(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error {
	for _, ip := range imagePlans(plan) {
		for _, p := range ip.Partitions {
			if p.Role != keziov1alpha1.PartitionRoleESP {
				continue
			}
			if err := e.ensureEFIBootEntry(ctx, plan.MachineName, ip, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// removableFallbackCandidates are the file names
// installRemovableFallback looks for under any other EFI/<name>/
// directory on the ESP - shim first (so a secure-boot image keeps
// chaining through it to grub), grub itself as the fallback for a
// non-shim image. These are the same two names kezio-bootd's own
// netboot chain fetches (cmd/bootd/main.go), so an image built for
// kezio's PXE flow already carries one of them somewhere under EFI/.
//
// These names are x86_64-specific (the "x64" suffix); this builtin
// shares efiRemovableLoaderPathForArch's two-architecture scope and
// fails explicitly rather than guessing an aarch64 name.
var removableFallbackCandidates = []string{"shimx64.efi", "grubx64.efi"}

// runBuiltinInstallRemovableFallback mounts every ESP-role partition
// across every image plan and makes sure it carries a bootloader at the
// image contract's fallback path (efiRemovableLoaderPath), copying one
// in from removableFallbackCandidates when it is missing. This exists
// only for a golden image that does not already meet that contract - a
// PostHook must name this builtin explicitly; nothing in finalize's own
// unconditional pass calls it (see efiRemovableLoaderPath's doc comment
// for why finalize itself never opens the ESP's file system).
func runBuiltinInstallRemovableFallback(ctx context.Context, e *Executor, plan *agentapi.DeployPlan) error {
	for _, ip := range imagePlans(plan) {
		for _, p := range ip.Partitions {
			if p.Role != keziov1alpha1.PartitionRoleESP {
				continue
			}
			if err := e.installRemovableFallback(ctx, ip, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// installRemovableFallback mounts esp (an ESP-role partition of ip.Disk)
// and makes sure it carries a bootloader at the deploying machine's own
// architecture's fallback path (efiRemovableLoaderPathForArch of
// runtime.GOARCH), installing one from whichever EFI/<name>/ directory
// holds a removableFallbackCandidates match when it does not already.
func (e *Executor) installRemovableFallback(ctx context.Context, ip agentapi.ImageDeployPlan, esp agentapi.PlanPartition) error {
	loaderPath, err := efiRemovableLoaderPathForArch(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("image %s: %w", ip.ImageRef.Name, err)
	}
	// loaderPath is the NVRAM-style path efibootmgr takes ("\EFI\BOOT\...");
	// relPath is the same path as a POSIX-style relative path under an ESP
	// mount, the form this function needs to check for and, if absent,
	// create the file at.
	relPath := filepath.FromSlash(strings.ReplaceAll(strings.TrimPrefix(loaderPath, `\`), `\`, "/"))

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

	// -t vfat is explicit rather than left to mount's own autoprobe: the
	// UEFI spec requires the ESP to carry a FAT file system, but a bare
	// "mount esp.Device mountpoint" lets the kernel/mount guess a type
	// from whatever superblocks it tries first, which can pick something
	// else entirely (observed picking squashfs and failing outright) when
	// the device's content happens to look ambiguous to autoprobe.
	if _, err := e.Runner.Run(ctx, nil, "mount", "-t", "vfat", esp.Device, mountpoint); err != nil {
		return fmt.Errorf("image %s: mounting ESP %s at %s: %w", ip.ImageRef.Name, esp.Device, mountpoint, err)
	}

	installed, source, err := ensureRemovableFallbackFile(mountpoint, relPath)
	if err != nil {
		return fmt.Errorf("image %s: ensuring removable fallback bootloader: %w", ip.ImageRef.Name, err)
	}
	if installed {
		e.log("installed removable fallback bootloader on %s from %s", esp.Device, source)
	}
	return nil
}

// ensureRemovableFallbackFile is installRemovableFallback's pure part:
// given root (an already-mounted ESP) and relPath (the ESP-relative
// fallback path to ensure), it reports whether relPath is already
// present, installing a copy from the first removableFallbackCandidates
// match under any other EFI/<name>/ directory when it is not. source
// names whichever file it copied from ("" when nothing needed copying).
// Kept separate from installRemovableFallback so this decision is
// unit-testable against a plain temp directory, with no mount or Runner
// involved.
func ensureRemovableFallbackFile(root, relPath string) (installed bool, source string, err error) {
	fallback := filepath.Join(root, relPath)
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

// findESPBootloader looks for removableFallbackCandidates under every
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
		for _, candidate := range removableFallbackCandidates {
			path := filepath.Join(efiDir, entry.Name(), candidate)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no bootloader (%s) found under any EFI/<name>/ directory in %s", strings.Join(removableFallbackCandidates, ", "), efiDir)
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
