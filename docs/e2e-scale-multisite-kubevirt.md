# Multi-site scale e2e (`e2e-scale-multisite-kubevirt.yml`)

A `workflow_dispatch`-only, release-gated GitHub Actions lane that proves
kezio's per-site seeder topology (`config/seeder/README.md`'s "Per-site
seeders" section) actually works end to end: two simulated sites, each
with its own leecher VM, deploying the same Image concurrently through a
swarm that has more than one seeder endpoint.

## What it proves

- A central `ezio-seeder` replica already holds the full ingested Image
  (kezio's Option A storage design mounts the store PVC directly - no
  BitTorrent leech is needed to "warm" it) plus one additional site-local
  `ezio-seeder` replica per simulated site, all as endpoints of the same
  Kubernetes Service, all seeding the same content - exercising
  `SeederReconciler`'s "every Ready endpoint of one Service" sync path
  (`internal/controller/seeder_controller.go`) with more than one
  endpoint for real, not as a unit-test fixture.
- Two target VMs, one per simulated site, each behind its own
  `kezio-bootd` instance on its own Multus bridge, PXE-boot and deploy
  the same Image **concurrently** - real ingest (qemu-img/sfdisk/
  partclone), a real BitTorrent leech, a real in-guest reboot, and a
  real boot-from-disk check (the same three-layer verification
  `e2e-kubevirt-reusable.yml` uses: controller status, guest-agent
  connection, and a byte-for-byte marker-file read back through QGA) -
  run for both VMs independently.
- While both leeches are in flight, the workflow samples ezio's own
  `GetTorrentStatus` gRPC call against all three seeder endpoints and
  reports the highest peer count observed, as supporting (not gating)
  evidence that more than one peer was actually connected to the
  torrent at once.

## What it deliberately does NOT claim

A single GitHub-hosted runner has one flat pod network - every seeder,
the tracker, and both leecher VMs' BitTorrent traffic cross the same
loopback/veth fabric, with no real inter-site latency, bandwidth cap, or
routing hop between them. This lane therefore does **not** claim:

- that the WAN carries close to one copy of the image - that needs real
  per-network traffic accounting, which this topology cannot produce;
- that same-site peers preferentially exchange pieces over a cheaper
  local path - BitTorrent's own peer selection has no real cost
  difference to respond to here;
- that the two site-local seeder replicas are storage-isolated from the
  central one - all three mount the identical store PVC, because every
  pod in this lane runs on one k3s node with one storage backend.

The workflow's own header comment and its final job-summary step both
restate this scope explicitly, so a run's own output does not need this
document to be understood correctly.

## The 2-site simulation

"Site" here means a separate boot L2 segment, genuinely (not
approximately) simulated: two Linux bridges on the runner
(`kezio-site-a0`, `kezio-site-b0`), two Multus
`NetworkAttachmentDefinition`s, and two independent `kezio-bootd`
Deployments - one per bridge. Each target VM's provisioning NIC attaches
to only one bridge, so each VM's PXE broadcast genuinely only reaches its
own site's `kezio-bootd` instance; this part is not faked, it is two
real, disjoint broadcast domains, matching what a physically separate
site's own L2 segment needs in production
(`config/bootd/deployment.yaml`'s "one replica per boot segment"
comment).

What is *not* separately simulated is the data plane: BitTorrent/gRPC
traffic between seeders, the tracker, and both leechers all travels over
the cluster's ordinary pod network, the same way the single-leecher
`e2e-kubevirt-reusable.yml` lane already does it. A genuinely isolated
per-site data network would need either multiple runners or a real
multi-cluster/multi-network setup, neither of which a single GitHub
Actions job can provide - see the "does NOT claim" section above.

## Dependency on a published image (or an in-lane build)

Like `e2e-deploy-kubevirt.yml` and `e2e-boot-path-kubevirt.yml`, this
lane needs a published `kezio-boot-artifacts` image to exist before
either VM can even reach a live boot (`boot_artifacts_image` selects
among what is already published; publishing itself is a maintainer
action) - or set `build_boot_artifacts: true` to build it from this
checkout instead. It will not pass on a repository with no image
published yet, unless `build_boot_artifacts` is set.

## Runner requirements

Needs a KubeVirt-capable runner: `/dev/kvm` must be usable, and the job
fails fast otherwise. It is the heaviest e2e lane in this repository -
two full boot+deploy chains (two VMs, two `kezio-bootd` pods, three
`ezio-seeder` pods, one KubeVirt/CDI/Multus stack) sharing one runner's
CPU/RAM budget, on top of the five from-source image builds every
KubeVirt lane already pays for. The default GitHub-hosted runner (4 vCPU,
16 GiB RAM) is close to its ceiling for the single-leecher lane already;
this lane may need a larger or self-hosted runner for reliable results.
Use the `runs_on` input to target one without editing the workflow file.

## How to dispatch it

From the Actions tab, select **E2E Scale (Multi-Site KubeVirt VMs)** and
run it manually (`workflow_dispatch`). Inputs:

| Input | Type | Default | Purpose |
|---|---|---|---|
| `boot_artifacts_image` | string | `ghcr.io/tjjh89017/kezio-boot-artifacts:latest` | `kezio-boot-artifacts` OCI image both site VMs net-boot. Ignored when `build_boot_artifacts` is true. |
| `build_boot_artifacts` | boolean | `false` | Build the live boot image (including `kezio-agent`) from this checkout instead of pulling `boot_artifacts_image`. |
| `runs_on` | string | `ubuntu-latest` | Runner label to run the job on - point this at a larger or self-hosted KubeVirt-capable runner if the default hosted runner proves too resource-constrained. |

It does **not** run on push or pull request - see the workflow file's own
header comment for the full gating rationale (shared with the other
release-gated KubeVirt lanes: PXE over a nested Multus bridge is a real
risk surface, and this lane is additionally the most resource-hungry one
in the repository).

## Why a dedicated workflow instead of a `workflow_call` of
`e2e-kubevirt-reusable.yml`

The single-leecher reusable workflow's inputs describe exactly one boot
segment and one leecher VM. Expressing "N sites" through `workflow_call`
inputs would mean turning that reusable workflow itself into a loop over
a site list, which would also reshape it for the ezio repository's own
use of it (a single-leecher integration test, not a topology test) for a
shape only this lane needs. This workflow instead follows the same
setup steps in spirit (image builds, k3s/KubeVirt/CDI/Multus install,
ingest) and reuses the same verification technique per site, but stays a
separate file so the reusable workflow's own contract does not grow a
site-count parameter it has no other caller for.
