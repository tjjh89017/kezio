# Build args:
#   TARGETOS, TARGETARCH - target platform for cross-compilation, set by buildx
ARG TARGETOS
ARG TARGETARCH

# kezio-bootd's dnsmasq-based proxyDHCP and in-process TFTP services
# (see internal/bootd). Ships separately from the manager: it's a
# standalone per-site binary needing privileged UDP ports and a Multus
# attachment onto the boot L2 segment (see config/bootd).
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# The whole api/ and internal/ trees are copied, not just the packages
# bootd currently imports: bootd's own imports change independently of
# this Dockerfile, and a hand-picked subset silently falls out of sync
# with them (as happened when the reverse proxy started importing
# internal/agentapi). Copying the full trees means the build context
# always has whatever bootd actually needs.
COPY cmd/bootd/ cmd/bootd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o bootd ./cmd/bootd

# debian:stable-slim rather than distroless: bootd supervises a dnsmasq
# child process (internal/bootd's Dnsmasq), so the image must carry the
# dnsmasq binary and its runtime libraries. dnsmasq-base is the DHCP/
# TFTP/DNS daemon alone, without the init scripts and resolvconf
# integration of the full dnsmasq package. The binary lands at
# /usr/sbin/dnsmasq (internal/bootd.DefaultDnsmasqPath).
FROM debian:stable-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends dnsmasq-base \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /workspace/bootd /bootd
# Root, unlike every other kezio image: Kubernetes grants a container's
# added capabilities to permitted/effective only for uid 0 - for a
# non-root uid they land in the bounding set alone, execve of a binary
# with no file capabilities clears permitted, and the pod's
# allowPrivilegeEscalation=false forbids regaining them via file
# capabilities. So a non-root bootd can never hand its dnsmasq child
# the NET_ADMIN/NET_RAW/NET_BIND_SERVICE it refuses to start without
# (see internal/bootd/caps.go). Confinement comes from the pod's
# securityContext instead: every capability dropped except those
# three, no privilege escalation, read-only root filesystem (see
# config/bootd/deployment.yaml).

ENTRYPOINT ["/bootd"]
