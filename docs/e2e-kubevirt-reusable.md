# Reusing the KubeVirt BT-transfer e2e from the ezio repo

`e2e-kubevirt-reusable.yml` is a `workflow_call` GitHub Actions workflow
that stands up a KubeVirt + Multus cluster, ingests a real disk image,
seeds it with opentracker + an ezio seeder, net-boots a target VM, leeches
the image over BitTorrent onto the VM's disk, and verifies the deployed
disk actually boots (controller status, guest agent connection, and a
byte-for-byte content check through QGA).

kezio's own `main.yaml` calls this workflow directly as its `e2e-bmc`
job, downloading the `boot-artifacts` job's own workflow artifact
(`boot_artifacts_artifact`) rather than rebuilding the live boot image
in-lane or pulling a published ghcr image - see `boot_artifacts_artifact`
below. The target VM is powered on and PXE-booted through a real
KubeVirtBMC/Redfish endpoint (kezio's own no-BMC lane is retired - kezio
never ships without a BMC driver). The [ezio
repository](https://github.com/tjjh89017/ezio) - which owns the
BitTorrent engine this scenario seeds and leeches through, and ships
build-only CI with no test suite of its own - can call the same workflow
directly to get a real integration test against a revision under test.

## Minimal example

```yaml
# .github/workflows/e2e.yml, in the ezio repository
on:
  pull_request:

jobs:
  e2e:
    uses: tjjh89017/kezio/.github/workflows/e2e-kubevirt-reusable.yml@main
    with:
      ezio_ref: ${{ github.event.pull_request.head.sha }}
```

## Inputs

| Input | Type | Default | Purpose |
|---|---|---|---|
| `ezio_ref` | string | `v2.0.29` | ezio git revision (tag, branch, or commit SHA) that `docker/seeder/Dockerfile` builds into the seeder image. This is the one input an ezio-repo caller needs to set - to the pull request's head SHA, a branch, or any other revision under test. Default mirrors `docker/seeder/Dockerfile`'s own `EZIO_REF` build arg, so kezio's own caller is unaffected. |
| `boot_artifacts_image` | string | `ghcr.io/tjjh89017/kezio-boot-artifacts:latest` | Published `kezio-boot-artifacts` OCI image the target VM net-boots (kernel/initrd/squashfs/shim/GRUB), pulled by the kubelet directly. Ignored when `boot_artifacts_artifact` is set or `build_boot_artifacts` is true. |
| `boot_artifacts_artifact` | string | `""` | Name of a workflow artifact - produced by an upstream job in the SAME workflow run (kezio's own `main.yaml` "Boot artifacts" job) with `actions/upload-artifact` - to download and package into the `kezio-boot-artifacts` image this job boots, instead of building it in-lane or pulling a published image. This is how `main.yaml`'s `e2e-bmc` job reuses the live boot image the graph's `boot-artifacts` job already built, exactly once. Empty disables this mode. |
| `build_boot_artifacts` | boolean | `false` | Build the live boot image (including `kezio-agent`) from this checkout instead of consuming `boot_artifacts_artifact` or pulling `boot_artifacts_image` - the way to boot an agent built from the commit under test when no upstream job's artifact is available. Costs roughly the same many-minutes budget kezio's own `main.yaml` "Boot artifacts" job reserves for the identical script. Ignored when `boot_artifacts_artifact` is set. |
| `manager_image` | string | `example.com/kezio:e2e-deploy` | Image tag built for the kezio controller-manager. |
| `bootd_image` | string | `example.com/kezio-bootd:e2e-deploy` | Image tag built for kezio-bootd (the network-boot server). |
| `ingest_image` | string | `example.com/kezio-ingest:e2e-deploy` | Image tag built for the image-ingest Job (qemu-img/sfdisk/partclone). |
| `seeder_image` | string | `example.com/kezio-seeder:e2e-deploy` | Image tag built for the ezio seeder; built from `ezio_ref`. |
| `image_service_image` | string | `example.com/kezio-image-service:e2e-deploy` | Image tag built for kezio-image-service (the upload/ingest front door). |
| `opentracker_image` | string | `ghcr.io/tunisiano187/opentracker-docker:master` | BitTorrent tracker image the seeder and leecher announce against. |
| `kubevirtbmc_version` | string | `v0.9.0` | KubeVirtBMC release tag to install. |
| `dhcp_scenario` | choice | `no-relay` | Which of kezio-bootd's DHCP scenarios this run drives: `no-relay` (proxyDHCP beside an existing-dhcp stand-in server) or `lease` (bootd's own dnsmasq is the segment's sole DHCP authority). |

The three boot-artifacts inputs are mutually exclusive; when more than
one is set, `boot_artifacts_artifact` wins over `build_boot_artifacts`,
which wins over `boot_artifacts_image`. A `boot_artifacts_artifact`
download that turns out empty fails the job immediately with an
`::error::`, before any KVM/cluster setup cost is paid.

No `secrets:` passthrough exists today: every image the job builds is
built from source and loaded locally (never pushed), and every external
pull (the Ubuntu cloud test image, the opentracker image, ezio's own git
clone) is public. A future caller that needs private-registry auth would
add a `secrets:` block to the reusable workflow at that point - none
exists because nothing in the job needs one yet.

## Caveats

- **Needs a KubeVirt-capable runner.** The job fails fast if `/dev/kvm`
  is not usable - it never falls back to software emulation. A hosted
  `ubuntu-latest` GitHub-hosted runner has nested virtualization
  available; a self-hosted runner needs `/dev/kvm` and enough CPU/RAM
  (the job's own resource-check step fails fast under 4 CPUs or 14000
  MiB RAM).
- **Gated on the kezio side.** kezio's own caller is `main.yaml`'s
  `e2e-bmc` job, which runs on every push and pull request (and on
  `workflow_dispatch` when `run_bmc_e2e` is set) and always passes
  `boot_artifacts_artifact`, so it needs the same run's `boot-artifacts`
  job to have actually produced that artifact (an explicit
  `needs.boot-artifacts.result == 'success'` gate skips `e2e-bmc`
  otherwise, rather than letting it fail on a missing download). An
  ezio-repo caller is free to run this reusable workflow on its own
  trigger (for example, every pull request), since the gating that
  applies is kezio's *caller* file, not the reusable workflow itself;
  such a caller has no upstream `boot-artifacts` job to hand off from,
  so it would use `build_boot_artifacts: true` or a published
  `boot_artifacts_image` instead.
- **A stale agent never passes silently.** The job's own "Assert the
  boot artifacts agent commit" step compares the booted image's
  `manifest.json` `agentCommit` against the commit under test and
  records the result (FRESH, MISMATCH, or STALE/PINNED) in the job
  summary - a `boot_artifacts_image`-pinned run is clearly labeled as
  not exercising the commit under test's own agent, and a
  `build_boot_artifacts` run that somehow still mismatches fails the
  job outright.
- **Long-running.** Budget close to 90 minutes: the job builds five
  images from source (including a from-source `debian:sid` ezio build),
  downloads and customizes a cloud test image, brings up a full
  KubeVirt/CDI/Multus stack, and drives a real PXE boot, BitTorrent
  leech, in-guest reboot, and boot-from-disk verification.
