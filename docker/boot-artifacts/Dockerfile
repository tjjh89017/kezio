# Packages the live boot artifacts hack/live-image/build.sh produces
# (vmlinuz, initrd.img, filesystem.squashfs, shimx64.efi, grubx64.efi,
# kernel.config, manifest.json, sha256sums) into an OCI image, so
# internal/controller's bootd Deployment initContainer can populate its
# BOOT_ARTIFACTS_DIR from a plain `cp` instead of a runtime fetch. Build
# context is the repository root; every COPY source below comes from
# dist/live/. kernel.config and sha256sums ride along for artifact-set
# consistency; bootd doesn't read either at boot time.
#
# busybox, not distroless/scratch: the whole point of this image is to
# be `cp`-able by an initContainer with no extra tooling, and busybox's
# own multi-call binary already provides cp with no package manager or
# shell surface beyond what that one command needs. USER 65532:65532
# (a numeric uid/gid, the same convention docker/ingest/Dockerfile and
# docker/image-service/Dockerfile use) is required, not cosmetic:
# restricted Pod Security Admission cannot resolve a non-numeric image
# USER to verify runAsNonRoot, so every consumer's initContainer
# securityContext can set runAsNonRoot: true with no runAsUser/runAsGroup
# override needed.
FROM busybox:1.36-musl

COPY --chown=65532:65532 dist/live/vmlinuz /boot-artifacts/vmlinuz
COPY --chown=65532:65532 dist/live/initrd.img /boot-artifacts/initrd.img
COPY --chown=65532:65532 dist/live/filesystem.squashfs /boot-artifacts/filesystem.squashfs
COPY --chown=65532:65532 dist/live/shimx64.efi /boot-artifacts/shimx64.efi
COPY --chown=65532:65532 dist/live/grubx64.efi /boot-artifacts/grubx64.efi
COPY --chown=65532:65532 dist/live/kernel.config /boot-artifacts/kernel.config
COPY --chown=65532:65532 dist/live/manifest.json /boot-artifacts/manifest.json
COPY --chown=65532:65532 dist/live/sha256sums /boot-artifacts/sha256sums

USER 65532:65532
