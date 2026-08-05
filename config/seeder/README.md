# Seeder data plane

This kustomization deploys the two pieces of kezio's BitTorrent seeding
data plane:

- **opentracker** - a BitTorrent tracker (`opentracker-deployment.yaml`,
  `opentracker-service.yaml`). Uses a maintained third-party image
  (`ghcr.io/tunisiano187/opentracker-docker`); see that Deployment's
  comment for the alternative (build opentracker from source) if that
  image ever goes stale.
- **ezio-seeder** - the ezio daemon that actually holds and serves
  partition-content torrents (`ezio-seeder-deployment.yaml`,
  `ezio-seeder-service.yaml`), built by `docker/seeder/Dockerfile`. This
  container ships no kezio code at all: it is upstream ezio plus a
  flag-passthrough entrypoint (`docker/seeder/entrypoint.sh`). The kezio
  operator is the only thing that decides what a seeder seeds, entirely
  through ezio's gRPC API (`internal/seeder`, `proto/ezio.proto`) - see
  `internal/controller/seeder_controller.go`'s doc comment for the
  reconcile loop that drives it.

It is a standalone kustomization, applied as an addition to
`config/default` (like `config/image-service`), not part of it:

```sh
kustomize build config/default | kubectl apply -f -
kustomize build config/seeder  | kubectl apply -f -
```

Both apply into the `kezio-system` namespace.

## Before applying

1. **A store PVC must already exist.** `ezio-seeder-deployment.yaml`
   mounts a PVC named `kezio-store` read-only at `/store`. This has to
   be the *same* volume the ingest Job writes to (see
   `cmd/main.go`'s `ingestConfigFromEnv`, `INGEST_STORE_PVC`) - a
   separately provisioned "store" PVC for the seeder would just be
   empty. Either name your real store PVC `kezio-store`, or patch
   `claimName` in an overlay.
2. **Wire the operator side.** The controller manager's seeder
   reconciler is off by default (see `cmd/main.go`'s
   `seederConfigFromEnv`); set on the `controller-manager` Deployment:
   - `SEEDER_TRACKER_URL` - the tracker's announce URL, reachable from
     wherever leechers are (for example
     `http://<opentracker's data-network IP>:6969/announce`; not the
     `opentracker` Service's ClusterIP - see the no-NAT rule below).
   - `SEEDER_STORE_ROOT` - where the store PVC is mounted, read-only, on
     the **manager** container itself (this is a separate mount from the
     ezio-seeder Deployment's; the manager reads `torrent.info` files
     directly to build `.torrent` bytes - see
     `internal/store.BuildTorrentFile`). Mounting this volume onto
     `controller-manager` is left to the cluster operator, the same
     deploy-time step `INGEST_STORE_PVC` already requires.
   - `SEEDER_SERVICE_NAMESPACE` / `SEEDER_SERVICE_NAME` - point at
     `ezio-seeder-service.yaml` once deployed (with this kustomization's
     `namePrefix`/`namespace`: `kezio-system` / `kezio-ezio-seeder`).
   - `SEEDER_GRPC_PORT_NAME` - optional, defaults to `grpc` (matches
     `ezio-seeder-service.yaml`'s port name; only needed if that is
     patched to something else).
   - `SEEDER_MAX_UPLOADS` / `SEEDER_MAX_CONNECTIONS` - optional, override
     the cluster-wide default per-torrent tuning (see "WAN swarm tuning"
     below).
3. **A real data network.** See the next section.

## The agent registration server reuses this same mount

`SEEDER_TRACKER_URL` and `SEEDER_STORE_ROOT` are not exclusive to the
seeder reconciler: `AGENT_SERVER_ADDR`'s GET `/agent/machines/<name>/next`
endpoint (`internal/agentserver`) builds each deployment plan's
per-partition `.torrent` bytes the same way `SeederReconciler.addContent`
does, from the identical `contents/<hash>/torrent.info` files under
`SEEDER_STORE_ROOT`, announcing at the identical `SEEDER_TRACKER_URL` -
the agent's leecher and the seeder join the same swarm. There is no
separate `AGENT_STORE_ROOT` / `AGENT_TRACKER_URL` pair to configure: if
`controller-manager` already has the store mounted read-only and
`SEEDER_TRACKER_URL` set for the seeder reconciler, the agent server
picks up both automatically. Running the agent server without the seeder
reconciler enabled still works, but GET `.../next` can then never build a
plan for an Image with a content partition - it answers `wait` forever
instead of a plan, which is easy to mistake for "still resolving".

## The no-NAT rule

BitTorrent is not NAT-friendly: a peer connects directly to the IP:port a
torrent's tracker announce advertised. If a seeder's actual listening
IP:port differs from what it announces - because a Service's ClusterIP
DNATs it, or a NAT gateway SNATs its egress - remote peers try to connect
to an address that either does not answer or answers as a different pod.
This repository's design settled on a hard requirement: **announce
IP:port == reachable IP:port, no NAT/SNAT anywhere on that path.**

Concretely:

- Both `ezio-seeder` and `opentracker` attach a routable data-network
  interface as a Multus secondary NIC (commented out by default in both
  Deployments' pod annotations - `k8s.v1.cni.cncf.io/networks`), separate
  from the cluster's own `eth0`. `eth0` stays for cluster-internal
  traffic only (kube-apiserver, kubelet, the operator's gRPC control
  connection to ezio); the data network carries BitTorrent peer
  connections and tracker announce/response traffic.
- Neither Deployment's Service exposes the BT/announce ports for
  cross-site traffic - both Services are ClusterIP, cluster-internal
  only (see each Service manifest's comment). Reachability for real BT
  traffic is the pod's own data-network IP, direct.
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

## Cross-network baseline: routed L3, not overlay/NAT

The no-NAT rule above is a peer-to-peer requirement; it also applies
between sites. kezio does not ship or require any particular VPN or
overlay technology - it requires only that every site's data network and
the central site's data network already have **routed L3 connectivity**:
a machine's leecher, a site-local seeder, the central seeder, and the
tracker must all be able to reach each other's data-network address
directly, with no NAT/SNAT/DNAT on the path in either direction. If
sites are not already connected at L3 (a WAN link, a private backbone,
inter-site BGP), a routed VPN mesh such as WireGuard between each site's
gateway is the natural way to get there: WireGuard forwards packets
between real addresses without translating them, so it satisfies the
no-NAT rule as long as its own routes are set up as specific routes (see
the no-NAT section above) - a WireGuard peer's tunnel address still has
to be reachable, unshadowed, from every other site. Setting up that VPN
mesh (or equivalent routed connectivity) is out of scope for this
kustomization and is the cluster operator's own network design; kezio
only assumes it already exists.

**The tracker must be reachable from every site.** There is exactly one
tracker in the whole deployment (see "Per-site seeders" below - the
tracker is deliberately not replicated per site), and every peer
(central seeder, every site-local seeder, every machine's leecher)
announces to it directly over whatever L3 path connects that peer's site
to the tracker's site. If a site cannot reach the tracker's data-network
address, machines at that site cannot join the swarm at all - this is a
harder requirement than reaching a particular seeder, since without the
tracker a leecher never learns any peer's address in the first place.

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
count is small by construction - the central seeder plus one seeder per
site plus whatever leechers are mid-deploy right now - and a low cap can
actually keep a leecher from connecting to every site's seeder at once.
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

**Slow start stays a seeder-only default**, applied at the daemon level
(`EZIO_SLOW_START` in `ezio-seeder-deployment.yaml`, on by default) -
unrelated to `max_uploads`/`max_connections` above, which are per-torrent
`AddTorrent` values. It ramps a seeder's upload rate up gradually instead
of going to line rate the instant a swarm forms, so a burst of machines
booting at once does not saturate the seeder's uplink and starve a
timeout-sensitive UEFI PXE/HTTP fetch on a machine that boots a little
later. Leechers (kezio-agent's local daemon) never set it: they finish
and leave, so there is no sustained upload burst to ramp.

### Overriding the defaults

Three layers, each optional, applied in this order:

1. **Seeder-side built-in default**: `seeder.DefaultMaxUploads` (3) and
   `seeder.DefaultMaxConnections` (10) - `internal/seeder/client.go`.
   Used by both the seeder reconciler's content torrents and the agent's
   leech torrents whenever nothing below overrides them.
2. **Cluster-wide operator default**, set on the `controller-manager`
   Deployment's environment:
   - `SEEDER_MAX_UPLOADS` / `SEEDER_MAX_CONNECTIONS` - applied to every
     content torrent `SeederReconciler` adds on every seeder endpoint.
   - `EZIO_DEFAULT_MAX_UPLOADS` / `EZIO_DEFAULT_MAX_CONNECTIONS` -
     applied to every `DeployPlan` this operator builds for a machine's
     leecher, before that Machine's own override (next layer). These are
     deliberately separate from `SEEDER_MAX_UPLOADS`/
     `SEEDER_MAX_CONNECTIONS`: a seeder pod serves every site at once and
     a leecher serves only itself, so they can reasonably want different
     defaults.
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

`ezio-seeder-deployment.yaml` sets `EZIO_BT_PORT` to a fixed value
(`16881`) rather than leaving ezio to pick one, because the no-NAT
contract above requires every seeder pod's actual listen port to match
what gets announced - an unpredictable ephemeral port would still work
per-pod, but makes firewalling the data network (allow exactly this
port, deny the rest) impossible.

## Operations: changing SEEDER_TRACKER_URL needs a seeder restart

Changing `SEEDER_TRACKER_URL` on the `controller-manager` Deployment does
**not** update torrents already added to a running `ezio-seeder` pod.
ezio has no remove-torrent API, and the seeder reconciler
(`internal/controller/seeder_controller.go`) diffs what to add by info
hash - an already-added torrent's info hash has not changed just because
the tracker URL changed, so the reconciler sees nothing new to do and
leaves it seeding under the old announce URL.

After changing `SEEDER_TRACKER_URL`, roll the `ezio-seeder` Deployment
(for example `kubectl rollout restart deployment/kezio-ezio-seeder -n
kezio-system`). The reconciler is level-triggered: a fresh, empty seeder
pod has no existing torrents to diff against, so it re-adds everything
from scratch with the new URL.

## Per-site seeders

A single central seeder works, but every byte a remote site's machines
deploy then crosses the WAN once per machine. When a site deploys more
than a handful of machines at once, it is worth running a seeder near
that site so same-site leechers exchange pieces with each other over
the LAN instead, and the WAN carries close to one copy total.

**Topology model: one Service, seeders in multiple sites, all as
endpoints of it - not a per-site Service.** Concretely:

- The **tracker stays central and singular.** `SEEDER_TRACKER_URL`
  never varies per site. The tracker is the one shared meeting point of
  the swarm (a per-site tracker would split the swarm into disconnected
  islands, since BT peers only ever come from their own tracker).
- A site-local seeder is **just another replica of the `ezio-seeder`
  component**, deployed as an additional Deployment whose pod template
  labels match `ezio-seeder-service.yaml`'s selector
  (`app.kubernetes.io/name: kezio`, `app.kubernetes.io/component:
  ezio-seeder`) - see `ezio-seeder-site.example.yaml` for the full
  template and its extended comments. Matching those labels is what
  makes the new pod a second endpoint of the *same* Service; nothing
  else changes.
- `SeederReconciler` (`internal/controller/seeder_controller.go`)
  already resolves every Ready endpoint of one Service independently -
  it was built that way from the start to tolerate more than one
  central replica (a pod mid-restart, for example), not specifically
  for sites. A site-local seeder endpoint is synced by exactly the same
  loop, with no code change: it is simply one more address in
  `seederTargets`'s list. `internal/controller/seeder_controller_test.go`
  has a case ("adds a Ready Image's content to every Ready seeder
  endpoint when there is more than one") that exercises this directly.
- Once a site's seeder is seeding, which peer a same-site leecher's
  libtorrent picks to exchange pieces with is **libtorrent's own peer
  selection over the shared swarm** - not something kezio decides or
  needs to decide. Concretely: "leechers
  in one site exchange pieces with each other locally... this is the
  normal BT behavior; no extra mechanism is needed." In practice:
  every peer (site seeder, central seeder, every machine's leecher)
  announces to the one tracker and gets back the swarm's full peer
  list; a site-local seeder is simply reachable at a shorter/cheaper
  route for that site's leechers, so it ends up serving most of their
  piece requests without any kezio-side hinting.

**Why not a per-site Service (option B), with the controller resolving
a Machine's `spec.networkSite` to a specific seeder Service for its
plan?** That would need: a Service per site, a naming/label convention
the controller resolves `networkSite` against, a fallback path for a
site with no seeder yet, and a change to `buildPartitionTorrent`
(`internal/agentserver/plan.go`) to pick a tracker/seeder set per
Machine. None of that changes what ends up in the `.torrent` bytes a
leecher gets, though: `SEEDER_TRACKER_URL` is the *only* thing that
actually has to be correct for a leecher to find the swarm, and it is
already the same value for every Machine by design (the central
tracker). Building a routing layer to solve a problem BT's own peer
discovery already solves for free would be extra machinery with no
behavior it makes possible that option A does not already give.
`Machine.spec.networkSite` therefore stays exactly what it already
was - the field that selects a machine's `kezio-bootd` zone - and does
not grow a second meaning for seeder routing (see its doc comment,
`api/v1alpha1/machine_types.go`).

**Precondition this model assumes:** a site-local seeder Deployment
still mounts the store PVC read-only, the same way the central seeder
does, so it needs the store volume to be reachable from that site's
node(s) (network-backed storage, or routed L3 to the central storage
backend - see the cross-network design notes below). A site with no
route to central storage at all cannot use this template as-is; it
would instead need a plain BT leecher that downloads the content once
and keeps seeding from local disk afterward (a "pre-warm" pattern:
with Option A this needs no artifact sync, since the download itself
is the sync). That pod shape is different (added with
`seed_mode=false`, not the `seed_mode=true` this reconciler always
uses) and is not implemented by this repository yet; it is a genuine
follow-up, not something the model above already covers.

## Known gap: ezio has no interface-binding flag

`docker/seeder/entrypoint.sh` documents this in more detail, but in short: ezio
has no flag to bind libtorrent's outgoing connections or announce IP to a
specific interface, only the listen *port*. On a pod with two interfaces
(`eth0` plus the data-network `net1`), libtorrent's own interface
selection may not always pick `net1`. If that turns out to matter in
practice, the fix is an upstream ezio change
(`outgoing_interfaces`/`announce_ip` flags), not something this
entrypoint or the operator should work around.

## Demand-driven torrent lifecycle

ezio has no `RemoveTorrent` RPC (`proto/ezio.proto` - only `AddTorrent`,
`PauseTorrent`, `ResumeTorrent`, `GetTorrentStatus`, `Shutdown`), so a
seeder's torrent set only ever grows in terms of what ezio itself knows
about; the operator's job is to control which of those torrents are
actively seeding (network traffic, connection slots) versus paused (no
network activity, pieces stay on disk, resumable later at no re-add
cost).

**What is implemented today**, in `SeederReconciler.syncTarget`
(`internal/controller/seeder_controller.go`): every reconcile computes
the demand set as "every content hash any Ready Image currently has,"
and diffs it against each seeder endpoint's current torrent set:

- a demanded hash missing from the endpoint gets `AddTorrent`;
- a demanded hash present but paused gets `ResumeTorrent`;
- a hash present on the endpoint but **not** in the demand set gets
  `PauseTorrent` (never removed - there is nothing to remove it with).

This makes "an Image stops being Ready" (deleted, or its ingest state
moves away from `Ready`) the demand signal: its content torrents get
paused on every seeder endpoint the next reconcile, and resumed
automatically if the Image becomes Ready again (for example, a
re-ingest) without kezio re-adding anything - `AddTorrent` is only ever
called once per hash per endpoint's lifetime (until that endpoint
restarts and loses its whole torrent set, at which point everything
demanded re-adds itself the same way a pod restart already self-heals
today).

**What is deliberately not implemented: per-site or per-machine demand
tracking.** The demand set above is Image-level - "does any Ready Image
still need this content seeded anywhere" - not "does some specific site,
or some specific machine, still need this content right now." A large
multi-site image set (many Ready Images, only a few actively being
deployed to any given site at any given moment) would ideally pause a
site-local seeder's torrent for an image nobody at that site is
currently deploying, even while the same content stays active on other
sites' seeders for their own in-progress deploys. Building that requires
correlating each seeder endpoint's site with which Machines are actively
provisioning which Image right now (a live signal, not the Image's own
static Ready state), and deciding what "idle" means per site rather than
per hash. That is a larger design (tracking demand per image *per site*,
not just per image) than fits safely alongside the tuning and coarse
pause/resume work here, and is left as a deliberate follow-up rather
than a half-built site-awareness layer bolted onto `syncTarget`.

In the meantime, the coarse Image-readiness signal above is still a real
win over "every seeder seeds every Ready Image's content forever": an
Image an operator explicitly retires (deletes, or fails and never
becomes Ready again) stops consuming seeder upload slots and connections
network-wide, even though it does not yet target a single idle site in
isolation.
