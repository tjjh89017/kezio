# Reusing the KubeVirt BT-transfer e2e from the ezio repo

`e2e-kubevirt-reusable.yml` is a `workflow_call` GitHub Actions workflow
that stands up a KubeVirt + Multus cluster, ingests a real disk image,
seeds it with opentracker + an ezio seeder, net-boots a target VM, leeches
the image over BitTorrent onto the VM's disk, and verifies the deployed
disk actually boots (controller status, guest agent connection, and a
byte-for-byte content check through QGA).

kezio's own `e2e-deploy-kubevirt.yml` is a thin `workflow_dispatch`
caller of this workflow - the extraction only moved the job body, it did
not change kezio's own trigger, gating, or behavior. The [ezio
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
| `boot_artifacts_image` | string | `ghcr.io/tjjh89017/kezio-boot-artifacts:latest` | `kezio-boot-artifacts` OCI image the target VM net-boots (kernel/initrd/squashfs/shim/GRUB from `build-live-image.yml`). Ignored when `build_boot_artifacts` is true. |
| `build_boot_artifacts` | boolean | `false` | Build the live boot image (including `kezio-agent`) from this checkout instead of pulling `boot_artifacts_image` - the only way this job boots an agent built from the commit under test. Costs roughly the same many-minutes budget `build-live-image.yml` reserves for the identical script. |
| `manager_image` | string | `example.com/kezio:e2e-deploy` | Image tag built for the kezio controller-manager. |
| `bootd_image` | string | `example.com/kezio-bootd:e2e-deploy` | Image tag built for kezio-bootd (the network-boot server). |
| `ingest_image` | string | `example.com/kezio-ingest:e2e-deploy` | Image tag built for the image-ingest Job (qemu-img/sfdisk/partclone). |
| `seeder_image` | string | `example.com/kezio-seeder:e2e-deploy` | Image tag built for the ezio seeder; built from `ezio_ref`. |
| `image_service_image` | string | `example.com/kezio-image-service:e2e-deploy` | Image tag built for kezio-image-service (the upload/ingest front door). |
| `opentracker_image` | string | `ghcr.io/tunisiano187/opentracker-docker:master` | BitTorrent tracker image the seeder and leecher announce against. |

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
- **Dispatch gated on the kezio side.** kezio's own caller
  (`e2e-deploy-kubevirt.yml`) is `workflow_dispatch` only and depends on
  a published `kezio-boot-artifacts` image existing (see
  `boot_artifacts_image` above), unless `build_boot_artifacts` is set -
  it does not run on every push or pull request. An ezio-repo caller is
  free to run this reusable workflow on its own trigger (for example,
  every pull request), since the gating that applies is kezio's
  *caller* file, not the reusable workflow itself; the reusable
  workflow still needs a `kezio-boot-artifacts` image to exist (or
  `build_boot_artifacts: true`) for the boot to succeed.
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
