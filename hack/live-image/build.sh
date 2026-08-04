#!/usr/bin/env bash
# Builds the kezio live boot image: a kernel, an initrd, and a squashfs
# root file system that GRUB (see internal/bootserver's grub.go) hands a
# PXE-booting machine.
#
# Tool choice: Debian live-build, not dracut+livenet.
#
# The initrd this image ships has one job at boot: DHCP the NIC, fetch
# the squashfs over HTTP from the URL the kernel cmdline carries
# (fetch=...), overlay a writable tmpfs over the read-only squashfs, and
# pivot into it. live-boot (a Debian package, pulled in by
# config/package-lists/kezio.list.chroot) already implements exactly
# that sequence and already speaks the same boot=live fetch=<url>
# cmdline contract that internal/bootserver's grub.go renders - see
# renderNetBootConfig. Choosing live-boot means this build script needs
# no hand-written initrd logic at all: `lb build` produces a working
# kernel+initrd+squashfs triple directly. dracut's livenet module covers
# similar ground with a leaner initrd, but reaching the same fetch+
# overlay+pivot behavior means assembling and testing that dracut module
# by hand - more custom initrd logic for no behavior this image needs
# that live-boot does not already provide. Given the goal is a working
# image with the least custom initrd logic, live-build wins.
#
# How this script runs live-build: entirely inside containers, so the
# host needs nothing beyond Docker. `lb build` itself needs to create
# device nodes, mount pseudo-filesystems, and chroot while assembling
# the squashfs - the live-build container below therefore runs with
# --privileged. There is no narrower --cap-add set that reliably covers
# every mount/chroot/loop operation lb build performs across its
# stages; --privileged is the documented, honest requirement, not a
# convenience shortcut. The CI workflow (.github/workflows/
# build-live-image.yml) runs on ubuntu-latest, whose runner already
# supports privileged Docker containers.
#
# Determinism: this script pins what upstream itself pins (the ezio
# release tag Dockerfile.seeder builds by default) and otherwise tracks
# Debian sid, the same as every other kezio image - see Dockerfile.
# seeder's own header for why sid, not a snapshot pin, is the right
# tradeoff here. A byte-identical rebuild is not a goal; a rebuild from
# the same commit producing a working image with the same package set
# is.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
live_dir="${script_dir}"
dist_dir="${repo_root}/dist/live"

# EZIO_REF selects the ezio revision built inside the live-build chroot
# (see hooks/live/0400-build-ezio.hook.chroot). Defaults to
# Dockerfile.seeder's own default so the live image ships the same ezio
# revision as the seeder pods it swarms with, unless a caller
# deliberately overrides one or the other.
ezio_ref="${EZIO_REF:-v2.0.28}"

log() {
	printf '[build-live-image] %s\n' "$*" >&2
}

cleanup_includes() {
	# config/includes.chroot is populated fresh on every run (kezio-agent
	# plus the ezio source-build inputs staged below) and is never
	# committed - see .gitignore. Clearing it first keeps a stale
	# EZIO_REF/patch set or a stale kezio-agent binary from a previous
	# run from lingering into this build.
	rm -rf "${live_dir}/config/includes.chroot"
	mkdir -p "${live_dir}/config/includes.chroot"
	cp -a "${live_dir}/config/includes.chroot.static/." "${live_dir}/config/includes.chroot/"
}

stage_ezio_build_inputs() {
	# ezio is compiled *inside* the live-build chroot by
	# hooks/live/0400-build-ezio.hook.chroot, not built in a separate
	# Docker image and copied in: the live image's chroot is a full
	# Debian sid system with its own glibc/libstdc++/etc that it is
	# actively using while `lb build` assembles it, and staging a
	# separately-built binary's shared-library closure over those system
	# paths (the approach Dockerfile.seeder's runtime stage uses, which
	# is fine for that image's empty distroless base) corrupts the
	# chroot mid-extraction. Building against the chroot's own apt
	# snapshot instead guarantees ABI consistency by construction, the
	# same lesson Dockerfile.seeder's builder-stage comment already
	# records for the seeder image.
	#
	# All this function does is hand that hook its inputs: the pinned
	# ref and the patch set (patches/ezio/*.patch is otherwise only
	# reachable from outside the chroot). Staged under includes.chroot so
	# they land in the chroot before hooks run; the hook removes them
	# again once ezio is built.
	log "staging ezio ${ezio_ref} + patches for the in-chroot build"
	local stage_dir="${live_dir}/config/includes.chroot/usr/local/src/kezio-ezio"
	mkdir -p "${stage_dir}/patches"
	printf '%s' "${ezio_ref}" >"${stage_dir}/ezio_ref"
	cp "${repo_root}"/patches/ezio/*.patch "${stage_dir}/patches/"
}

build_agent() {
	log "building kezio-agent (CGO_ENABLED=0, static, matches go.mod's toolchain)"
	local out_dir="${live_dir}/config/includes.chroot/usr/local/bin"
	mkdir -p "${out_dir}"

	# A plain golang:1.26 container, not a Dockerfile target: cmd/agent
	# has no build-time inputs beyond the module itself (unlike ezio,
	# which is built from an upstream source tree inside the live-build
	# chroot - see hooks/live/0400-build-ezio.hook.chroot), so a one-off
	# `go build` run is the whole job. GOCACHE/GOPATH point at a writable
	# path inside the container since the repo itself is bind-mounted
	# read-only - go build must never write into the source tree it is
	# building from. GIT_CONFIG_* silences `go build`'s VCS stamping step
	# (it shells out to git) refusing to touch a checkout it does not
	# own: the checkout's UID rarely matches this container's, root
	# included since git's ownership check does not exempt root, and
	# that mismatch varies by host/runner - setting safe.directory here
	# rather than relying on the environment already having it keeps the
	# build reproducible regardless.
	docker run --rm \
		-v "${repo_root}:/workspace:ro" \
		-v "${out_dir}:/out" \
		-w /workspace \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
		-e GOCACHE=/tmp/go-build -e GOPATH=/tmp/go \
		-e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
		golang:1.26 \
		go build -o /out/kezio-agent ./cmd/agent
}

run_live_build() {
	log "running live-build inside debian:sid (--privileged: lb build chroots and mounts pseudo-filesystems)"
	cat >"${live_dir}/.build-in-container.sh" <<'INNER'
#!/bin/sh
set -eu
apt-get update
apt-get install -y --no-install-recommends live-build ca-certificates
cd /work
# lb build tracks completed stages under .build/ so a re-run after a
# failure resumes instead of repeating finished stages - convenient for
# live-build's own iterative workflow, but it means a config/package-list
# edit made after a failed run silently would not take effect on the next
# run (the stage that would pick it up is skipped as "already done").
# This container is thrown away when the script exits either way, so
# there is no resume to preserve - always start from a clean state.
lb clean --purge
lb config
lb build
INNER
	chmod +x "${live_dir}/.build-in-container.sh"

	docker run --rm --privileged \
		-v "${live_dir}:/work" \
		-w /work \
		debian:sid \
		/work/.build-in-container.sh
	rm -f "${live_dir}/.build-in-container.sh"
}

collect_artifacts() {
	log "collecting artifacts into ${dist_dir}"
	mkdir -p "${dist_dir}"
	local binary_live="${live_dir}/binary/live"
	cp "${binary_live}/vmlinuz" "${dist_dir}/vmlinuz"
	cp "${binary_live}/initrd.img" "${dist_dir}/initrd.img"
	cp "${binary_live}/filesystem.squashfs" "${dist_dir}/filesystem.squashfs"

	# manifest.json: sizes and sha256s for every artifact, so the CI
	# step summary can report them against the 100-300 MiB squashfs
	# target and so internal/bootserver's KernelPath/InitrdPath/
	# SquashfsPath defaults (which these three filenames match) have
	# something to check integrity against downstream.
	(
		cd "${dist_dir}"
		{
			printf '{\n  "artifacts": [\n'
			first=1
			for f in vmlinuz initrd.img filesystem.squashfs; do
				[ "${first}" -eq 1 ] || printf ',\n'
				first=0
				size="$(stat -c%s "${f}")"
				sha="$(sha256sum "${f}" | cut -d' ' -f1)"
				printf '    {"name": "%s", "sizeBytes": %s, "sha256": "%s"}' "${f}" "${size}" "${sha}"
			done
			printf '\n  ]\n}\n'
		} >manifest.json
	)
	log "manifest written to ${dist_dir}/manifest.json"
}

main() {
	mkdir -p "${dist_dir}"
	cleanup_includes
	stage_ezio_build_inputs
	build_agent
	run_live_build
	collect_artifacts
}

main "$@"
