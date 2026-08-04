# Build the image-service binary. This is a separate image from the
# controller manager's Dockerfile: image-service mounts the staging
# volume and scales independently of the reconcile loop, so it ships and
# is deployed on its own (see config/image-service).
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/image-service/ cmd/image-service/
COPY internal/imageservice/ internal/imageservice/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o image-service ./cmd/image-service

# Use distroless as minimal base image to package the binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/image-service .
USER 65532:65532

ENTRYPOINT ["/image-service"]
