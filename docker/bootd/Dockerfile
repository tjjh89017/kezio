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

COPY cmd/bootd/ cmd/bootd/
COPY api/ api/
COPY internal/bootd/ internal/bootd/
COPY internal/bootserver/ internal/bootserver/

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
# Same non-root uid:gid as every other kezio image (distroless's
# "nonroot"). dnsmasq runs as this user too - it is exec'd by bootd,
# never setuid - with the capabilities config/bootd's deployment grants
# (see internal/bootd's caps.go).
USER 65532:65532

ENTRYPOINT ["/bootd"]
