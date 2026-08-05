# Opt-in, ipmitool-enabled build of the manager binary. This is the same
# manager as the repository's default Dockerfile - same source, same
# entrypoint - the only difference is the final base image: it ends on
# debian:stable-slim with the ipmitool package installed, instead of
# distroless/static, so internal/bmc/ipmi's driver can find and exec
# ipmitool on PATH.
#
# Use this image only if you have ipmi:// BMCs. redfish:// is the
# recommended path for hardware that supports it and needs no external
# binary at all (see internal/bmc/redfish); the default manager image
# (built from the repository's Dockerfile) stays on the smaller,
# dependency-free distroless base for that reason. ipmitool is a
# dynamically-linked C binary (glibc, libcrypto) that distroless/static
# cannot run, which is why this needs a separate, glibc-capable base
# rather than a build ARG on the default Dockerfile.
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

# debian:stable-slim (not distroless) so apt-get is available to install
# ipmitool and its shared-library closure in one step, without a
# hand-maintained ldd-driven copy into a distroless base.
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ipmitool \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
