#!/bin/sh
# Fetches the published kezio boot artifacts (see
# .github/workflows/build-live-image.yml: vmlinuz, initrd.img,
# filesystem.squashfs, shimx64.efi, grubx64.efi, manifest.json,
# sha256sums) from a GitHub Release into DEST_DIR, then verifies every
# fetched file against the release's sha256sums asset.
#
# Runs as an initContainer (config/bootserver and config/bootd each add
# their own patch that runs this against a different BOOT_ARTIFACTS_FILES
# / DEST_DIR - see this directory's README) so the volume the main
# container mounts is always populated before it starts, replacing what
# used to be a bare "operator-provided volume" gap. Written for the
# `curlimages/curl` image's ash/busybox, so no bash-only syntax.
set -eu

: "${BOOT_ARTIFACTS_REPO:?set BOOT_ARTIFACTS_REPO, e.g. tjjh89017/kezio}"
: "${BOOT_ARTIFACTS_VERSION:?set BOOT_ARTIFACTS_VERSION to a release tag (e.g. v0.1.0) or the literal 'latest'}"
: "${BOOT_ARTIFACTS_FILES:?set BOOT_ARTIFACTS_FILES to a space-separated list of release asset names to fetch}"
: "${DEST_DIR:?set DEST_DIR to the directory to fetch artifacts into}"

# GitHub serves a stable "latest release" alias under /releases/latest/
# download/<asset>, distinct from /releases/download/<tag>/<asset> for a
# specific tag - both are plain, unauthenticated-friendly HTTPS URLs on
# this public repository (see build-live-image.yml's publishing-target
# rationale), so BOOT_ARTIFACTS_VERSION=latest needs no GitHub API call
# to resolve.
if [ "${BOOT_ARTIFACTS_VERSION}" = "latest" ]; then
	base_url="https://github.com/${BOOT_ARTIFACTS_REPO}/releases/latest/download"
else
	base_url="https://github.com/${BOOT_ARTIFACTS_REPO}/releases/download/${BOOT_ARTIFACTS_VERSION}"
fi

mkdir -p "${DEST_DIR}"
cd "${DEST_DIR}"

echo "Fetching sha256sums from ${base_url}"
curl -fsSL -o sha256sums "${base_url}/sha256sums"

for f in ${BOOT_ARTIFACTS_FILES}; do
	echo "Fetching ${f}"
	curl -fsSL -o "${f}" "${base_url}/${f}"
done

# Filter sha256sums down to just the files this call actually fetched -
# a plain `sha256sum -c sha256sums` would also fail on assets this
# DEST_DIR was never meant to receive (bootserver's DEST_DIR has no
# reason to hold shimx64.efi, and vice versa for bootd).
: >sha256sums.filtered
for f in ${BOOT_ARTIFACTS_FILES}; do
	grep -E "  ${f}\$" sha256sums >>sha256sums.filtered
done

echo "Verifying checksums"
sha256sum -c sha256sums.filtered
rm -f sha256sums.filtered

echo "Boot artifacts ready in ${DEST_DIR}:"
ls -la "${DEST_DIR}"
