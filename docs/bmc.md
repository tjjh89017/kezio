# BMC drivers and the manager image

kezio powers and boots machines through a BMC (board management
controller). `internal/bmc` picks a driver from the scheme of the BMC
address you configure:

- `redfish://` (`internal/bmc/redfish`): talks Redfish over HTTPS using
  the pure-Go `gofish` library. It needs no external binary and works
  in the default manager image.
- `ipmi://` (`internal/bmc/ipmi`): shells out to the `ipmitool` binary
  for every call. It needs `ipmitool` on the manager container's PATH.

## Redfish is the recommended path

Use `redfish://` when the BMC supports it. It has no extra binary
dependency, so it works with the default manager image, and it is the
protocol modern BMC hardware prefers over IPMI.

## The default manager image is Redfish-only

The manager's container image (built from the repository's
`Dockerfile`) is `FROM gcr.io/distroless/static:nonroot`: a minimal
base with no package manager and no libc. `ipmitool` is a
dynamically-linked C binary (it needs glibc and libcrypto), so it
cannot be installed into, or run from, that image. Forcing every kezio
deployment onto a heavier, glibc-capable base just so the minority
that use IPMI get `ipmitool` was judged the wrong default; the default
image stays minimal.

If an `ipmi://` BMC is configured against the default manager image,
`internal/bmc/ipmi`'s driver returns a clear error the first time it
tries to run `ipmitool` and cannot find it on PATH, naming both
alternatives below.

## Using ipmi:// BMCs

If you have BMCs that only support IPMI, build and deploy the opt-in,
ipmitool-enabled manager image instead of the default one:

```sh
make docker-build-manager-ipmi   # builds Dockerfile.manager-ipmi, tag: $(IMG_IPMI)
make docker-push-manager-ipmi
```

`Dockerfile.manager-ipmi` builds the same manager binary as the
default `Dockerfile`, but finishes on `debian:stable-slim` with
`ipmitool` installed via `apt-get`, instead of on distroless. Deploy
the resulting image the same way as the default manager image (e.g.
`make deploy IMG=<your-ipmitool-enabled-tag>`), and `ipmi://` BMCs work
as normal from there.
