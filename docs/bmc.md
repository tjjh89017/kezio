# BMC drivers and the manager image

kezio powers and boots machines through a BMC (board management
controller). `internal/bmc` picks a driver from the scheme of the BMC
address you configure:

- `redfish://` (`internal/bmc/redfish`): talks Redfish over HTTPS using
  the pure-Go `gofish` library. It needs no external binary and works
  in the default manager image.
- `ipmi://` (`internal/bmc/ipmi`): talks IPMI directly using the
  pure-Go `bougou/go-ipmi` library. It needs no external binary and
  works in the default manager image.
- `ipmitool://` (`internal/bmc/ipmitool`): shells out to the
  `ipmitool` binary for every call. It needs `ipmitool` on the manager
  container's PATH, so it only works with the opt-in ipmitool-enabled
  manager image (see below).

## Redfish is the recommended path

Use `redfish://` when the BMC supports it. It has no extra binary
dependency, so it works with the default manager image, and it is the
protocol modern BMC hardware prefers over IPMI.

## ipmi:// is the recommended path for IPMI-only BMCs

Use `ipmi://` for a BMC that only supports IPMI. Like `redfish://`, it
has no extra binary dependency and works with the default manager
image: `internal/bmc/ipmi` opens an IPMI 2.0/RMCP+ session directly
over the network using `bougou/go-ipmi`, without shelling out to any
external tool.

## Using ipmitool:// as a fallback

`ipmitool://` exists as an operator-selectable escape hatch, not the
default IPMI path: IPMI's long history of inconsistent vendor firmware
means a specific BMC can occasionally misbehave against the pure-Go
`ipmi://` driver while working fine against `ipmitool`'s
battle-tested reference implementation. Reach for `ipmitool://` only
when a specific BMC demonstrably needs it.

`ipmitool://` requires the opt-in, ipmitool-enabled manager image
instead of the default one:

```sh
make docker-build-manager-ipmi   # builds docker/manager-ipmi/Dockerfile, tag: $(IMG_IPMI)
make docker-push-manager-ipmi
```

`docker/manager-ipmi/Dockerfile` builds the same manager binary as the
default `Dockerfile`, but finishes on `debian:stable-slim` with
`ipmitool` installed via `apt-get`, instead of on distroless. Deploy
the resulting image the same way as the default manager image (e.g.
`make deploy IMG=<your-ipmitool-enabled-tag>`), and `ipmitool://` BMCs
work as normal from there.

If an `ipmitool://` BMC is configured against the default manager
image, `internal/bmc/ipmitool`'s driver returns a clear error the
first time it tries to run `ipmitool` and cannot find it on PATH,
naming both `ipmi://` and `redfish://` as alternatives that already
work in that image, alongside the ipmitool-enabled image if
`ipmitool://` itself is genuinely required.
