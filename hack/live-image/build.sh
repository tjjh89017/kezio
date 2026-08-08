#!/usr/bin/env bash
# Builds the kezio live boot image: a kernel, an initrd, and a squashfs
# root file system that GRUB (see internal/bootserver's grub.go) hands a
# PXE-booting machine. It also stages the signed shimx64.efi/grubx64.efi
# kezio-bootd serves over TFTP (see stage_signed_boot_binaries), so the
# whole PXE-to-agent artifact set - vmlinuz, initrd.img,
# filesystem.squashfs, shimx64.efi, grubx64.efi, kernel.config,
# manifest.json, sha256sums - comes out of dist/live/ as one set for
# main.yaml's boot-artifacts job to publish together.
#
# Tool choice: Debian live-build, not dracut+livenet. The initrd's only
# job at boot is to DHCP the NIC, fetch the squashfs over HTTP from the
# kernel cmdline's fetch=... URL, overlay a writable tmpfs over it, and
# pivot into it. live-boot (pulled in by config/package-lists/
# kezio.list.chroot) already implements that sequence and speaks the
# same boot=live fetch=<url> contract internal/bootserver's grub.go
# renders (see renderNetBootConfig), so `lb build` needs no hand-written
# initrd logic. dracut's livenet module would need custom assembly to
# reach the same behavior for no benefit live-boot doesn't provide.
#
# This script runs live-build entirely inside containers, so the host
# needs nothing beyond Docker. `lb build` needs to create device nodes,
# mount pseudo-filesystems, and chroot while assembling the squashfs, so
# the live-build container runs with --privileged; no narrower
# --cap-add set reliably covers every mount/chroot/loop operation it
# performs. The CI runner (ubuntu-latest) supports privileged containers.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
live_dir="${script_dir}"
dist_dir="${repo_root}/dist/live"

log() {
	printf '[build-live-image] %s\n' "$*" >&2
}

cleanup_includes() {
	# config/includes.chroot (kezio-agent, staged fresh by build_agent
	# below) is never committed - see .gitignore. Clearing it first keeps
	# a stale kezio-agent binary from a previous run from lingering into
	# this build.
	rm -rf "${live_dir}/config/includes.chroot"
	mkdir -p "${live_dir}/config/includes.chroot"
	cp -a "${live_dir}/config/includes.chroot.static/." "${live_dir}/config/includes.chroot/"
}

stage_signed_boot_binaries() {
	# shimx64.efi and grubx64.efi are what kezio-bootd serves over TFTP
	# (internal/bootd.ShimFilename / GrubFilename): PXE-booting UEFI
	# firmware chainloads the signed shim, which chainloads grub, which
	# loads the GRUB config internal/bootserver renders. kezio never
	# builds either binary itself - Secure Boot requires a chain the
	# firmware already trusts, which only a distribution's own signed
	# package provides. Pulling shim-signed/grub-efi-amd64-signed from
	# debian:sid (the same base image run_live_build uses) keeps Debian
	# sid the single version-pin for every package this script touches.
	#
	# The GRUB build extracted here is grubnetx64.efi.signed - the
	# NETBOOT variant - published as grubx64.efi (the fixed name shim
	# requests from the directory it was loaded from, so the published
	# name cannot change without patching shim). The package's plain
	# grubx64.efi.signed is the DISK-boot build: Debian embeds prefix
	# "/EFI/debian" in it, so loaded over TFTP it looks for a local disk
	# it cannot have yet and drops to a bare grub> prompt. The netboot
	# build instead embeds prefix "/grub" resolved against the device it
	# was loaded from, i.e. (tftp,<next-server>)/grub, satisfied by
	# kezio-bootd serving its rendered config at grub/grub.cfg (see
	# internal/bootd.GrubConfigPath). The image is monolithic (every
	# module in Debian's NET_MODULES list, http and tftp included, is
	# built in), so no x86_64-efi module tree needs publishing alongside
	# it.
	log "extracting shimx64.efi/grubnetx64.efi from Debian's signed packages"
	docker run --rm \
		-v "${dist_dir}:/out" \
		debian:sid \
		sh -c '
			set -eu
			apt-get update
			# sbsigntool provides sbverify, used below to confirm the
			# extracted binaries actually carry an Authenticode signature
			# instead of silently shipping an unsigned fallback.
			apt-get install -y --no-install-recommends sbsigntool
			cd /tmp
			apt-get download shim-signed grub-efi-amd64-signed
			mkdir -p extracted
			for deb in *.deb; do
				dpkg -x "${deb}" extracted
			done
			# Match the ".signed" suffix explicitly, not a bare
			# "shimx64.efi*" glob: shim-signed/grub-efi-amd64-signed ship
			# only the *.signed forms today (verified against Debian sid
			# package contents), but a bare "*" glob would silently accept
			# an unsigned shimx64.efi/grubx64.efi too if a future package
			# revision ever ships one alongside the signed copy. Requiring
			# the ".signed" suffix keeps this extraction fail-closed
			# against that layout change instead of relying on find(1)
			# traversal order to prefer the right file.
			shim_src="$(find extracted -iname "shimx64.efi.signed" -print -quit)"
			grub_src="$(find extracted -iname "grubnetx64.efi.signed" -print -quit)"
			if [ -z "${shim_src}" ] || [ -z "${grub_src}" ]; then
				echo "shimx64.efi.signed / grubnetx64.efi.signed not found under the extracted shim-signed/grub-efi-amd64-signed packages; package layout may have changed." >&2
				find extracted -iname "*.efi*" >&2
				exit 1
			fi
			# Confirm each binary actually carries an Authenticode
			# signature (sbverify --list fails on an unsigned PE/COFF
			# image) before it ever reaches dist/live/ - a silent
			# unsigned shim/grub would boot fine with Secure Boot off and
			# only be caught, if at all, by a machine refusing to chainload
			# it with Secure Boot on.
			for f in "${shim_src}" "${grub_src}"; do
				if [ ! -s "${f}" ]; then
					echo "${f} is empty; refusing to publish an empty boot binary." >&2
					exit 1
				fi
				if ! sbverify --list "${f}"; then
					echo "${f} failed Authenticode signature verification (sbverify --list); it is not a validly signed boot binary." >&2
					exit 1
				fi
			done
			# internal/bootd.ShimFilename / GrubFilename are fixed names
			# with no ".signed" suffix - the packages ship them with
			# one, so normalize on copy. The netboot GRUB build is also
			# renamed grubnetx64 -> grubx64 here: grubx64.efi is the
			# hard-coded second-stage name shim fetches from the
			# directory it was loaded from (confirmed in the e2e TFTP
			# logs), so the netboot build must be published under it.
			cp "${shim_src}" /out/shimx64.efi
			cp "${grub_src}" /out/grubx64.efi
		'
}

build_agent() {
	log "building kezio-agent (CGO_ENABLED=0, static, matches go.mod's toolchain)"
	local out_dir="${live_dir}/config/includes.chroot/usr/local/bin"
	mkdir -p "${out_dir}"

	# A plain golang:1.26 container, not a Dockerfile target: cmd/agent
	# has no build-time inputs beyond the module itself, so a one-off
	# `go build` is the whole job. GOCACHE/GOPATH point at a writable
	# path since the repo is bind-mounted read-only. GIT_CONFIG_* sets
	# safe.directory so `go build`'s VCS-stamping git call does not
	# refuse a checkout whose UID mismatches the container's (root
	# included, since git's ownership check does not exempt it).
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
lb build || {
	echo "=== DEBUG: lb build failed, dumping chroot apt state ==="
	cat chroot/etc/apt/sources.list 2>&1 || true
	for f in chroot/etc/apt/sources.list.d/*; do
		echo "--- ${f} ---"
		cat "${f}" 2>&1 || true
	done
	echo "=== DEBUG: apt-cache policy for firmware-linux ==="
	chroot chroot apt-cache policy firmware-linux 2>&1 || true
	exit 1
}
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

	# config-<ver> has no other path into dist/live/ once
	# config/rootfs/excludes drops /boot from the squashfs.
	cp "${live_dir}"/chroot/boot/config-* "${dist_dir}/kernel.config"

	# hooks/live/9900-package-size-report.hook.chroot's report. Not part
	# of manifest.json/sha256sums - the CI step summary reads it directly.
	cp "${live_dir}/chroot/tmp/package-sizes.txt" "${dist_dir}/package-sizes.txt"

	# manifest.json: sizes and sha256s for every artifact, so the CI step
	# summary can report them against the 100-300 MiB squashfs target and
	# downstream can check integrity against internal/bootserver's
	# KernelPath/InitrdPath/SquashfsPath defaults.
	#
	# agentCommit: the git SHA build_agent's checkout was at when it
	# cross-compiled cmd/agent into this image (AGENT_COMMIT overrides it
	# for a caller that already knows the SHA). Lets a consumer tell
	# which kezio-agent commit a given kezio-boot-artifacts image
	# actually carries instead of trusting an image tag alone - a lane
	# pinned to a published release could otherwise go green while
	# silently booting a stale agent.
	local agent_commit="${AGENT_COMMIT:-$(cd "${repo_root}" && git rev-parse HEAD)}"
	(
		cd "${dist_dir}"
		{
			printf '{\n  "agentCommit": "%s",\n  "artifacts": [\n' "${agent_commit}"
			first=1
			for f in vmlinuz initrd.img filesystem.squashfs shimx64.efi grubx64.efi kernel.config; do
				[ "${first}" -eq 1 ] || printf ',\n'
				first=0
				size="$(stat -c%s "${f}")"
				sha="$(sha256sum "${f}" | cut -d' ' -f1)"
				printf '    {"name": "%s", "sizeBytes": %s, "sha256": "%s"}' "${f}" "${size}" "${sha}"
			done
			printf '\n  ]\n}\n'
		} >manifest.json
	)
	log "manifest written to ${dist_dir}/manifest.json (agentCommit ${agent_commit})"

	# sha256sums: a GNU-coreutils-format checksum file (one
	# "<sha256>  <name>" line per artifact, manifest.json included) so a
	# human downloading the GitHub Release asset can `sha256sum -c`
	# instead of parsing manifest.json. Runtime consumers instead verify
	# the OCI image digest docker/boot-artifacts/Dockerfile bakes these
	# files into.
	(
		cd "${dist_dir}"
		sha256sum vmlinuz initrd.img filesystem.squashfs shimx64.efi grubx64.efi kernel.config manifest.json >sha256sums
	)
	log "checksums written to ${dist_dir}/sha256sums"
}

main() {
	mkdir -p "${dist_dir}"
	cleanup_includes
	stage_signed_boot_binaries
	build_agent
	run_live_build
	collect_artifacts
}

main "$@"
