# kezio

> An earlier implementation of kezio lives on the
> [`legacy`](https://github.com/tjjh89017/kezio/tree/legacy) branch. This
> tree replaced it and is the one to use.

> **Proof of concept. Do not deploy machines you care about.**
>
> kezio is under active development and has not run anywhere but its own
> labs. The API is not stable: field names, defaults and whole resources
> change between releases with no migration path. Deploying an image
> overwrites the target disk, so a mistake here costs data rather than
> uptime.

**Start with [`docs/quick-start.md`](docs/quick-start.md).** It takes an
empty cluster and one machine to a deployed OS, one checked step at a
time.

## What it is

kezio is a Kubernetes operator that deploys an OS onto bare-metal
machines. It uses [ezio](https://github.com/tjjh89017/ezio) as the
deploy engine: disk partitions travel over BitTorrent, with partclone
images as payload, so many machines can be written at once from one
seeder.

kezio drives each machine through its BMC (Redfish or IPMI). It powers
the machine on, net boots it into a live agent, writes the disk, and
power-cycles it into the deployed OS. A `Machine` requires a BMC; kezio
has no mode that deploys a machine without one.

## How a deploy works

1. Upload a disk image with `kezioctl image upload`. An in-cluster Job
   slices every partition once with partclone and builds a `.torrent`
   per partition. The result is an immutable `Image` over one
   `PartitionContent` per partition.
2. Create a `Machine` (BMC address, credentials, boot MAC, and the
   `Subnet` it boots on). kezio net boots it once to inspect it; the
   agent reports its disks and NICs as a `MachineHardware`, and the
   `Machine` becomes `Available`.
3. Bind a `MachineClaim` to the `Machine`: which `Image`, which disk,
   which `PostHook`s. kezio starts a seeder for that `Image` at the
   machine's `Site`, net boots the machine again, and the agent writes
   every partition over BitTorrent, replays the partition table, runs
   the hooks, and sets the UEFI boot entry.
4. kezio power-cycles the machine into the deployed disk. The `Machine`
   is `Provisioned`; the seeder stops a few minutes after the last
   deploy of that `Image` finishes.

The objects behind this - `Site`, `Subnet`, `Machine`, `MachineHardware`,
`MachineClaim`, `Image`, `ImageImport`, `PartitionContent`, `DeployRun`,
`PostHook` - are described field by field in
[`docs/crd-reference.md`](docs/crd-reference.md).

## Documentation

- [`docs/quick-start.md`](docs/quick-start.md): the step-by-step
  bring-up from an empty cluster to one deployed machine.
- [`docs/crd-reference.md`](docs/crd-reference.md): every custom
  resource, how they relate, and the rules the schema enforces.
- [`docs/network-model.md`](docs/network-model.md): what a `Site`
  guarantees, data-plane-only `Subnet`s, the no-NAT rule, and the
  address-pool sizing rule.
- [`docs/image-boot-contract.md`](docs/image-boot-contract.md): what a
  deployable image must ship on its EFI System Partition.
- [`docs/bmc.md`](docs/bmc.md): the `redfish://` and `ipmi://` BMC
  drivers and how power actions are driven.
- [`docs/secure-boot.md`](docs/secure-boot.md): the UEFI Secure Boot
  signature chain of the net boot path.

## Releases and building

Each [GitHub Release](https://github.com/tjjh89017/kezio/releases)
ships the container images on ghcr.io, `install.yaml` (the controller
manager alone - the quick start builds the net boot overlay on top of
it), and prebuilt `kezioctl` binaries for Linux, macOS, and Windows.

To build from source (Go 1.26+):

```sh
make build      # compile the manager binary into bin/
make build-kezioctl
make test       # run unit tests
make lint       # run golangci-lint
```

`main.yaml` builds, lints, tests, and runs KubeVirt-based e2e lanes
(single subnet in both DHCP modes, routed multi-segment, two sites,
three concurrent machines) on every push and pull request. `release.yaml`
publishes the images and the release assets on `v*` tags.

## License

Apache-2.0. See [LICENSE](LICENSE).
