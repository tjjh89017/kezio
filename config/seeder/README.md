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

## Known gap: ezio has no interface-binding flag

`entrypoint.seeder.sh` documents this in more detail, but in short: ezio
has no flag to bind libtorrent's outgoing connections or announce IP to a
specific interface, only the listen *port*. On a pod with two interfaces
(`eth0` plus the data-network `net1`), libtorrent's own interface
selection may not always pick `net1`. If that turns out to matter in
practice, the fix is an upstream ezio change
(`outgoing_interfaces`/`announce_ip` flags), not something this
entrypoint or the operator should work around.
