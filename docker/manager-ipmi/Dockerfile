# Build args:
#   TARGETOS, TARGETARCH - target platform for cross-compilation, set by buildx
ARG TARGETOS
ARG TARGETARCH

# Same manager binary as the repository's default Dockerfile; only the
# final base image differs (debian:stable-slim + ipmitool, not
# distroless) so internal/bmc/ipmi's driver can exec ipmitool on PATH.
# Use only for ipmi:// BMCs - redfish:// needs no external binary and
# should use the default, smaller distroless image instead.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# debian:stable-slim (not distroless): apt-get installs ipmitool's glibc
# shared-library closure in one step, no hand-maintained ldd copying.
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ipmitool \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
