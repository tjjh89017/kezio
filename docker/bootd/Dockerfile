# Build args:
#   TARGETOS, TARGETARCH - target platform for cross-compilation, set by buildx
ARG TARGETOS
ARG TARGETARCH

# kezio-bootd's proxyDHCP and TFTP services (see internal/bootd). Ships
# separately from the manager: it's a standalone per-site binary needing
# privileged UDP ports and a Multus attachment onto the boot L2 segment
# (see config/bootd).
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

# distroless/static: minimal, no shell or package manager.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bootd .
USER 65532:65532

ENTRYPOINT ["/bootd"]
