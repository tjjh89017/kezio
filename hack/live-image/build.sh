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

# EZIO_REF selects the ezio revision the seeder-builder stage compiles.
# Defaults to Dockerfile.seeder's own default so the live image ships
# the same ezio revision as the seeder pods it swarms with, unless a
# caller deliberately overrides one or the other.
ezio_ref="${EZIO_REF:-v2.0.28}"
ezio_builder_image="kezio-live-ezio-builder:local"

log() {
	printf '[build-live-image] %s\n' "$*" >&2
}

cleanup_includes() {
	# config/includes.chroot is populated fresh on every run (the ezio
	# binary and its shared-library closure, copied out of the builder
	# image below) and is never committed - see .gitignore. Clearing it
	# first keeps a stale binary from a previous EZIO_REF from lingering
	# into this build.
	rm -rf "${live_dir}/config/includes.chroot"
	mkdir -p "${live_dir}/config/includes.chroot"
	cp -a "${live_dir}/config/includes.chroot.static/." "${live_dir}/config/includes.chroot/"
}

build_ezio() {
	log "building ezio ${ezio_ref} (reusing Dockerfile.seeder's builder stage)"
	docker build \
		--target builder \
		--build-arg "EZIO_REF=${ezio_ref}" \
		-f "${repo_root}/Dockerfile.seeder" \
		-t "${ezio_builder_image}" \
		"${repo_root}"

	# Dockerfile.seeder's builder stage already resolves ezio's full
	# shared-library closure into /out (see that Dockerfile's own
	# comment on why: apt-get install and ldd run against the same apt
	# transaction there, which a second, independent apt-get install in
	# this image would not guarantee). Reusing that /out tree, instead
	# of re-deriving the closure here, is exactly the "reuse the seeder
	# image build recipe's approach" this build follows.
	local cid
	cid="$(docker create "${ezio_builder_image}")"
	trap 'docker rm -f "${cid}" >/dev/null 2>&1 || true' RETURN
	docker cp "${cid}:/out/." "${live_dir}/config/includes.chroot/"
	mkdir -p "${live_dir}/config/includes.chroot/usr/local/sbin"
	docker cp "${cid}:/usr/local/sbin/ezio" "${live_dir}/config/includes.chroot/usr/local/sbin/ezio"
	docker rm -f "${cid}" >/dev/null
	trap - RETURN
}

run_live_build() {
	log "running live-build inside debian:sid (--privileged: lb build chroots and mounts pseudo-filesystems)"
	cat >"${live_dir}/.build-in-container.sh" <<'INNER'
#!/bin/sh
set -eu
apt-get update
apt-get install -y --no-install-recommends live-build ca-certificates
cd /work
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
	build_ezio
	run_live_build
	collect_artifacts
}

main "$@"
