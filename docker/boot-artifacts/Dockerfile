# Packages the live boot artifacts hack/live-image/build.sh produces
# (vmlinuz, initrd.img, filesystem.squashfs, shimx64.efi, grubx64.efi,
# manifest.json) into an OCI image, so config/bootserver's and
# config/bootd's fetch-boot-artifacts initContainers can populate their
# shared emptyDir with a plain `cp` instead of a runtime curl against a
# GitHub Release (see .github/workflows/build-live-image.yml for the
# publishing side of this and why an OCI image replaces that runtime
# fetch). Build context is the repository root; every COPY source below
# comes from dist/live/, the same directory build-live-image.yml already
# uploads as release assets.
#
# busybox, not distroless/scratch: the whole point of this image is to
# be `cp`-able by an initContainer with no extra tooling, and busybox's
# own multi-call binary already provides cp with no package manager or
# shell surface beyond what that one command needs. USER 65532:65532
# (a numeric uid/gid, the same convention docker/ingest/Dockerfile and
# docker/image-service/Dockerfile use) is required, not cosmetic:
# restricted Pod Security Admission cannot resolve a non-numeric image
# USER to verify runAsNonRoot (curlimages/curl's "curl_user" hit exactly
# this - see config/bootserver/boot-artifacts-init-patch.yaml's history),
# so every consumer's initContainer securityContext can now set
# runAsNonRoot: true with no runAsUser/runAsGroup override needed.
FROM busybox:1.36-musl

COPY --chown=65532:65532 dist/live/vmlinuz /boot-artifacts/vmlinuz
COPY --chown=65532:65532 dist/live/initrd.img /boot-artifacts/initrd.img
COPY --chown=65532:65532 dist/live/filesystem.squashfs /boot-artifacts/filesystem.squashfs
COPY --chown=65532:65532 dist/live/shimx64.efi /boot-artifacts/shimx64.efi
COPY --chown=65532:65532 dist/live/grubx64.efi /boot-artifacts/grubx64.efi
COPY --chown=65532:65532 dist/live/manifest.json /boot-artifacts/manifest.json

USER 65532:65532
