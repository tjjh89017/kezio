# Ships upstream ezio, a minimal decision-free entrypoint, and
# kezio-seeder-register.
#
# ezio itself stays driven entirely through its own gRPC API
# (internal/seeder, proto/ezio.proto), and the entrypoint still makes no
# decisions. The register binary is a SEPARATE container in the same pod
# (the seeder Deployment overrides the command), not part of ezio's
# process: a content's .torrent lives in that content's own PVC, which
# only this pod mounts, so nothing outside the pod can read it and hand
# it to ezio.
#
# ezio comes from the DRBL apt repository - same source and key as
# hack/live-image's live boot image (see that image's
# config/archives/drbl.* for the rest of the story: DRBL's own CI tracks
# upstream ezio releases and rebuilds daily against sid). No compiler, no
# cmake, no ldd closure-staging: apt resolves the shared-library
# dependencies in one transaction against this stage's own sid snapshot.

# Cross-compiled the same way as every other kezio Go image.
FROM --platform=$BUILDPLATFORM golang:1.26 AS go-builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/seeder/ cmd/seeder/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o kezio-seeder-register ./cmd/seeder

FROM debian:sid

# Plain http, not https: the base image's ca-certificates may be stale
# relative to today's snapshot; the GPG key below is what actually
# authenticates this repo, so TLS was never load-bearing.
COPY hack/live-image/config/archives/drbl.key.chroot /etc/apt/trusted.gpg.d/drbl.asc
RUN echo "deb http://free.nchc.org.tw/drbl-core drbl live-sid" >/etc/apt/sources.list.d/drbl.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends ezio \
    && rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/drbl.list \
    && mkdir -p /app && cp /usr/sbin/ezio /app/ezio

COPY --from=go-builder /workspace/kezio-seeder-register /usr/local/bin/kezio-seeder-register
COPY docker/seeder/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Matches every other kezio image: the deployment manifests set
# runAsNonRoot: true, which the kubelet enforces at image level.
USER 65532:65532

WORKDIR /app
ENTRYPOINT ["/app/entrypoint.sh"]
