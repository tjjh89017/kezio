# Build args:
#   TARGETOS, TARGETARCH - target platform for cross-compilation, set by buildx
ARG TARGETOS
ARG TARGETARCH

# image-service mounts the staging volume and scales independently of the
# reconcile loop, so it ships separately (see config/image-service). Unlike
# ingest it shells out to nothing - plain net/http and filesystem calls
# (internal/imageservice) - so the runtime stage needs no apt packages,
# only the static binary.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/image-service/ cmd/image-service/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o image-service ./cmd/image-service

# distroless/static: no shell, no package manager, smallest base that can
# still run a CGO_ENABLED=0 binary - nothing here needs more.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/image-service .
USER 65532:65532

ENTRYPOINT ["/image-service"]
