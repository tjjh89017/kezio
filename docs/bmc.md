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

## Powering a machine off: graceful first, forced if it is ignored

`spec.online: false` first asks the machine to shut down gracefully
(Redfish `GracefulShutdown`, IPMI's soft shutdown), so a machine
running a deployed OS closes its files instead of losing in-flight
writes.

A machine with no running OS - one sitting in its firmware setup or
boot menu, or with a hung kernel - has nothing to receive that
request. The BMC accepts it and the machine simply stays on. When the
machine keeps reporting itself powered on 5 minutes after the graceful
request, the controller escalates once to a forced power-off (Redfish
`ForceOff`, IPMI `chassis power off`), which the BMC carries out
itself. `spec.online: false` therefore takes effect even on a machine
that is wedged before any OS runs.

`afterDeploy: PowerOff` reaches the same path: it makes one graceful
request of its own at the end of the deployment and then sets
`spec.online: false`, so a machine that ignores that request is
escalated by the same rule.

## Redfish is the recommended path

Use `redfish://` when the BMC supports it. It is the protocol modern
BMC hardware prefers over IPMI. `redfish://` connects over HTTPS;
`redfishs://` is an accepted alias for the same behavior.

### `redfish+http://` is a lab-only escape hatch, not for production BMCs

`internal/bmc/redfish` also registers `redfish+http://`, which connects
over plain HTTP instead of HTTPS. It exists for lab or test Redfish
endpoints that have no TLS listener at all - for example, kezio's own
KubeVirtBMC-driven end-to-end lane uses it, because KubeVirtBMC's
generated Redfish Service does not terminate TLS. A real BMC's Redfish
endpoint is reachable over HTTPS; use `redfish://` for it. Reach for
`redfish+http://` only against a lab endpoint you know has no TLS
listener.

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
