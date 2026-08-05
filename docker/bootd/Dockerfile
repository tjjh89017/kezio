# Build the bootd binary: kezio-bootd's proxyDHCP and TFTP services (see
# internal/bootd's package doc comment). This is a separate image from
# the controller manager's Dockerfile: bootd is not part of the manager
# process at all - it is a standalone, per-site binary that needs
# privileged UDP ports and a Multus attachment onto the boot L2 segment
# (see config/bootd), so it ships and is deployed on its own.
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

# Use distroless as minimal base image to package the binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bootd .
USER 65532:65532

ENTRYPOINT ["/bootd"]
