# Multi-segment and multi-site e2e lanes

Two jobs in `.github/workflows/main.yaml` prove kezio's Site/Subnet
network model end to end on real KubeVirt VMs: `e2e-routed-site` (a
routed, three-segment single Site) and `e2e-two-site-concurrent` (two
Sites that deliberately cannot route to each other). This document
describes what each currently proves, what each deliberately does not
claim, and how to read their output. Read `docs/network-model.md` first
for the Site/Subnet model both lanes exercise.

## e2e-routed-site: one Site, three routed segments

### Topology

Three genuinely separate Linux bridges on the runner, one routed group:

- Segment A and segment B each host one boot-half `Subnet` (their own
  `bootdServerIP`, DHCP in `proxy` mode) and one target VM.
- Segment C hosts one data-plane-only `Subnet` (`seederNetworkRef` set,
  no boot half) carrying the Site's tracker and its one seeder
  Deployment.

All three `Subnet`s name the same `Site`. `create-routed-segments` puts
all three bridges in one forwarding group, so the runner routes between
every pair of them at L3 - this is a real routed hop, not a shared flat
network: each boot segment's own NAD carries an IPAM route toward
segment C, and segment C's seeding NAD carries routes back toward both
boot segments, matching a real routed multi-subnet Site (see
`docs/physical-lab-deployment.md`, section 2.7).

### What it proves

- **Concurrency**: both target VMs' deploy intents are set back-to-back
  and the workflow polls until both Machines are observed mid-deploy at
  the same instant that `PartitionContent.status.seeders[]` reports
  `machineCount: 2` for the shared Site - proving the two deploys
  actually overlapped, not merely that both eventually succeeded.
- **One seeder Deployment per `(Image, Site)`, not per machine or per
  Subnet**: exactly one seeder Deployment exists for the whole lane, and
  the workflow explicitly asserts no second one appears even though the
  two machines sit on two different boot `Subnet`s.
- **The Site-owned tracker with no Service**: the tracker is reachable
  at its pinned address on segment C, and `Site.status` reflects it.
- **Real routed reachability**: from each boot segment, a probe
  namespace with no direct L2 presence on segment C still reaches the
  seeder's BitTorrent port and torrent HTTP port, and the tracker's
  announce port, through the routed hop alone.
- **A full boot-to-disk chain for both machines**: real ingest
  (qemu-img/sfdisk/partclone), a real BitTorrent leech, a real in-guest
  reboot, and the same three-layer verification
  (`docs/legacy/e2e-kubevirt-reusable.md`) other lanes use - run for both VMs
  independently.
- **`Site.status`**: both boot Subnets and the data Subnet appear in
  `status.subnetRefs`, and `Ready`/`Valid` are both True.

### What it deliberately does NOT claim

- Nothing about WAN-scale latency or bandwidth: the "routed hop" is
  Linux kernel routing between bridges on one runner, not a real
  multi-site link with a latency or bandwidth cap.
- Nothing about a routed-L3-to-a-cluster-Service tracker/seeder shape
  (`docs/network-model.md`'s "Option 2" style Multus attachment is the
  only one exercised) - kezio ships no LoadBalancer/NodePort/hostPort
  variant of a tracker or seeder Service to exercise instead.
- Nothing about more than two boot segments or more than one Image
  deploying at the Site concurrently - the seeder's static-IPAM address
  is correct for exactly this lane's one-Image-at-a-time shape; see
  `docs/network-model.md`'s address-pool sizing rule for what changes
  once that is no longer true.

## e2e-two-site-concurrent: two Sites that cannot route to each other

### Topology

Two bridges, two separate forwarding groups: `create-routed-segments`
gives each Site's segment its own group, so the runner's own routing
table drops all traffic between them. Each Site is the simplified
single-segment shape (boot and data plane on the same `Subnet`) - proving
the routed multi-subnet hop within one Site is `e2e-routed-site`'s job;
this lane's job is proving concurrency and cross-Site isolation, which a
second routed hop per Site would not add anything to.

### What it proves

- **Network isolation, both before and during the deploy**: a probe on
  Site X's segment can reach Site X's tracker and (once it exists)
  seeder, but never Site Y's - and the same in reverse. This is checked
  twice: once right after both Sites are created (tracker only), and
  again once both seeder Deployments exist (tracker and seeder both).
- **Concurrency across Sites**: both machines' deploy intents are set
  back-to-back, and the workflow polls until both are observed
  mid-deploy at the same instant that `PartitionContent.status.seeders[]`
  reports `machineCount: 1` for each Site separately - proving the two
  Sites' demand is tracked and served independently, not merged.
- **One seeder Deployment and one tracker per Site**: exactly two seeder
  Deployments exist in the whole lane (one per Site), each reachable
  only from its own Site's segment.
- **A full boot-to-disk chain for both machines**, concurrently, the
  same three-layer verification other lanes use.
- **Fault isolation**: the workflow injects a fault into Site Y's
  seeding `Subnet` (pointing it at a nonexistent NAD) and asserts that
  only Site Y's status degrades - Site X's status is unaffected by a
  fault at a Site it cannot even route to.

### What it deliberately does NOT claim

- Nothing about latency, bandwidth, WAN copy counts, or cross-site
  leech efficiency - one runner cannot produce those, and two Sites that
  cannot route to each other could never leech across Sites regardless
  (leeching across a Site boundary is not supported by the deploy-plan
  path at all - see `docs/network-model.md`).
- Nothing about a routed multi-subnet Site - each Site here is the
  simplified single-segment shape; `e2e-routed-site` covers the routed
  case.

## Runner requirements

Both lanes need a KubeVirt-capable runner: `/dev/kvm` must be usable,
and each job fails fast otherwise. Both are heavy - two full
boot-and-deploy chains, two bootd pods (or, in `e2e-routed-site`, two
bootd pods on separate segments plus one shared seeder/tracker pod pair),
and the KubeVirt/CDI/Multus stack, concurrently, on one runner. Each
job's own `check-runner-resources` step fails fast if the runner is
under-resourced rather than silently running slow or flaky; if a real
run needs more, the fix is a larger runner class for that job (a
`runs-on` change), never serializing the two deploys - concurrency is
each lane's own reason to exist.

## Dependency on a published or in-lane-built boot image

Like every other KubeVirt lane in this repository, both jobs need the
`kezio-boot-artifacts` live image, built earlier in the same workflow
run (the `boot-artifacts` job) and downloaded as a workflow artifact.
They do not pull a previously published image.

## How to read a run's failure

Both jobs collect site/tracker/seeder/bootd/runner-network diagnostics
on failure (`collect-site-diagnostics`), plus continuous VM console
capture for every VM they boot. Start there before re-reading this
document; both jobs' own step comments already label every CI-only
shortcut in place (which NADs are unused, why a probe network namespace
is used, and so on).
