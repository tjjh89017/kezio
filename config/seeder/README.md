# Seeder data plane

This kustomization deploys **opentracker**, the BitTorrent tracker
(`opentracker-deployment.yaml`, `opentracker-service.yaml`). It uses a
maintained third-party image (`ghcr.io/tunisiano187/opentracker-docker`);
see that Deployment's comment for the alternative (build opentracker
from source) if that image ever goes stale.

The tracker is the only always-on part of the seeding data plane. The
seeder itself is not a standalone Deployment: the operator creates one
seeder Deployment per Image, per site, only while a Machine at that site
is deploying that Image (see "Per-Image, on-demand seeding" below).

Apply this kustomization as an addition to `config/default` (like
`config/image-service`):

```sh
kustomize build config/default | kubectl apply -f -
kustomize build config/seeder  | kubectl apply -f -
```

Both apply into the `kezio-system` namespace.

## Per-Image, on-demand seeding

Each Image's content lives in one PVC per partition, written by the
ingest and publish Jobs (see `internal/controller/image_ingest.go`,
`internal/controller/image_publish.go`). The Image reconciler
(`internal/controller/seeder_deployment.go`) watches every Machine and,
for each site where at least one Machine currently holds a reference to
an Image (deploying it, or retrying a deploy failure - see
`machineHoldsSeederReference`), ensures exactly one seeder Deployment
exists for that (Image, site) pair. Each Deployment:

- mounts read-only exactly the partition PVCs its own Image owns - no
  shared store volume, and nothing wider than what that Image needs;
- runs two containers: `ezio-seeder` (the BitTorrent daemon) and
  `seeder-register`, a pod-local process that registers this pod's own
  mounted partition content with `ezio-seeder` over its local gRPC
  socket. Registration happens inside the pod because the content it
  reads (each partition's PVC) is not reachable from the operator
  itself.

When the last Machine referencing an (Image, site) pair stops deploying,
the reconciler keeps that Deployment for a grace period (in case another
deploy against the same site starts right after), then deletes it. This
means: **no Machine deploying an Image at a site implies no seeder
Deployment for it.** A freshly-ingested Image with no Machine deploying
it yet has no seeder running - this is the intended shape, not a gap.

Deleting the Image itself deletes every seeder Deployment for it through
ordinary Kubernetes garbage collection (each Deployment carries an owner
reference to its Image).

## Wiring the operator side

Set these on the `controller-manager` Deployment's environment
(`cmd/main.go`'s `seederDeploymentConfigFromEnv`, `trackerConfigFromEnv`,
`agentServerConfigFromEnv`):

- `SEEDER_DEPLOYMENT_IMAGE` - the `ezio-seeder` container image
  reference (built by `docker/seeder/Dockerfile`). Leaving this unset
  disables per-Image seeder Deployments entirely.
- `SEEDER_TRACKER_URL` - the tracker's announce URL, reachable from
  wherever leechers are (for example
  `http://<opentracker's data-network IP>:6969/announce`; not the
  `opentracker` Service's ClusterIP - see the no-NAT rule below). This
  URL rides inside every `.torrent` a seeder Deployment or a Machine's
  leecher builds, so leaving it unset means Deployments still come and
  go with demand, but nothing ever seeds any content on them.
- `SEEDER_DEPLOYMENT_GRACE_PERIOD` - optional (a Go duration string,
  for example `5m`); how long an idle Deployment is kept before
  deletion. A default applies when unset.
- `SEEDER_MAX_UPLOADS` / `SEEDER_MAX_CONNECTIONS` - optional, override
  the cluster-wide default per-torrent tuning (see "WAN swarm tuning"
  below).

There is no cluster-wide `SEEDER_DEPLOYMENT_NETWORK` any more: a seeder
pod's Multus default-network annotation is derived per site, from the
Site's designated seeder Subnet (`SiteSpec.SeederSubnetRef`) and that
Subnet's own `SeederNetworkRef` (`seederPodAnnotations`,
`internal/controller/seeder_deployment.go`). A Site whose seeder Subnet
carries no `SeederNetworkRef` still gets its seeder Deployment, just
with its pods on the ordinary cluster network only - the same "no
annotation" fallback the old unset env var produced, now decided per
Site instead of once for the whole cluster. A Site with no
`SeederSubnetRef` at all gets no seeder Deployment created for it: any
Image with torrent content needs its deploying Site to have its own
seeder Subnet, since the deploy-plan path only ever resolves a seeder at
the Machine's own Site (`ImageConditionSeederDegraded`'s
`SeederSubnetRefUnset` Reason surfaces this on the Image). See
`config/samples/kezio_v1alpha1_site.yaml` and
`config/samples/kezio_v1alpha1_subnet.yaml`.

## The agent registration server needs no store mount

`AGENT_SERVER_ADDR`'s GET `/agent/machines/<name>/next` endpoint
(`internal/agentserver`) builds each deployment plan's per-partition
`.torrent` bytes straight from `ImagePartitionStatus.TorrentInfo` -
carried inline in the Image's own status, not read from a mounted
volume - announcing at `SEEDER_TRACKER_URL` (reused from the same
tracker config, the identical value a seeder Deployment and a Machine's
leecher both need to join the same swarm). Running the agent server
without `SEEDER_TRACKER_URL` set still works, but GET `.../next` can
then never build a plan for an Image with a content partition - it
answers `wait` forever instead of a plan, which is easy to mistake for
"still resolving".

## The no-NAT rule

BitTorrent is not NAT-friendly: a peer connects directly to the IP:port a
torrent's tracker announce advertised. If a seeder's actual listening
IP:port differs from what it announces - because a Service's ClusterIP
DNATs it, or a NAT gateway SNATs its egress - remote peers try to connect
to an address that either does not answer or answers as a different pod.
This repository's design settled on a hard requirement: **announce
IP:port == reachable IP:port, no NAT/SNAT anywhere on that path.**

Concretely:

- Both a per-Image seeder pod and `opentracker` attach a routable
  data-network interface as a Multus secondary NIC when configured (the
  seeder Subnet's `SeederNetworkRef` for the seeder, resolved per Site -
  see "Wiring the operator side" above; a commented-out annotation on
  `opentracker-deployment.yaml`), separate from the cluster's own
  `eth0`. `eth0` stays for cluster-internal traffic only
  (kube-apiserver, kubelet); the data network carries BitTorrent peer
  connections and tracker announce/response traffic.
- `opentracker-service.yaml` is ClusterIP, cluster-internal only.
  Reachability for real BT traffic is each pod's own data-network IP,
  direct.
- Routing onto the data network uses **specific routes via the secondary
  interface, not a default-route flip.** Most cluster CNIs (Calico
  included) rely on the pod's default route staying on `eth0` for the
  cluster's own pod/service CIDRs; flipping the default route to the
  data NIC breaks that return path unless every cluster CIDR is also
  explicitly routed back over `eth0`. Scoping the data network's routes
  to the secondary interface only (via the NetworkAttachmentDefinition's
  own routing config) avoids touching the CNI's existing routing
  decisions at all.

See `networkattachmentdefinition.example.yaml` for the actual
NetworkAttachmentDefinition this depends on (a placeholder - it is
site-specific and not applied by this kustomization) and its extended
comments on IPAM choice (static vs. `whereabouts`, and why not DHCP).

## Provisioning-segment address pool

**Hard prerequisite:** the provisioning network a Site's seeder Subnet
names through `SeederNetworkRef` must carry enough addresses for every
per-Image seeder Deployment that Site can have running at once, not just
one. Each seeder pod is single-homed on this network - the Multus
default-network annotation replaces its whole default attachment - so
`pod.Status.PodIP` is the one address that Deployment's pod uses. One
(Image, site) pair holds
one seeder Deployment (see "Per-Image, on-demand seeding" above); a
site where several Images can be deploying at the same time therefore
needs an address for each one of them, concurrently.

**Exclude the Subnet's `bootdServerIP` from the pool.** bootd and the
seeder share one L2 segment on this Subnet, whether or not they attach
through the same NAD. `bootdServerIP` doubles as the PXE next-server
and TFTP target that firmware caches mid-boot (see
`api/v1alpha1/subnet_types.go`'s `SubnetSpec` doc comment). A pool that
can hand `bootdServerIP` to a seeder pod collides with bootd's own
pinned address. Bound or size the pool so it never includes
`bootdServerIP`. Sharing one NAD between bootd and the seeder is still
allowed - only address overlap is forbidden. The controller checks this
and raises a `SeederOverlapValid` condition on the Subnet when it finds
an overlap, but treat that as a safety net. Fix the pool's bounds
before the address ever gets handed out.

**Sizing rule:**

```
pool size >= (max concurrently active Images at the site) x replicas, PLUS HEADROOM
```

"Concurrently active Images at the site" is the largest number of
distinct Images any Machine at that site is deploying, or retrying a
provisioning failure for, at the same moment
(`machineHoldsSeederReference`). "Replicas" is each seeder Deployment's
own replica count (`buildSeederDeployment` sets 1 today). Headroom
absorbs the grace period (`SEEDER_DEPLOYMENT_GRACE_PERIOD`, default
5m) that keeps a just-emptied Deployment's pod - and its address -
alive for a while after the last Machine referencing it stops
deploying, plus (with `whereabouts`) any leaked allocation that has not
yet been reclaimed. Size generously: an address in a dedicated
provisioning-segment pool is cheap relative to a seeder Deployment that
fails to start because the pool ran dry.

Two example NADs cover this - **which one a site needs is that site's
own choice, driven by how many Images it deploys concurrently**, not a
fixed rule:

- `networkattachmentdefinition.example.yaml` - a single static address.
  Every attaching pod gets the same address, which is only safe while
  the site runs at most one Image at a time - a lab, or a small
  single-Image site.
- `networkattachmentdefinition-whereabouts.example.yaml` - a
  range-based `whereabouts` pool, sized for the site's concurrency (see
  the sizing rule above). Needed once a site can have more than one
  Image deploying at once, since each concurrent (Image, site) seeder
  Deployment then needs its own address from the pool.

Choosing static ipam for a site that later grows past one concurrent
Image is not rejected outright - the controller instead raises an
Advisory `SeederStaticMultiImage` condition on the seeder Subnet
(reason `SeederStaticIPAMMultiImage`,
`nadvalidate.CheckSeederStaticMultiImage`) once more than one Image is
deploying concurrently at that Subnet's Site while its `SeederNetworkRef`
still resolves to static ipam. Treat that condition as the signal to
move to a range-based pool, not a hard failure to work around.

**With `whereabouts`, the `ip-reconciler` CronJob is required, not
optional.** `whereabouts` leaks allocations on ungraceful pod deletion,
and per-Image seeder Deployments are a high-churn create/delete pattern
by design - every deploy start and grace-period expiry creates or
deletes a pod, so this leak path is exercised constantly, not as a rare
edge case. Deploy `whereabouts`' own `ip-reconciler` CronJob
(https://github.com/k8snetworkplumbingwg/whereabouts/tree/master/doc#ip-reconciler)
alongside the NAD; without it, a leaked pool slowly starves new seeder
Deployments of addresses until nothing can start.

**Two ports must be reachable across the provisioning segment** for
every peer that needs to reach a seeder pod: the fixed BitTorrent
listen port (`16881` - see "Fixed BT listen port" below; stable because
each pod gets its own network namespace) and the `.torrent` HTTP
endpoint's port `8080` (`seederdeploy.TorrentHTTPPort`), which the
agent fetches a partition's `.torrent` from directly at
`http://<PodIP>:8080/torrents/<infohash>`.

**Namespace-qualified `SeederNetworkRef`:** Multus resolves an
unqualified `default-network` annotation value in its own system
namespace (`kube-system`), not the pod's namespace the way the ordinary
`k8s.v1.cni.cncf.io/networks` annotation is resolved. A bare NAD name
would therefore silently point at a NAD that does not exist there.
`seederPodAnnotations` (`internal/controller/seeder_deployment.go`)
accounts for this: it qualifies `SeederNetworkRef` with the seeder
Subnet's own namespace (`<namespace>/<name>`), not the seeder
Deployment's namespace (the Image's own) - the NAD lives wherever its
Subnet lives, the same way bootd's NAD and Deployment share a namespace.
`SeederNetworkRef` itself can still be set to either a bare name
(qualified automatically) or an already-qualified `<namespace>/<name>`
value (`NameRef`'s own optional `Namespace` field).

## Cross-network baseline: routed L3, not overlay/NAT

The no-NAT rule above is a peer-to-peer requirement; it also applies
between sites. kezio does not ship or require any particular VPN or
overlay technology - it requires only that every site's data network and
the central site's data network already have **routed L3 connectivity**:
a machine's leecher, a per-Image seeder pod at any site, and the tracker
must all be able to reach each other's data-network address directly,
with no NAT/SNAT/DNAT on the path in either direction. If sites are not
already connected at L3 (a WAN link, a private backbone, inter-site
BGP), a routed VPN mesh such as WireGuard between each site's gateway is
the natural way to get there: WireGuard forwards packets between real
addresses without translating them, so it satisfies the no-NAT rule as
long as its own routes are set up as specific routes (see the no-NAT
section above) - a WireGuard peer's tunnel address still has to be
reachable, unshadowed, from every other site. Setting up that VPN mesh
(or equivalent routed connectivity) is out of scope for this
kustomization and is the cluster operator's own network design; kezio
only assumes it already exists.

**The tracker must be reachable from every site.** There is exactly one
tracker in the whole deployment, and every peer (every per-Image seeder
pod, every machine's leecher) announces to it directly over whatever L3
path connects that peer's site to the tracker's site. If a site cannot
reach the tracker's data-network address, machines at that site cannot
join the swarm at all - this is a harder requirement than reaching a
particular seeder, since without the tracker a leecher never learns any
peer's address in the first place.

## WAN swarm tuning

EZIO's per-torrent `AddTorrent` call takes two tuning values that this
operator resolves and applies on every torrent it adds:
`max_uploads` and `max_connections` (`proto/ezio.proto`'s `AddRequest`;
there is no other per-torrent knob in that message besides
`sequential_download`, which kezio leaves at its default, `false`, for
every torrent - sequential order helps nothing here and would slow
piece availability across peers).

**`max_connections` matters most for a routed, multi-site swarm.**
EZIO's own default (5) suits one LAN segment, where limiting fan-out
barely matters. Across a routed, multi-site swarm the relevant peer
count is small by construction - a handful of per-Image seeder pods
plus whatever leechers are mid-deploy right now - and a low cap can
actually keep a leecher from connecting to every seeder pod at once.
kezio's own built-in default is **10**, not EZIO's 5: enough headroom
for a handful of sites without opening the large LAN-style fan-out
(50-100+ connections) that finds no additional useful peers on a routed
network, only more idle connections across the routed links.
`max_uploads` keeps EZIO's own default (3): there is no WAN-specific
reason to raise it, since it caps how many peers this node uploads to
concurrently, not how many it can reach.

**Encryption and uTP are off by ezio's own contract, not a kezio
choice.** `AddRequest` has no field to toggle either, so there is
nothing here to configure: BitTorrent peer connections are plain TCP,
unencrypted. Encrypt the routed path itself (for example, terminate it
inside a WireGuard tunnel, per the section above) if that matters for
your network.

**Slow start is a daemon-level flag** (`EZIO_SLOW_START`), unrelated to
`max_uploads`/`max_connections` above, which are per-torrent
`AddTorrent` values. It would ramp a seeder's upload rate up gradually
instead of going to line rate the instant a swarm forms, so a burst of
machines booting at once does not saturate the seeder's uplink and
starve a timeout-sensitive UEFI PXE/HTTP fetch on a machine that boots a
little later. The per-Image seeder pod template
(`internal/controller/seeder_deployment.go`) does not set it today - a
follow-up, not something this kustomization configures.

### Overriding the defaults

Three layers, each optional, applied in this order:

1. **Seeder-side built-in default**: `seeder.DefaultMaxUploads` (3) and
   `seeder.DefaultMaxConnections` (10) - `internal/seeder/client.go`.
   Used whenever nothing below overrides them.
2. **Cluster-wide operator default**, set on the `controller-manager`
   Deployment's environment:
   - `SEEDER_MAX_UPLOADS` / `SEEDER_MAX_CONNECTIONS` - applied to every
     content torrent a per-Image seeder pod's `seeder-register`
     container adds.
   - `EZIO_DEFAULT_MAX_UPLOADS` / `EZIO_DEFAULT_MAX_CONNECTIONS` -
     applied to every `DeployPlan` this operator builds for a machine's
     leecher, before that Machine's own override (next layer). These are
     deliberately separate from `SEEDER_MAX_UPLOADS`/
     `SEEDER_MAX_CONNECTIONS`: a seeder pod serves every leecher at its
     site at once and a leecher serves only itself, so they can
     reasonably want different defaults.
   - Either pair left unset keeps layer 1's built-in default.
3. **Per-Machine override**: `Machine.spec.ezio.maxUploads` /
   `Machine.spec.ezio.maxConnections` (`MachineEzioTuning`, validated
   `1`-`1024`) - for one machine behind a slower link, on a small-RAM
   box, or in CI. Only the fields actually set on the Machine override
   layer 2; every other field still falls back
   (`keziov1alpha1.MergeEzioTuning`).

`Machine.spec.ezio` also carries `cacheSizeMB`, `aioThreads`, and `port`
- EZIO daemon-level flags applied when kezio-agent launches its local
leecher daemon (`cmd/agent/ezio.go`). Those have no cluster-wide default
of their own today; an absent value there falls back to EZIO's own
built-in daemon default, not to a KEZIO-computed one.

## Fixed BT listen port

Each per-Image seeder pod sets `EZIO_BT_PORT` to a fixed value (`16881`)
rather than leaving ezio to pick one, because the no-NAT contract above
requires every seeder pod's actual listen port to match what gets
announced - an unpredictable ephemeral port would still work per-pod,
but makes firewalling the data network (allow exactly this port, deny
the rest) impossible.

## Known gap: ezio has no interface-binding flag

`docker/seeder/entrypoint.sh` documents this in more detail, but in
short: ezio has no flag to bind libtorrent's outgoing connections or
announce IP to a specific interface, only the listen *port*. On a pod
with two interfaces (`eth0` plus the data-network `net1`), libtorrent's
own interface selection may not always pick `net1`. If that turns out
to matter in practice, the fix is an upstream ezio change
(`outgoing_interfaces`/`announce_ip` flags), not something the operator
should work around.

## What is not tracked: per-machine demand within a site

The reference count behind each (Image, site) seeder Deployment counts
Machines currently deploying that Image at that site
(`seederDemandBySite`); it does not track which specific torrents any
one Machine still needs mid-deploy. Every content torrent a seeder pod's
`seeder-register` container has registered stays registered for the
Deployment's whole lifetime (ezio has no `RemoveTorrent` RPC - see
`proto/ezio.proto`); only the Deployment's existence, not its individual
torrent set, responds to demand. This is a coarser signal than per-torrent
demand, but a correct one: a Deployment that exists at all always has
every torrent a Machine deploying that Image at that site could need.
