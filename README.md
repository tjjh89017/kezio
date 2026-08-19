# kezio

> **This tree is being rebuilt from scratch.** The description below
> documents the previous implementation, which now lives on the
> [`legacy`](https://github.com/tjjh89017/kezio/tree/legacy) branch.
> This README is rewritten when the rebuild completes.

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

## How a deploy works

1. An ingest Job reads a source disk image. It writes one PVC per
   partition, and it writes an `ImageLayout` CR with the disk's
   `sfdisk` layout.
2. A publish step builds a `.torrent` file for each partition. Each
   `.torrent` file lives inside that partition's own content PVC.
3. When a `Machine` needs an `Image`, the operator starts one
   `ezio-seeder` Deployment for that `Image` at that site. The seeder
   stops after a grace period once no machine needs it.
4. The network-booted agent asks the operator for its deploy plan. It
   fetches each partition's `.torrent` over HTTP from the seeder pod.
   It leeches the partition content over BitTorrent. It writes each
   partition with partclone, replays the `sfdisk` layout, runs any
   `PostHook` steps, and points the UEFI boot entry at the new disk.
5. The operator power-cycles the machine through its BMC. The machine
   boots into the deployed disk.

## Custom resources

kezio defines these CRDs under the `kezio.kojuro.date/v1alpha1` group:

- **Image** — a source disk image to deploy. It tracks ingest state
  (pending, ingesting, ready, failed), the disk layout, and per-site
  seeder demand.
- **Machine** — one bare-metal machine. It holds the BMC address and
  credentials, the boot MAC address, the image to deploy, and the
  machine's own state (enrolling, inspecting, available, provisioning,
  provisioned).
- **ImageLayout** — the `sfdisk --json` dump for one `Image`, written
  once by the ingest Job.
- **PostHook** — a named, reusable sequence of steps (built-in actions,
  live-OS scripts, or chroot scripts) that a `Machine` or `Image` can
  reference to run after partclone writes the disk.

## Network boot

`bootd` runs one Deployment per network segment. It answers PXE
requests with proxyDHCP, and it serves the boot loader over TFTP.
The controller manager can serve boot configuration and live boot
artifacts (kernel, initrd, squashfs) over HTTP; `bootd` can proxy
these onward to the booting machine.

kezio supports two DHCP setups:

- **On-segment DHCP.** An existing DHCP server on the segment keeps
  handing out leases. `bootd`'s proxyDHCP answers PXE requests only,
  beside the existing DHCP server.
- **bootd-managed leases.** `bootd` becomes the segment's own DHCP
  server and hands out leases itself.

See [`docs/physical-lab-deployment.md`](docs/physical-lab-deployment.md)
for the full network setup, including how each scenario is configured.

## Getting started

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

## Documentation

- [`docs/lab-proxmox-rke2.md`](docs/lab-proxmox-rke2.md): a step-by-step
  walkthrough that builds one working lab, from an empty Proxmox VE host
  through RKE2, a Redfish shim in front of the Proxmox API, and a first
  deployed machine.
- [`docs/physical-lab-deployment.md`](docs/physical-lab-deployment.md):
  the manual guide for building a kezio lab on bare metal, including
  network and DHCP setup.
- [`docs/bmc.md`](docs/bmc.md): the `redfish://`, `ipmi://`, and
  `ipmitool://` BMC drivers, why `redfish://` and `ipmi://` both work
  in the default manager image, and how to build the opt-in
  ipmitool-enabled manager image for `ipmitool://` BMCs.
- [`docs/secure-boot.md`](docs/secure-boot.md): the UEFI Secure Boot
  signature chain the network boot path relies on, the kernel-signing
  story, and what CI does and does not verify.
- [`docs/e2e-kubevirt-reusable.md`](docs/e2e-kubevirt-reusable.md): how
  another repository (the ezio repository in particular) can reuse
  kezio's KubeVirt BT-transfer e2e steps by calling its composite
  actions under `.github/actions/` directly.
- [`docs/e2e-scale-multisite-kubevirt.md`](docs/e2e-scale-multisite-kubevirt.md):
  historical record of the retired multi-site scale e2e lane.

## Continuous integration

`main.yaml` builds, lints, tests, and runs e2e checks on every push to
`main` and on every pull request. `release.yaml` publishes container
images and boot artifacts on `v*` tags. A KubeVirt-based BMC e2e lane
verifies the full deploy chain, from ingest through BitTorrent leech
to a booted disk.

## License

Apache-2.0. See [LICENSE](LICENSE).
