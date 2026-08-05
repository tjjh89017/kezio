# Build the kezio-ingest binary. This is a separate image from the
# controller manager's and image-service's Dockerfiles: it runs as a Job
# pod, not a long-lived Deployment, and needs partclone/qemu-img/sfdisk/
# blkid on its PATH, which the manager and image-service images do not.
#
# kezio-ingest itself needs no elevated privilege: every tool below reads
# and writes plain files (a downloaded/staged source, a converted raw
# disk, per-partition slice files this binary extracts with a plain file
# copy), never a block device - see internal/ingest's package doc
# comment and cmd/ingest/main.go for the full reasoning. The final image
# runs as a non-root user and needs no added Linux capabilities.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ingest/ cmd/ingest/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o kezio-ingest ./cmd/ingest

# Runtime image: partclone and its file-system-specific binaries, plus
# qemu-img (qemu-utils) and sfdisk/blkid (util-linux, split into the
# fdisk package for sfdisk on current Debian). Debian sid tracks current
# partclone releases; a stable release lags too far behind for the file
# systems this pipeline needs to clone.
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
