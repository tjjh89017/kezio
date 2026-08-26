# Build args:
#   TARGETOS, TARGETARCH - target platform for cross-compilation, set by buildx
ARG TARGETOS
ARG TARGETARCH

# kezio-ingest runs as a Job pod, not a long-lived Deployment, and needs
# partclone/qemu-img/qemu-nbd/sfdisk/blkid on PATH - unlike
# manager/image-service. By default (IMAGE_INGEST_ATTACH=nbd, see
# internal/controller's Job builder) it attaches its source image with
# qemu-nbd and needs the privilege that comes with - a privileged pod
# with the node's /dev, granted by the Job's securityContext, not by
# anything in this image. IMAGE_INGEST_ATTACH=copy (internal/ingest's doc comment)
# only reads/writes plain files, never a block device, and needs none of
# that.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ingest/ cmd/ingest/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o kezio-ingest ./cmd/ingest

# debian:sid (not stable) tracks current partclone releases; stable lags
# too far behind for the file systems this pipeline needs to clone.
FROM debian:sid-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        partclone \
        qemu-utils \
        util-linux \
        fdisk \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /workspace/kezio-ingest /usr/local/bin/kezio-ingest

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/kezio-ingest"]
