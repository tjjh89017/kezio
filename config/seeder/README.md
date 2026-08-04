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
  `ezio-seeder-service.yaml`), built by `Dockerfile.seeder`. This
  container ships no kezio code at all: it is upstream ezio plus a
  flag-passthrough entrypoint (`entrypoint.seeder.sh`). The kezio
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
  the swarm (see the design doc's cross-network section); a per-site
  tracker would split the swarm into disconnected islands instead of
  helping it, since a torrent's peers are only ever the peers *its own*
  tracker knows about.
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
  needs to decide. This is the design doc's point explicitly: "leechers
  in one site exchange pieces with each other locally... this is the
  normal BT behavior; no extra mechanism is needed." Concretely here:
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
and keeps seeding from local disk afterward (the design doc's
"pre-warm" pattern - "with Option A this needs no artifact sync; the
download itself is the sync"). That pod shape is different (added with
`seed_mode=false`, not the `seed_mode=true` this reconciler always
uses) and is not implemented by this repository yet; it is a genuine
follow-up, not something the model above already covers.

## Known gap: ezio has no interface-binding flag

`entrypoint.seeder.sh` documents this in more detail, but in short: ezio
has no flag to bind libtorrent's outgoing connections or announce IP to a
specific interface, only the listen *port*. On a pod with two interfaces
(`eth0` plus the data-network `net1`), libtorrent's own interface
selection may not always pick `net1`. If that turns out to matter in
practice, the fix is an upstream ezio change
(`outgoing_interfaces`/`announce_ip` flags), not something this
entrypoint or the operator should work around.
