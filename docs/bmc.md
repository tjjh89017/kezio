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

## Powering a machine off: graceful first, forced if it is ignored

MachineSpec carries no power-intent field: there is no `spec.online`
switch, and the reboot annotation
(`kezio.kojuro.date/reboot[-<client>]`) is the only extra power lever a
client holds over an otherwise idle or provisioned machine (see
`api/v1alpha2/machine_types.go`'s own doc comment on MachineSpec). The
BMC-level graceful-then-forced power-off only runs as part of deleting
a Machine: `MachineReconciler`'s delete walk powers the machine off
(`AgentDeployer.PowerOff`) once deprovisioning finishes, before
releasing it. That step asks for a graceful shutdown (Redfish
`GracefulShutdown`, IPMI's soft shutdown) first, so a machine running a
deployed OS closes its files instead of losing in-flight writes, then
checks the BMC's own reported power state once and escalates
immediately to a forced power-off (Redfish `ForceOff`, IPMI `chassis
power off`) if it still reports on - a machine with no running OS (one
sitting in its firmware setup or boot menu, or with a hung kernel) has
nothing to receive the graceful request, so this escalation is what
actually powers it down.

`afterDeploy: PowerOff` is a different mechanism entirely and never
reaches the BMC: it only applies when a deployment finishes with no OS
image to reboot into (a dataImages-only deploy), and kezio-agent itself
runs the guest-side power-off (`systemctl poweroff`) from inside the
live environment at the end of the deploy, before it ever hands control
back to firmware. A machine whose guest never receives or acts on that
command stays on; only an operator-driven reboot annotation, or a
Machine deletion, reaches it through the BMC afterward.

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
