#!/bin/sh
# kezio-seeder entrypoint: a thin, decision-free translation from
# environment variables to ezio's own command-line flags (run `ezio
# --help` for the authoritative flag list). It holds no state and makes
# no choices of its own - the kezio operator is the only thing that
# decides what this seeder does, entirely through ezio's gRPC API
# (internal/seeder, proto/ezio.proto), not through this script or this
# container. Keep it that way: any decision logic belongs in the
# operator, not here.
set -eu

# -F: read partition content from regular files under save_path (the
# content's own PVC mount - see internal/ingest.ContentMountPath), not
# from a raw block device. The seeder always runs in this mode; kezio
# never gives ezio direct access to a target disk.
set -- -F --listen "${EZIO_GRPC_LISTEN:-0.0.0.0:50051}"

if [ -n "${EZIO_BT_PORT:-}" ]; then
	set -- "$@" --port "${EZIO_BT_PORT}"
fi
# EZIO_CACHE_SIZE_MB and EZIO_AIO_THREADS only affect ezio's raw-disk
# mode (its raw_disk_io, where ezio manages the block cache and I/O
# threads itself). This seeder always runs in -F file mode (see above),
# where libtorrent's own default mmap-backed disk I/O handles caching -
# neither flag has an effect there, so setting either env var on this
# seeder is a silent no-op, not a tuning knob.
if [ -n "${EZIO_CACHE_SIZE_MB:-}" ]; then
	set -- "$@" --cache-size "${EZIO_CACHE_SIZE_MB}"
fi
if [ -n "${EZIO_AIO_THREADS:-}" ]; then
	set -- "$@" --aio-threads "${EZIO_AIO_THREADS}"
fi
if [ "${EZIO_SLOW_START:-}" = "true" ]; then
	set -- "$@" --slow-start
	if [ -n "${EZIO_SLOW_START_PERIOD:-}" ]; then
		set -- "$@" --slow-start-period "${EZIO_SLOW_START_PERIOD}"
	fi
fi

exec /app/ezio "$@"
