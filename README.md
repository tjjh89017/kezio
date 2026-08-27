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
>
> Expect to hit problems the documentation does not cover, and to read
> the code when you do.

kezio is a Kubernetes operator. It orchestrates bare-metal OS deployment.
It uses [ezio](https://github.com/tjjh89017/ezio) as the deploy engine.
ezio writes disk partitions over BitTorrent, with partclone images as
payload.

kezio drives each machine's BMC. The operator powers the machine on,
boots it into the network agent, and power-cycles it into the deployed
disk. A `Machine` requires a BMC address and credentials; kezio has no
mode that deploys a machine without one.

## Custom resources

kezio defines these CRDs under the `kezio.kojuro.date/v1alpha3` group:

| Kind | Purpose |
|---|---|
| `Site` | A maximal routable domain: every `Subnet` in one `Site` can reach every other. Owns this Site's tracker (a pinned IP, or a reference to one the operator already runs). |
| `Subnet` | One broadcast domain. Carries an optional boot half (bootd's address, its network attachment, and DHCP mode) and an optional seeder network reference. A `Subnet` names its `Site` through `spec.siteRef`. In lease mode it also holds the boot-scoped DHCP reservation table in `status.dhcp.reservations`. |
| `Machine` | One bare-metal machine: BMC address and credentials, boot MAC, and the `Subnet` it network boots through. It carries no deploy intent of its own; its own state moves through Enrolling, Inspecting, Available, Provisioning, Provisioned. |
| `MachineHardware` | The disk/NIC/CPU/memory inventory a `Machine` reports at inspection. |
| `MachineClaim` | The deploy intent for one `Machine`: which `Image`, which data images, which disk, which hooks, and per-machine ezio tuning. Binding a claim to a `Machine` (by `spec.machineName` or `spec.selector`) is what starts a deploy. |
| `Image` | A disk layout: an ordered list of partition slots, each optionally bound to a `PartitionContent`. Immutable once created, and always created over content that already exists. |
| `ImageImport` | One request to turn a source disk image into content plus the `Image` that binds it. It runs partclone exactly once, in the cluster, then creates one `PartitionContent` per non-swap partition and the `Image` over them. |
| `PartitionContent` | One partition's immutable data set (a partclone data set plus its torrent). Named by the user: an import names partition N `<spec.contentPrefix>-p<N>`. Its BitTorrent info hash is reported in `status.infoHash` once it publishes. |
| `DeployRun` | The resolved, immutable snapshot of one deployment attempt: which images, which disks, which hooks, and its phase (Partitioning, WritingContent, RunningPostHook, Finalizing, ...). |
| `PostHook` | A named, reusable, ordered sequence of steps (builtins or scripts) that a `MachineClaim` or `Image` can attach to run after content is written. A script step runs in the live environment, not in the deployed OS: no deployed file system is mounted for it, the plan's device paths come to it in the environment (`KEZIO_TARGET_DISK`, `KEZIO_PARTITIONS`, `KEZIO_PART_<number>`, and the `KEZIO_DATA_DISK_*` set), and a script that mounts a device must unmount it before it ends. |

See [`docs/crd-reference.md`](docs/crd-reference.md) for every field of
each kind, the references between them, and the rules the schema
enforces.

## How a deploy works

A `Machine` moves through Enrolling, Inspecting, Available,
Provisioning, and Provisioned. Enroll and inspect need no deploy
intent; binding a `MachineClaim` is what starts a provision.

1. An `ImageImport`'s ingest Job reads a source disk image, slices every
   partition once with partclone, and reports the disk layout it found.
   The operator then creates one immutable `PartitionContent` per
   non-swap partition and the `Image` whose layout binds them. An import
   fails rather than write over a name that already exists.
2. A publish step builds a `.torrent` file for each `PartitionContent`.
3. Each time the operator arms a net boot (for inspect or for provision),
   it powers the machine on and, on a lease-mode `Subnet`, reserves a
   fixed DHCP address for it in `status.dhcp.reservations`. The
   reservation releases again once that step completes, the `Machine` is
   deleted, or its boot MAC or `Subnet` changes.
4. A `MachineClaim` bound to the `Machine` carries the deploy intent -
   which `Image`, which data images, which disk, which hooks. Binding
   it starts a provision.
5. When a `Machine` needs an `Image`, the operator starts one seeder
   Deployment for that `Image` at that `Machine`'s `Site` - one process
   serving every `PartitionContent` the `Image` references. The seeder
   stops after a grace period once no machine at that `Site` needs it.
6. The network-booted agent asks the operator for its deploy plan. It
   fetches each partition's `.torrent` over HTTP from the seeder pod,
   leeches the content over BitTorrent, writes each partition with
   partclone, replays the disk layout, runs any `PostHook` steps, and
   points the UEFI boot entry at the new disk.
7. The operator power-cycles the machine through its BMC. The machine
   boots into the deployed disk. The DHCP reservation from step 3
   releases now that the provision has completed.

## Network boot

`bootd` runs one Deployment per `Subnet` that declares a boot half. It
answers PXE requests with proxyDHCP (or, on an isolated segment, becomes
that segment's own DHCP lease authority), and it serves the boot loader
over TFTP. The controller manager serves boot configuration and live
boot artifacts (kernel, initrd, squashfs) over HTTP; `bootd` proxies
these onward to the booting machine.

See [`docs/network-model.md`](docs/network-model.md) for what a `Site`
guarantees, how the boot and data planes split across `Subnet` objects,
and the tracker/seeder connectivity rules. See
[`docs/physical-lab-deployment.md`](docs/physical-lab-deployment.md) for
the full operational setup, including both DHCP scenarios.

## Getting started

### Install a release

Each [GitHub Release](https://github.com/tjjh89017/kezio/releases) ships
an `install.yaml` asset. It carries the CRDs, RBAC, and the controller
manager Deployment (`config/default`, with cert-manager as a
prerequisite for the webhooks):

```sh
kubectl apply -f https://github.com/tjjh89017/kezio/releases/download/v0.3.8/install.yaml
```

This alone does not network boot a machine. Also apply
`config/bootd` for the `bootd` ServiceAccount and RBAC that every
bootd Deployment needs:

```sh
kustomize build config/bootd | kubectl apply -f -
```

See [`docs/physical-lab-deployment.md`](docs/physical-lab-deployment.md)
for the rest of the bring-up: the boot-server and agent-server
components, the namespace's Pod Security Admission label, and the
Site/Subnet objects a network boot needs.

### Build from source

Prerequisites:

- [Go](https://go.dev/) 1.26+
- [operator-sdk](https://sdk.operatorframework.io/)
- Access to a Kubernetes cluster (e.g. via `kubectl`)

Install the CRDs and deploy the controller manager:

```sh
make install   # install the CRDs into the current cluster
make deploy    # deploy the controller manager
```

Other useful targets:

```sh
make build      # compile the manager binary into bin/
make manifests  # generate CRD/RBAC/webhook manifests
make generate   # generate DeepCopy/runtime.Object code
make test       # run unit tests
make lint       # run golangci-lint
```

See `make help` for the full list of targets.

`kezioctl`, the command-line client, can also be built on its own with
`make build-kezioctl` into `bin/kezioctl`. Each [GitHub
Release](https://github.com/tjjh89017/kezio/releases) also ships prebuilt
`kezioctl` binaries for Linux, macOS, and Windows (amd64/arm64, Windows
amd64 only) as `kezioctl-<os>-<arch>` archives; verify a download against
the release's `kezioctl-SHA256SUMS` file before running it.

## Documentation

- [`docs/crd-reference.md`](docs/crd-reference.md): every custom
  resource and how they relate - the network model, the content
  model, and the machine and deploy model, with the rules that are
  easiest to get wrong.
- [`docs/network-model.md`](docs/network-model.md): what a `Site`
  guarantees and does not, data-plane-only `Subnet`s, the no-NAT rule,
  the address-pool sizing rule, and why the tracker is scoped per `Site`.
- [`docs/lab-proxmox-rke2.md`](docs/lab-proxmox-rke2.md): a step-by-step
  walkthrough that builds one working lab, from an empty Proxmox VE host
  through RKE2, a Redfish shim in front of the Proxmox API, and a first
  deployed machine.
- [`docs/physical-lab-deployment.md`](docs/physical-lab-deployment.md):
  the manual guide for building a kezio lab on bare metal, including
  network and DHCP setup, and the routed multi-subnet case.
- [`docs/bmc.md`](docs/bmc.md): the `redfish://` and `ipmi://` BMC
  drivers, why both work in the default manager image, and how the
  graceful-then-forced power-off and the reboot annotation each drive
  the BMC.
- [`docs/secure-boot.md`](docs/secure-boot.md): the UEFI Secure Boot
  signature chain the network boot path relies on, the kernel-signing
  story, and what CI does and does not verify.

## Continuous integration

`main.yaml` builds, lints, tests, and runs e2e checks on every push to
`main` and on every pull request. `release.yaml` publishes container
images and boot artifacts on `v*` tags. Several KubeVirt-based e2e lanes
verify the deploy chain end to end, from ingest through BitTorrent leech
to a booted disk, including a routed multi-segment lane, a two-site
concurrent lane, and a three-machine concurrent lane.

## License

Apache-2.0. See [LICENSE](LICENSE).
