# Build args:
#   EZIO_REF - ezio git ref (tag/branch/SHA) to build, e.g.
#     docker build --build-arg EZIO_REF=master -f docker/seeder/Dockerfile .
ARG EZIO_REF=v2.0.29

# Ships upstream ezio, a minimal decision-free entrypoint, and
# kezio-seeder-register.
#
# ezio itself stays driven entirely through its own gRPC API
# (internal/seeder, proto/ezio.proto), and the entrypoint still makes no
# decisions. The register binary is a SEPARATE container in the same pod
# (the Image reconciler overrides the command - see buildSeederDeployment),
# not part of ezio's process: a partition's .torrent lives in that
# partition's PVC, which only this pod mounts, so nothing outside the pod
# can read it and hand it to ezio.
#
# Adapted from ezio's own Dockerfile: that one does `COPY . /usr/src/ezio`
# from its own checkout; this one clones upstream instead, since kezio's
# build context is this repository, not ezio's. Package lists otherwise
# match upstream, including tracking debian:sid unpinned. No
# BUILDPLATFORM/TARGETPLATFORM cross-compilation like the Go Dockerfiles:
# cmake's C++ build has no equivalent to `GOARCH=... go build`; buildx runs
# this natively per platform under QEMU instead.
FROM debian:sid AS builder
ARG EZIO_REF

RUN apt-get -y update && \
    apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    build-essential \
    libssl-dev \
    cmake \
    make \
    libgrpc-dev \
    libgrpc++-dev \
    libprotobuf-dev \
    protobuf-compiler-grpc \
    libtorrent-rasterbar-dev \
    libboost-program-options-dev \
    libspdlog-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src/ezio
RUN git clone https://github.com/tjjh89017/ezio.git . \
    && git checkout --detach "${EZIO_REF}"

RUN cmake . && make && make install

# Resolve ezio's full shared-library closure in this same builder stage, in
# the same apt transaction that installed the -dev packages it linked
# against, and stage each file at its real absolute path under /out. A
# second, independent apt-get in the runtime stage below can resolve a
# newer debian:sid snapshot with a bumped soname (this is how libfmt.so.10
# once went missing); copying the resolved closure avoids that permanently.
# Each dependency is staged twice: once at its ldd-reported path (`cp -a`
# preserves a soname symlink like libfmt.so.10 -> libfmt.so.10.1.0 instead
# of dereferencing it - the dynamic linker looks up the exact soname), and
# once at its fully-resolved real path. Canonicalizing the directory
# component (not the final symlink) also turns /lib64/ld-linux-*.so.2 into
# /usr/lib64/ld-linux-*.so.2: debian aliases /lib* to /usr/lib* via
# top-level symlinks, and COPY-ing a real /out/lib64 over that symlink in
# the runtime stage fails outright.
RUN mkdir -p /out && \
    ldd /usr/local/sbin/ezio | grep -oE '/[^ ]+' | sort -u | while read -r f; do \
        d="$(realpath "$(dirname "$f")")/$(basename "$f")"; \
        cp -a --parents "$d" /out; \
        r="$(realpath "$f")"; \
        [ "$r" = "$d" ] || cp --parents "$r" /out; \
    done

# Cross-compiled the same way as every other kezio Go image; kept in its
# own stage so the C++ build above is untouched by it.
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

# No apt-get here: every shared library ezio needs is copied from the
# builder's /out instead, so this stage can never drift from what ezio was
# actually linked against.
COPY --from=builder /out/ /
COPY --from=builder /usr/local/sbin/ezio /app/ezio
COPY --from=go-builder /workspace/kezio-seeder-register /usr/local/bin/kezio-seeder-register
COPY docker/seeder/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Matches every other kezio image: the deployment manifests set
# runAsNonRoot: true, which the kubelet enforces at image level.
USER 65532:65532

WORKDIR /app
ENTRYPOINT ["/app/entrypoint.sh"]
