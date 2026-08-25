# The Site/Subnet network model

kezio models an operator's network as two objects: `Site`
(`api/v1alpha3/site_types.go`) and `Subnet`
(`api/v1alpha3/subnet_types.go`). Read this document before
`docs/physical-lab-deployment.md`: every prerequisite in that guide is a
consequence of the model here.

## A Site is a maximal routable domain

Every `Subnet` inside one `Site` is mutually routable. Anything a VRF,
firewall, or other barrier separates is, by definition, a different
`Site`. The user declares this by setting `Subnet.spec.siteRef`; kezio
never probes it (`SiteSpec`'s own doc comment,
`api/v1alpha3/site_types.go`).

**What a Site guarantees:**

- Every `Subnet` that names it can reach every other `Subnet` that names
  it, at L3.
- One tracker, either a pinned local address on the Site's own seeding
  `Subnet` or a reference to one the operator already runs
  (`Site.spec.tracker`, mutually exclusive `ip`/`externalURL`).
- At most one seeder Deployment per `Image` currently deploying at that
  Site (`internal/controller/image_seeder_placement.go`'s
  `buildImageSeederDeployment`): every `Subnet` of the Site shares the
  same seeder and tracker, so a machine on any of them leeches from the
  same swarm.

**What a Site does NOT guarantee:**

- Reachability to any other Site. Two Sites are never assumed to route
  to each other (`SiteSpec`'s doc comment: "Two different Sites are NOT
  guaranteed to reach each other at all"). This is why seeder and tracker
  placement are Site-scoped, not cluster-wide.
- A seeder or tracker at all. A Site with no `spec.seederSubnetRef` runs
  neither (`SiteReconciler.onChange`, `internal/controller/site_controller.go`)
  - correct for a Site whose Images carry no torrent content, but a
  Site whose Images do carry torrent content needs one: leeching across
  a routed link from another Site's seeder is not supported, so a
  Machine at such a Site waits forever
  (`ImageConditionSeederDegraded`, reason `SeederSubnetRefUnset`).

A Machine's Site is never hand-entered. It is derived:
`Machine.spec.subnetRef -> Subnet.spec.siteRef -> Site`
(`internal/sitederive`). This closes a class of misconfiguration a
hand-entered field would allow: a machine could physically boot from one
segment's bootd while declaring a different Site, and leech from a
distant seeder instead of the one beside it. Under this model that
mismatch cannot happen.

## Subnet: one broadcast domain, an optional boot half, an optional data half

A `Subnet` denotes one broadcast domain. Its **boot half**
(`bootdServerIP`, `bootdNetworkRef`, `dhcp`) is optional as a group: set
all three together and the Subnet gets a bootd Deployment, or leave all
three unset and the Subnet carries no machines
(`SubnetSpec`'s doc comment, `api/v1alpha3/subnet_types.go`). Separately,
`seederNetworkRef` names the network attachment seeder and tracker pods
use when this Subnet is a Site's designated seeding Subnet.

A Subnet must declare at least one half - the CRD schema rejects one with
neither (`"a Subnet must declare a boot half ... or seederNetworkRef; one
with neither hosts nothing"`). This is what makes a **data-plane-only
Subnet** expressible: a Subnet with `seederNetworkRef` set and no boot
half at all, carrying no machines, existing only to host the Site's
seeder and tracker pods on their own segment. `Subnet.HasBootPlane()`
reports which shape a given Subnet is.

`bootdServerIP` is explicit and pinned, never IPAM-allocated: it is the
address firmware caches mid-boot as the PXE next-server, so a
reallocation on pod restart would strand an in-flight TFTP fetch. A
seeder or tracker pod's address carries no such constraint from bootd's
side, but the tracker's own address is likewise pinned once chosen
(`Site.spec.tracker.ip`), because it is baked into every `.torrent` a
Site's seeders serve.

## The no-NAT rule

BitTorrent is not NAT-friendly: a peer connects directly to the
`IP:port` its tracker announce advertised. kezio's hard requirement is:
**announce `IP:port` == reachable `IP:port`, no NAT/SNAT/DNAT anywhere on
that path.**

Two ports every peer at a Site must reach, on every seeder pod's own
address (never a ClusterIP - a Service would DNAT the connection and
break this rule):

| Port | Protocol | Purpose | Verified against |
|---|---|---|---|
| `16881` | TCP | ezio's BitTorrent listen port (`EzioBTPort`) | `internal/seederdeploy/identity.go` |
| `8080` | TCP | the seeder's `.torrent`-by-info-hash HTTP server (`TorrentHTTPPort`) | `internal/seederdeploy/identity.go` |

The tracker's own announce port is `6969` (TCP+UDP,
`trackerAnnouncePort`, `internal/controller/site_tracker_deployment.go`).
No Service fronts a seeder or a Site's tracker Deployment: each is
single-homed on the Site's seeding `Subnet` via a Multus default-network
annotation (`multusDefaultNetworkAnnotation`), so the address a peer
announces is the address the pod actually listens on, with nothing
rewriting it in between.

## The address-pool sizing rule

A seeder Deployment exists per `(Image, Site)`, not per Site
(`internal/controller/image_seeder.go`, and
`image_seeder_placement.go`): a Site where several Images can
be deploying at once needs that many addresses available on its seeding
`Subnet`'s network attachment at once, plus the tracker's own pinned
address. `internal/nadvalidate.CheckSeederStaticMultiImage` raises an
Advisory once a Site's concurrent Image count exceeds what static IPAM
(one fixed address) can serve - not a hard Violation, since static IPAM
is exactly correct while a Site never runs more than one Image at a
time. That Advisory is not visible on the Subnet today:
`SubnetReconciler.updateSubnetConditions` reads only the Violation and
Indeterminate verdicts, so an Advisory changes no condition. Its input
is also a runtime count (the seeder Deployments that have an available
replica right now), not a declared one.

**Sizing rule:** pool size >= (max concurrently deploying Images at the
Site) x replicas (always 1 today: every seeder and tracker Deployment is
pinned to `replicas: 1`) + headroom. `Subnet.spec.bootdServerIP` is
excluded from this pool: `internal/nadvalidate.CheckSeederOverlap` and
`CheckTrackerAddress` both raise a Violation if the seeding `Subnet`'s
own network attachment could ever hand `bootdServerIP` (or the tracker's
own pinned IP) to a seeder pod, since that would collide with the
address bootd itself is bound to, or with the tracker's own address.

**The `host-local` exception.** `host-local` refuses a requested address
that falls outside its own `rangeStart`/`rangeEnd` and fails the pod's
sandbox with `failed to allocate all requested IPs`. On a seeding
attachment of that shape, the tracker's pinned address must therefore
lie INSIDE the range, not outside it. Put it at the TOP of the range:
`host-local` allocates upward from `rangeStart`, so no seeder pod takes
the pinned address before the tracker asks for it. The sizing rule then
reads: range size >= (max concurrently deploying Images at the Site) +
1 for the pinned tracker address. `internal/nadvalidate` does not model
a range-bounded `host-local` pool, so `CheckTrackerAddress` reports
Indeterminate for this shape instead of a verdict.

## A Site-managed tracker needs a pool on the seeding attachment

The tracker pod attaches to the seeding `Subnet`'s own
`seederNetworkRef` attachment, and pins `Site.spec.tracker.ip` with a
per-pod `ips` override on the default-network annotation
(`trackerPodAnnotations`,
`internal/controller/site_tracker_deployment.go`). Every seeder pod of
that Site attaches to the same attachment.

Multus gives that pinned address to the IPAM plugin as the CNI_ARGS
`IP` entry (`DelegateAdd`). It cannot give it as a runtime
configuration here: the default-network delegate is the master plugin,
and Multus writes a runtime configuration only for a delegate that is
not the master plugin (`mergeCNIRuntimeConfig`). The CNI_ARGS path is
therefore the only path this address takes.

The CNI `static` plugin **adds** a CNI_ARGS address to the address list
of its own configuration. It does not replace that list (`static`'s own
`LoadIPAMConfig`; its `runtimeConfig` and `args` paths do replace the
list, but Multus uses neither of them here). On a `static` seeding
attachment, the tracker pod therefore holds two addresses: its pinned
one, and the attachment's own configured one. Every seeder pod holds
that same configured address. Two pods then hold one address on one
segment - observed in CI as two pods that both held `192.0.2.5`.

A pool plugin does not do this. `host-local` gives one address for each
range, and uses the requested address for that range when the address
is inside the range. `whereabouts` behaves the same way.

**Rule:** a Site that declares `tracker.ip` must use a pool IPAM -
`host-local` or `whereabouts` - on its seeding attachment. A `static`
attachment with one address is correct only where the Site runs no
tracker of kezio's own: the `tracker.externalURL` case, which declares
no `tracker.ip`.

`internal/nadvalidate.CheckSeederStaticWithTracker` raises a Violation
for this shape, and `SiteReconciler` writes it to the Site's
`Valid`/`Ready` conditions. A Violation, not an Advisory, because no
`static` shape serves both roles at once: an address list that holds
`tracker.ip` collides with the tracker directly (`CheckTrackerAddress`,
reason `TrackerOverlapStatic`), and any other address list gives the
tracker an address that every seeder pod also holds. The Violation
withholds nothing - the tracker Deployment is still created, and still
binds its pinned address - so it only tells the operator what is wrong.
`CheckSeederStaticMultiImage` answers a different question: how many
seeders one static address can serve. It says nothing about the
tracker, and reports OK for a Site that runs one Image.

## whereabouts needs its ip-reconciler, if you choose whereabouts

A seeder network attachment sized for more than one concurrent Image
needs a range-based IPAM plugin, not static addressing. kezio's own e2e
lanes and lab guide make that choice with `host-local` and a narrowed
range (`.github/actions/create-provisioning-nads`,
`docs/lab-proxmox-rke2.md`). `whereabouts` is the other supported
choice, and it carries one condition. Deploying
`whereabouts` without also deploying its `ip-reconciler` CronJob is not a
supported configuration: `ip-reconciler` reclaims addresses whose pod
was deleted without whereabouts observing the deletion event (a node
crash, a hard kubelet restart) - without it, a pool can silently leak
down to zero free addresses over the life of a cluster, and a Site whose
seeder count fluctuates as Images come and go is exactly the workload
that leaks addresses this way. Treat `ip-reconciler` as part of
deploying `whereabouts` at all, never as an optional extra.

## The tracker is per Site, pinned or external

`Site.spec.tracker` is either `ip` (a pinned local address on the Site's
seeding `Subnet`, backing a Site-owned tracker Deployment
`SiteReconciler` creates and keeps current) or `externalURL` (a tracker
the operator already runs; kezio creates nothing and checks nothing
about it) - the two are mutually exclusive
(`SiteTracker`'s `XValidation` rule, `api/v1alpha3/site_types.go`). The
`ip` choice constrains the seeding attachment's IPAM: see "A
Site-managed tracker needs a pool on the seeding attachment" above.

**A single global tracker cannot exist across Sites that do not route to
each other.** A tracker's announce responses must be reachable by every
peer in its swarm; if two Sites cannot route to each other by
definition (the non-guarantee `SiteSpec`'s doc comment states), a
tracker on one Site's segment is, by that same non-guarantee, not
provably reachable from the other. Scoping the tracker to the Site that
can actually reach it - rather than running one cluster-wide tracker and
hoping every Site happens to route to it - is what keeps the model
honest about what it guarantees. An operator whose Sites do all happen
to share one routed L3 fabric may still choose to point every Site's
`tracker.externalURL` at one shared tracker; kezio does not forbid this,
it simply never assumes it.

## See also

- `docs/physical-lab-deployment.md` - the full operational setup,
  including the single-subnet and routed multi-subnet cases.
- `docs/lab-proxmox-rke2.md` - a worked walkthrough that makes one
  concrete choice at every point this document gives options for.
