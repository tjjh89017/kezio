# Custom resource reference

kezio defines ten custom resources in the group
`kezio.kojuro.date`, version `v1alpha3`. Every kind is
namespace-scoped. This document describes each kind, and how the kinds
refer to each other.

The order below is the order an operator meets these kinds: first the
network, then the content, then the machines that receive the content.

Each `*Ref` field is a `NameRef`: a `name`, and an optional
`namespace`. An empty `namespace` means the namespace of the object
that holds the reference (`api/v1alpha3/shared_types.go`).

## How the kinds relate

```
NETWORK                        CONTENT
-------                        -------

 Site                           ImageImport
  |                              |  one partclone run, in the cluster
  |  spec.seederSubnetRef        |
  |  (optional)                  +--> PartitionContent   (one per non-swap
  v                              |      partition; immutable; owns a PVC)
 Subnet                          |
  ^  spec.siteRef                +--> Image              (created complete)
  |                                      |
  |                                      |  spec.layout.slots[].contentRef
  |                                      v
  |                              PartitionContent
  |
  |  spec.subnetRef
  |
 Machine
  ^  spec.machineName
  |
 MachineClaim ---- spec.imageRef ------------> Image
  |            \--- spec.dataImages[].imageRef -> Image
  |             \-- spec.postHookRefs ---------> PostHook
  |                                                ^
  |                                                |  spec.postHookRefs
  |                                              Image
  |
 Machine
  +--> MachineHardware   (same name, owned by the Machine)
  +--> DeployRun         (owned by the Machine)
```

The Site of a Machine is derived, never written by hand. The chain is
`Machine.spec.subnetRef` to `Subnet.spec.siteRef` to `Site`
(`internal/sitederive`).

A `Machine` carries no deploy intent of its own. A `MachineClaim`
binds to one `Machine` and carries the intent - which Image, which
data images, which disk, which hooks. See
[MachineClaim](#machineclaim) below.

## 1. The network model

### Site

A `Site` is a maximal routable domain. Every `Subnet` that belongs to
one `Site` can route to every other `Subnet` of that `Site`. No VRF,
firewall, or other barrier is between them. The user declares this;
kezio never probes it.

Two different Sites are not assumed to reach each other at all. This
is why seeder and tracker placement are Site-scoped, and not
cluster-wide.

| Field | Purpose |
|---|---|
| `spec.seederSubnetRef` | The `Subnet` of this Site that seeder pods and the tracker attach to. Optional. A Site without it runs no seeder and no tracker. |
| `spec.tracker.ip` | A pinned IPv4 address for a tracker that kezio runs for this Site. |
| `spec.tracker.externalURL` | A tracker that the operator already runs. kezio creates no tracker for this Site, and checks nothing about the address. |
| `status.subnetRefs` | The names of the Subnets in this Site's namespace whose `spec.siteRef` names this Site. |
| `status.trackerURL` | The resolved announce URL. It comes from `tracker.ip`, or it echoes `tracker.externalURL`. It is empty when the Site has no tracker. |
| `status.seederReady` | Whether the seeder placement of this Site is healthy. Always false for a Site with no `seederSubnetRef`. |

`tracker.ip` and `tracker.externalURL` are mutually exclusive. Both
are meaningless without `seederSubnetRef`, because a Site with no
seeder has nothing to announce.

`tracker.ip` is pinned, and never allocated by an IPAM plugin. kezio
writes this address into every `.torrent` that the seeders of this
Site serve. The address must stay the same across tracker pod
restarts.

Conditions: `Valid` (schema checks, plus the checks of the Site
reconciler against `seederSubnetRef`) and `Ready` (the tracker
Deployment is available). A Site with no tracker Deployment of its own
is Ready as soon as it is Valid.

### Subnet

A `Subnet` is one broadcast domain.

| Field | Purpose |
|---|---|
| `spec.siteRef` | The `Site` this Subnet belongs to. Required. |
| `spec.cidr` | The IPv4 network of this Subnet, for example `192.0.2.0/24`. Required. |
| `spec.bootdServerIP` | The IPv4 address of bootd on this Subnet. Firmware reads it back as the PXE boot server, and as the TFTP next-server. |
| `spec.bootdNetworkRef` | The `NetworkAttachmentDefinition` that the bootd Deployment attaches through. |
| `spec.dhcp` | The DHCP behavior of bootd on this Subnet. |
| `spec.seederNetworkRef` | The `NetworkAttachmentDefinition` that seeder and tracker pods attach through, when this Subnet is the seeding Subnet of its Site. |
| `spec.nodeSelector` | Holds bootd pods, and seeder pods, on nodes that are attached to this broadcast domain. Empty means no constraint. |
| `status.dhcp.reservations` | The boot-scoped DHCP address reservation table: one entry per Machine currently net-booting or deployed through this Subnet, in lease mode. Each entry has `address`, `machine`, `mac`, and `since`. |
| `status.dhcp.revision` | Changes whenever `reservations` changes. A digest of the sorted table, not a counter. |
| `status.dhcp.appliedRevision` | The `revision` bootd's dnsmasq hostsfile last actually rendered and reloaded for. Written only after dnsmasq has confirmed - by its own log line - that it re-read the hostsfile, not merely after the reload was requested. Written by bootd, never by the manager. |

`bootdServerIP`, `bootdNetworkRef`, and `dhcp` are the **boot half**.
They are optional as a group. Set all three, and the Subnet gets a
bootd Deployment. Leave all three unset, and the Subnet carries no
machines.

`seederNetworkRef` is the **data half**. A Subnet must declare at
least one half. The schema rejects a Subnet that declares neither,
because such a Subnet hosts nothing.

A Subnet with `seederNetworkRef` and no boot half is a
**data-plane-only Subnet**. It carries no machines. It exists only to
hold the seeder and tracker pods of its Site on their own segment.
`SubnetSpec.HasBootPlane()` reports which shape a Subnet has.

`bootdServerIP` is pinned, and never allocated by an IPAM plugin.
Firmware caches it during the boot, so a new address after a pod
restart strands a TFTP fetch that is in progress.

Conditions: `Valid` (schema checks, plus the checks of the Subnet
reconciler against the referenced network attachments and the DHCP
configuration), `Ready` (the bootd Deployment is available), and
`DHCPPoolExhausted` (the lease-mode address pool has no free address
left for a new reservation - always False in proxy mode).

### Boot-scoped DHCP address reservations (lease mode)

A lease-mode Subnet hands out a fixed, pre-decided address to each
Machine it net-boots, instead of leaving dnsmasq's own dynamic lease pool
to arbitrate between concurrently booting machines. This removes the
race that produces a DHCPNAK "address in use" at fleet scale, and keeps
the pool holding only machines that are actually booting or deployed -
never every enrolled Machine, and never a Machine long since provisioned.

The lifecycle:

- **Allocated** at net-boot arm time - the same moment the deployer mints
  that boot's registration token (Inspect's and Provision's arming
  step) - by picking the lowest free address in the Subnet's lease
  range that is not already in `status.dhcp.reservations` and is not
  `spec.bootdServerIP` or a non-empty `spec.dhcp.gateway`. Calling this
  again for a Machine that already has an entry reuses it: the address
  a Machine boots with never changes mid-attempt. A pool with no free
  address sets `DHCPPoolExhausted` and an Event on the Machine, and the
  Machine is held (not powered on) until an address frees up.
- **Applied** - bootd renders every reservation into its dnsmasq
  hostsfile alongside the MAC allowlist, signals dnsmasq to reload, and
  only once dnsmasq's own log confirms it has actually re-read that
  hostsfile writes `status.dhcp.appliedRevision` to match
  `status.dhcp.revision`. The deployer waits for that match before it
  powers the machine on: a reservation persisted but not yet live at
  dnsmasq would otherwise race the machine's own DHCPDISCOVER.
- **Released** when the deploy step completes (Inspect reaching
  `Available`, Provision reaching `Provisioned`), when the Machine is
  deleted, and when its `spec.bootMACAddress` or `spec.subnetRef`
  changes - the last two by the Subnet's own reconcile, which also
  garbage-collects any entry whose Machine no longer exists or whose
  current MAC/subnetRef no longer match, as a backstop.

Proxy mode never has a reservation table at all: proxyDHCP never assigns
addresses, so bootd renders only the MAC allowlist.

### The three states of `dhcp.gateway`

`spec.dhcp.mode` is `proxy` or `lease`:

- `proxy`: the production DHCP server of the segment keeps all
  ownership of leases. bootd answers only the PXE part of the
  exchange.
- `lease`: bootd is the DHCP lease authority of the segment. Use this
  for a segment that has no DHCP server of its own.

`dhcp.gateway` is the router option (DHCP option 3) that bootd hands
out. It has three states, and each state has a different result:

| State | In `lease` mode | In `proxy` mode |
|---|---|---|
| Absent | Rejected at admission. | Accepted. bootd hands out no router option. |
| Empty string `""` | Accepted. bootd hands out no router option. Use this for a segment with no exit. | Accepted. It means the same as absent. |
| An IPv4 address | Accepted. bootd hands out this address as the router of the segment. | Rejected at admission. |

Lease mode makes the field mandatory on purpose. If the field stays
optional, dnsmasq fills the gap. It advertises the address of bootd
itself. bootd is a pod, and it forwards nothing. The machines then
receive a default route into a black hole. This defect shows itself
only after a Site gets a second segment, far away from its cause.

Set an address whenever the seeder or the tracker of the Site is on a
different Subnet. The router option is the only thing that tells a
machine how to get there.

### `dhcp.leaseTime`

`spec.dhcp.leaseTime` sets the DHCP lease duration bootd hands out in
`lease` mode, for example `30m` or `1h`. Left unset, it defaults to 30
minutes. The admission schema rejects anything under 2 minutes.

Ignored in `proxy` mode: the segment's own DHCP server owns lease
lifetime there.

Operational risk: if bootd is unavailable for longer than `leaseTime`, a
machine mid-deploy loses its address once its lease expires with nobody
to renew it against. A short lease time shrinks that safety margin.

### The bootd lease PVC

A `lease`-mode Subnet gets a second object alongside its bootd
Deployment: a PersistentVolumeClaim named `kezio-bootd-<subnet>-leases`,
`ReadWriteOnce`, 64Mi, owned by the Subnet (it is deleted with the
Subnet, taking every persisted lease with it). It is mounted into the
bootd pod and holds dnsmasq's lease file alone - the rest of bootd's
writable state (its rendered config, the dhcp-hostsfile MAC allowlist)
still lives on an ephemeral `emptyDir`.

Without this PVC, every pod recreate (an upgrade, a rollout, a node
eviction, or switching a Subnet between `lease` and `proxy` mode and
back) would forget every lease dnsmasq had handed out. With it, leases
outlive the pod.

A `proxy`-mode Subnet gets no lease PVC: proxy mode never runs its own
lease file at all.

The bootd Deployment's update strategy is `Recreate` in `lease` mode (the
default `RollingUpdate` otherwise): the lease PVC is `ReadWriteOnce`, and
dnsmasq is meant to be the segment's sole DHCP authority, so the old pod
is torn down before a new one starts rather than both briefly running.

### What releases a DHCP lease

In `lease` mode, a lease ends one of four ways:

- **Expiry.** dnsmasq's own lease timer, unchanged by anything below.
- **An active DHCPRELEASE bootd sends** when a MAC that held a lease
  drops out of the enrolled-Machine allowlist - the Machine was deleted,
  or its `spec.bootMACAddress` changed away from that MAC. The address
  returns to the pool immediately rather than waiting out `leaseTime`.
- **An active DHCPRELEASE bootd sends when a Machine's `spec.subnetRef`
  changes to a different Subnet.** The MAC stays in the cluster-wide
  allowlist (the Machine is still enrolled), but its reservation
  disappears from this Subnet's own `status.dhcp.reservations`. bootd
  tells this case apart from an ordinary Complete release (below) by
  checking, through its own Machine informer, whether the Machine still
  targets this Subnet: if it does not, the address is released instead
  of left to expire. A bootd instance with no Subnet identity configured
  (`BOOTD_SUBNET_NAME` unset) cannot make that check and never releases
  from a reservation disappearing alone.
- **The startup filter.** On every bootd start in `lease` mode, once the
  Machine allowlist's first sync completes, bootd rewrites the persisted
  lease file to drop any lease held by a MAC no longer in that allowlist,
  before dnsmasq ever reads it back - so a lease surviving a bootd
  restart on behalf of a Machine deleted while bootd was down does not
  get silently renewed.

What does **not** release a lease: a Machine's ordinary
Deployed-to-Complete transition. Its reservation disappears the same way
a `spec.subnetRef` change's does, but the Machine still targets this
Subnet, so bootd's check above tells them apart and leaves the lease
alone - the completed deployment's operating system keeps renewing it as
normal DHCP traffic.

In proxy mode bootd is not the DHCP server. It cannot hand out a
router option that the DHCP server of the segment owns. kezio rejects
a non-empty address there, and does not ignore it. If kezio ignored
it, an operator could write an address, believe that the machines
receive it, and find out much later that they do not.

## 2. Content

### ImageImport

An `ImageImport` is the request to turn one source disk image into a
set of `PartitionContent` objects, and the `Image` that binds them.

One import runs partclone exactly once, in the cluster. Only that run
can know the partition table, the role and file system of each
partition, and the size of each content. For this reason nothing in
the spec describes them.

| Field | Purpose |
|---|---|
| `spec.source.url` | Where the ingest Job fetches the source disk image from. |
| `spec.source.checksum` | `sha256:<hex digest>`. Ingest verifies the fetched image before it converts it. |
| `spec.imageName` | The name of the `Image` this import creates, in its own namespace. |
| `spec.contentPrefix` | The name prefix of the content this import captures. Partition N becomes `<contentPrefix>-p<N>`. |
| `spec.osFamily` | Copied onto `Image.spec.osFamily`. `Linux`, `Windows`, `FreeBSD`, or `Other`. Default `Linux`. |
| `spec.bootable` | Copied onto `Image.spec.bootable`. Default true. |
| `spec.params` | Copied onto `Image.spec.params`. |
| `spec.postHookRefs` | Copied onto `Image.spec.postHookRefs`. |
| `spec.scratchSize` | Overrides the size of the ingest scratch volume. Left unset, the manager sizes it from the source image: by default the ingest Job attaches the source with `qemu-nbd` and writes only each partition's cloned content to scratch, so about 1.2 times the source size is enough; `IMAGE_INGEST_UNPRIVILEGED=true` falls back to the original copy pipeline (about twice the source size for a raw source, three times for one that ingest must convert first). Either way, a source ingest cannot size falls back to a floor. |
| `spec.ttlSecondsAfterFinished` | How long a `Succeeded` or `Failed` import stays around after `status.completionTime`, before the controller deletes it. Left unset, a finished import is kept forever. Set only at creation - the spec is immutable. |
| `status.state` | `Pending`, `Ingesting`, `Succeeded`, or `Failed`. |
| `status.imageRef` | The `Image` this import created. |
| `status.contentRefs` | Every `PartitionContent` this import created, in partition order. |
| `status.completionTime` | When the import first reached `Succeeded` or `Failed`. Absent while the import is in progress. |

The spec is immutable after creation.

The import fails if a name it must create is already taken. It never
writes over an existing content, and never writes over an existing
Image.

Deleting a finished `ImageImport` (by hand, or once
`spec.ttlSecondsAfterFinished` elapses) never deletes the `Image` or
the `PartitionContent` objects it created. Both outlive the import
that captured them.

A swap partition gets no content. The import gives it a blank slot
that carries only its file-system UUID. The agent runs `mkswap` on it.

When `spec.source.url` is a `kezio-staged://<name>` reference (a
`kezioctl image upload`), the ingest Job removes that staged upload
from the staging volume once every partition's content has been
safely captured to the ingest scratch PVC - the last step of a
successful ingest run. A failed ingest leaves the staged upload in
place: the source may still be needed to retry, and cleanup only ever
runs after the content it was there to produce is durably stored.
Cleanup is best-effort - a failure to remove it does not fail an
otherwise-successful import - but the staging volume mount itself
must be read-write for it to ever succeed at all.

### PartitionContent

A `PartitionContent` is the immutable record of the data of one
partition.

| Field | Purpose |
|---|---|
| `spec.fsType` | The file system of the partition, for example `ext4`. Empty when the partition carries no file system that partclone recognizes. Recorded for audit only. |
| `spec.usedBytes` | The number of bytes of real data in the partition, measured at capture. |
| `spec.sizeBytes` | The size of the partition at capture. |
| `spec.lastExtentEnd` | The end offset of the highest extent that ingest wrote. Extents write at absolute offsets, so a target partition must be at least this large. |
| `spec.pieceLength` | The BitTorrent piece length used to hash this content. A pinned constant. |
| `spec.source.importName` | The `ImageImport` that captured this content. |
| `spec.source.partitionNumber` | The partition of the source disk this content comes from. Audit data only. |
| `status.state` | `Pending`, `Publishing`, `Ready`, or `Failed`. |
| `status.infoHash` | The BitTorrent v1 info hash, in lowercase hex. Absent until the publish succeeds. |
| `status.pvcRef` | The PVC that holds the bytes of this content. |
| `status.seeders` | One entry per Site that has an available seeder for this content, with the number of machines at that Site that deploy an Image which references it. |

The spec is immutable. The user chooses the name, so the name pins
nothing about the bytes. An `Image` slot that references the name must
be able to trust that the bytes behind it never change. Immutability
is what gives that trust.

Each content owns its own PVC and its own publish Job. Both carry an
owner reference back to the content, so Kubernetes removes them when
the content goes away.

Every `Image` whose layout references a `PartitionContent` adds its
own (non-controller) owner reference to that content, alongside the
same reference any other `Image` sharing it already holds. This is
what lets an orphaned content actually get cleaned up: once every
`Image` that ever named it is deleted, Kubernetes garbage collection
marks the content itself for deletion (and, transitively, its PVC and
publish Job) - in place of nothing ever removing it. A content named
by more than one `Image` survives any one of them being deleted; only
the last reference clears it. This ownership is additive only - an
`Image`'s layout is immutable, so a slot's `contentRef`, once set,
never needs to be un-referenced later.

`PartitionContentFinalizer` (`kezio.kojuro.date/partitioncontent`)
still holds the actual removal of a content while an `Image` slot or
an active `DeployRun` references it - unchanged by the owner
reference above, which only gets the content marked for deletion in
the first place. The `DeletionBlocked` condition reports this hold.
Once unblocked, the finalizer also deletes the content's own publish
Job outright (rather than waiting on the Job's
`ttlSecondsAfterFinished`, below) before it clears, so a content's
deletion never waits on that TTL to free the ingest scratch PVC the
Job's pod still mounts.

Conditions: `Ready` (the `.torrent` exists and the content is
seedable), `Valid` (spec-level validity only; it makes no claim about
the integrity of the bytes), `SeederDegraded`, and `DeletionBlocked`.

### Image

An `Image` binds a disk layout to an ordered list of slots. Each slot
optionally references a `PartitionContent`.

| Field | Purpose |
|---|---|
| `spec.osFamily` | `Linux`, `Windows`, `FreeBSD`, or `Other`. Default `Linux`. It gates OS-specific validation and builtins. |
| `spec.bootable` | Default true. A bootable image needs an ESP slot, and finalize creates a boot entry for it. Set false for a data disk or a scratch layout. |
| `spec.layout.partitionTable` | `gpt` or `mbr`. |
| `spec.layout.sfdiskJSON` | The `sfdisk --dump --json` output that describes the partition table. `sfdisk` accepts it back to recreate the table. |
| `spec.layout.slots` | The ordered list of slots, in partition-table order. |
| `spec.params` | Schemaless input for the templating of the attached hooks. |
| `spec.postHookRefs` | An ordered list of `PostHook` objects that the content of this image needs. |
| `status.state` | `Pending`, `Ready`, or `Failed`. |

Each slot has these fields:

| Field | Purpose |
|---|---|
| `number` | The partition number, 1-based. Unique in the slot list. |
| `role` | `esp`, `data`, `swap`, or `msr`. The roles are OS-neutral. |
| `contentRef` | The `PartitionContent` this slot restores. Absent makes this a blank slot. |
| `fsType` | The file system to create at deploy time, for a blank `data` slot. |
| `uuid` | The file-system UUID to restore, for a `swap` slot that has no content. |
| `typeGUID` | The partition type this slot is written with. |
| `partUUID` | The unique GPT partition GUID. Empty for an `mbr` table. |
| `sizeBytes` | The size of this slot, when it is known before deploy time. |

`contentRef` and `fsType` are mutually exclusive. The schema rejects a
slot that sets both. Restored content already carries its own file
system.

`contentRef.namespace`, if it is set, must equal the namespace of the
Image. The webhook denies any other value, because the deletion
finalizer of `PartitionContent` looks only for a referencing Image in
the namespace of the content.

`typeGUID`, `partUUID`, and `sizeBytes` carry no restore behavior of
their own. They are present so that a future adopt mode can match a
slot against partitions that are already on a target disk.

The whole spec is immutable after creation. A `contentRef` binds into
a BitTorrent swarm. If the layout could change, an Image that is in
use could start to describe different content, and nothing would say
so. To publish a different layout or content set, create an Image with
a new name.

An `Image` never ingests anything. It is always a declaration over
`PartitionContent` objects that already exist. An `ImageImport`
creates the Image only when it knows the whole layout, so the spec is
complete at creation, and nothing patches it later. A slot can
therefore never reference content that does not exist.

The Image reconciler is thin. It aggregates the readiness of the
referenced content, and it drives the per-Site seeder Deployments.

A Site's seeder Deployment for this Image starts once any `Machine`
there has a bound `MachineClaim` naming it. It stops, after the grace
period, once every deploy of it at that Site has finished: no bound
Machine there is still `Enrolling`, `Inspecting`, `Available`, or
`Provisioning`, no `Provisioned` Machine there still has a pending
re-provision (a claim edit or hook change the provisioning trigger has
not yet acted on), and no active `DeployRun` there names it either.

Conditions: `Ready`, `Valid` (every referenced content resolves, and
its `lastExtentEnd` fits the slot), and `SeederDegraded`.

## 3. Machines and deploys

### Machine

A `Machine` is one bare-metal machine.

| Field | Purpose |
|---|---|
| `spec.bmc.address` | The BMC endpoint URL. The scheme selects the driver, for example `redfish://` or `ipmi://`. Required. |
| `spec.bmc.credentialsSecretRef` | The Secret that holds the BMC user name and password. Required. |
| `spec.subnetRef` | The `Subnet` this machine network boots through. Required. A Machine with no Subnet cannot network boot. |
| `spec.bootMACAddress` | The MAC address of the NIC that network boots. The MAC gate (bootd's dnsmasq hostsfile) answers only an enrolled MAC, so this field must normally already be set before a Machine can PXE-boot at all - inspection cannot discover a MAC it needs the MAC gate open to reach. The one exception is a Subnet whose bootd runs with the MAC gate disabled (`BOOTD_ANSWER_ALL`, an answer-all Subnet, for a deliberately inventory-only lab): there, any machine boots and inspection discovers the MAC from the agent's own registration. It is mandatory (schema-enforced) only when the inspect-disable annotation skips inspection outright. |
| `spec.claimRef` | The `MachineClaim` bound to this machine. Written only by the claim controller. |
| `spec.console` | Kernel `console=` values for the live environment, in order, for example `["ttyS0,115200n8", "tty0"]`. The last value is the primary console. A hardware attribute, like BMC - not deploy intent. Empty falls back to the boot server's default. Optional, up to 4 entries. |
| `spec.bootTimeout` | The longest time kezio waits from power-on until the agent registers, for this Machine only. It sets the boot token lifetime (see below). It can only extend the operator-wide default; a shorter value has no effect. Optional. |

A `Machine` carries no deploy intent: no image, no disk hint, no
hooks. That intent lives on a `MachineClaim` bound to it - see
[MachineClaim](#machineclaim) below.

**There is no power intent field on a Machine.** Power follows the
deploy lifecycle, and the bound claim's `afterDeploy` mechanism. To
reboot a machine outside that lifecycle, use the reboot annotation.

The `Site` of a Machine is derived, and never written by hand. kezio
follows `Machine.spec.subnetRef` to the `Subnet`, then
`Subnet.spec.siteRef` to the `Site` (`internal/sitederive`). This
closes a class of misconfiguration. With a hand-written field, a
machine could boot from the bootd of one segment but declare a
different Site, and then leech from a distant seeder instead of the
one beside it.

`status.state` is `Enrolling`, `Inspecting`, `Available`,
`Provisioning`, `Provisioned`, `Deprovisioning`, `PoweringOff`, or
`Released`. `Deprovisioning` and `PoweringOff` occur only after a
deletion timestamp is set. `Released` means the bound claim is gone
and the machine's disks still hold whatever that claim wrote - kezio
erases nothing. An operator returns a `Released` machine to
`Available` with the re-inspect annotation. There is no Error state.
`status.operationalStatus` (`OK`, `error`, `delayed`, `detached`) is a
separate axis, so a failure never erases the position of the machine
in the workflow.

Other status fields include `currentRunRef`, `lastSuccessfulRunRef`,
`lastAttemptedRunRef` (the last DeployRun that ended, successful or
not - the only reference that can name a failed run),
`poweredOn`, `lastUpdated`, the observed BMC credentials, and the
hashes of the net-boot token and the agent session token. kezio stores
only the hash and the expiry of each token on the Machine itself, and
never the token.

The net-boot token's plaintext lives only in the manager's memory and in
a `Secret` named `<machine>-boot-token`, in the Machine's own namespace.
kezio writes this Secret at the same time it arms a net boot, and owns
it with a controller reference, so Kubernetes removes it when the
Machine is removed. The boot config server reads this Secret only when
its own in-memory copy is missing - after a manager restart, for
example - so a machine that starts its boot before the restart can still
register afterward, instead of waiting out the full inspection timeout.

`status.netBoot.expiresAt` is also the only clock kezio waits on for a
machine to boot into the agent: inspection and provisioning both give up
once the boot token they minted for the current attempt expires, rather
than tracking a second, independent deadline. The token's lifetime
defaults to one hour (`BOOT_TOKEN_TTL` on the manager raises it for every
Machine); `spec.bootTimeout` raises it further for one Machine whose
hardware needs longer, for example slow POST or firmware initialization
on the way to a net boot.

Conditions: `Ready`, `Progressing`, `AgentCompatible`,
`AgentRegistered`, `StatusLossHold`, and `RetryHeld`.

A Machine stops retrying and holds for an operator once it hits 3
consecutive failures with `errorType: Restart` in its current state
(`status.restartCount`, a subset of `status.errorCount` that only ever
counts `Restart` failures - a `Transient` failure in between does not
consume one of the three). A held machine shows `operationalStatus:
error`, condition `RetryHeld: True`, and a Warning Event (reason
`BootRetryExhausted`); the controller stops calling the deployer
entirely, so it never re-arms PXE, never power-cycles the machine again,
and never touches the machine's DHCP reservation or its bound claim -
both stay exactly as they were at the moment of the 3rd failure. Set
`kezio.kojuro.date/clear-error` to release the hold: the controller
clears `operationalStatus`/`errorType`/`errorMessage`, zeroes both error
counters, and resumes the state walk immediately. The annotation is
consumed whether or not the machine was actually held - outside a hold it
is a plain "clear this error and retry now".

Annotations, read directly off the object:

| Annotation | Result |
|---|---|
| `kezio.kojuro.date/paused` | The reconciler returns at once. No status writes, no deployer calls, no requeue. It also blocks the delete walk. |
| `kezio.kojuro.date/detached` | The controller calls no deployer action, but keeps status current. `operationalStatus` reports `detached`. |
| `kezio.kojuro.date/reboot` | Asks for a reboot. The value is `{"mode":"hard"}` or `{"mode":"soft"}`. A `-<client>` suffix gives a client its own copy of the annotation. |
| `kezio.kojuro.date/inspect-disable` | Set to exactly `true`, it skips hardware inspection. The webhook then requires `spec.bootMACAddress`. |
| `kezio.kojuro.date/re-inspect` | Asks for a new inspection. The controller consumes the annotation, deletes the existing `MachineHardware`, and emits an Event. |
| `kezio.kojuro.date/confirm-status-loss` | Releases the `StatusLossHold` condition. The controller consumes the annotation. |
| `kezio.kojuro.date/bmc-insecure-skip-verify` | Set to exactly `true`, the BMC connection does not verify the TLS certificate. `false` means verify. The webhook refuses every other value. |
| `kezio.kojuro.date/clear-error` | Releases the `RetryHeld` condition (if present) and clears the current error: `operationalStatus`, `errorType`, `errorMessage`, `errorCount`, and `restartCount` all reset, and the walk resumes at once. The controller consumes the annotation. |

### MachineClaim

A `MachineClaim` holds the deploy intent: which Image, which data
images, which disk, which hooks. Give it a `Machine`, and it deploys.

| Field | Purpose |
|---|---|
| `spec.machineName` | Binds the claim to exactly one `Machine` by name. Mutually exclusive with `spec.selector`. |
| `spec.selector` | Chooses a candidate `Machine` by label and, optionally, reported hardware, instead of naming one. Mutually exclusive with `spec.machineName`. |
| `spec.imageRef` | The `Image` to deploy as the OS. It must be bootable. It may be absent when the claim deploys only data images. |
| `spec.dataImages` | Additional non-OS images, deployed in the same live session. Each entry has its own `imageRef` and `targetDisk`. |
| `spec.targetDisk` | The hints that select the disk for the OS image. Ignored when `imageRef` is absent. |
| `spec.postHookRefs` | An ordered list of `PostHook` objects attached to this claim's deployment. |
| `spec.params` | Schemaless input for the templating of the attached hooks. |
| `spec.ezio` | Per-machine overrides of the cluster-wide ezio tuning of the leecher. |
| `spec.afterDeploy` | `Reboot` or `PowerOff`. Default `Reboot`. It applies only when the deployment ends with no OS image to reboot into. |

`spec.targetDisk` and each `dataImages[].targetDisk` hold disk hints:
`deviceName`, `serialNumber`, `wwn`, `model`, `vendor`,
`minSizeGigabytes`, `maxSizeGigabytes`, `rotational`, `pciePath`,
`hctl`, and `slotNumber`. All given fields must match the same disk.
The controller matches the hints against the reported disk inventory,
and needs exactly one match before it writes anything. All disks
resolved for one claim must be different from each other.

`spec.ezio.maxUploads` and `spec.ezio.maxConnections` override the
operator's cluster-wide leecher default. `spec.ezio.cacheSizeMB`,
`spec.ezio.aioThreads`, and `spec.ezio.port` have no cluster-wide
default: they pass straight through to the agent's local ezio daemon as
`--cache-size`, `--aio-threads`, and `--port`, and an absent field
omits its flag. An absent `cacheSizeMB` does not fall back to ezio's own
built-in cache size. Instead, the agent computes one from the machine's
own memory, right before it execs ezio: it holds back 2 GiB (1 GiB for
the OS, the agent, and the live image, plus 1 GiB for ezio's own fixed
buffer pools), then uses half of what remains, in megabytes, clamped to
64-8192 MiB. The other half stays free for the kernel page cache. A
machine that reports no readable memory total gets no `--cache-size`
flag at all, and ezio's own built-in default applies.

The claim controller binds a `Pending` claim to a `Machine` (by
`spec.machineName`, or by resolving `spec.selector` against candidate
Machines), then writes `Machine.spec.claimRef` back and sets
`status.phase` to `Bound`. `status.machineName` and `status.boundAt`
record what it bound to and when. `status.phase` is `Failed` only when
`spec.machineName` names a `Machine` that does not exist - a state
that never retries on its own.

Deleting a `MachineClaim` releases its `Machine`: the claim controller
clears `Machine.spec.claimRef` and the Machine moves to
`status.state` `Released`, disks untouched. Only the re-inspect
annotation brings a `Released` machine back to `Available`.

`status.currentRunRef` and `status.lastSuccessfulRunRef` name the
`DeployRun` in flight and the most recent one that succeeded, for this
claim's intent.

Conditions: `Bound` and `Ready` (the bound machine reached
`Provisioned` for this claim's current intent).

### MachineHardware

A `MachineHardware` is the hardware inventory that the agent reports
at registration. It has the same name as its `Machine`, it is in the
same namespace, and the `Machine` owns it.

It carries no status, because it is itself the observed state.

`spec.disks` lists the disks. The field set matches the disk hints of
a `Machine`, so a hint matches an entry with no unit conversion.
`spec.nics` lists the network interfaces. `spec.memoryBytes` and
`spec.cpuCount` complete the inventory. Every field is optional. The
object starts empty, and the controller fills it in when inspection
ends.

### DeployRun

A `DeployRun` is the resolved snapshot of one deployment attempt. The
Machine reconciler writes the spec once, at creation. The `Machine`
owns the object.

| Field | Purpose |
|---|---|
| `spec.machineRef` | The `Machine` this run deploys to. |
| `spec.claimRef` | The `MachineClaim` this run serves. The run outlives the claim - this is a record of who asked for it, not a dependency the run needs kept alive. |
| `spec.imageRef` | The OS `Image` resolved for this run, copied from `MachineClaim.spec.imageRef`. Absent for a run that has only data images. |
| `spec.dataImages` | The resolved non-OS image list. |
| `spec.resolvedDisks` | The disk each image resolved to, against the inventory known at creation. |
| `spec.hooksHash` | A content hash of every resolved hook step for this run. |
| `status.phase` | `Pending`, `Partitioning`, `WritingContent`, `RunningPostHook`, `Finalizing`, `Succeeded`, or `Failed`. |
| `status.partitions` | One entry per partition this run writes, with a percentage and a byte count. |
| `status.phaseTimings` | The start time and the end time of each phase the run entered. |

The spec is immutable, because it is the historical record of what
this attempt was resolved to run.

The trigger that starts a new run compares the current deploy intent
against the snapshot of the last successful run. `spec.resolvedDisks`
is excluded from that comparison. Device names drift between boots, so
a difference there alone must never start a new run. `spec.hooksHash`
is included, so a change to a hook does start a new run.

Condition: `Succeeded`. It is absent while the run is in progress.

### PostHook

A `PostHook` is a named, reusable, ordered list of steps. A `Machine`
or an `Image` attaches it. The steps run after the content is written.

| Field | Purpose |
|---|---|
| `spec.params` | The named inputs the steps can reference through templating, each with an optional default. |
| `spec.steps` | The steps, in list order. At least one step is required. |

Each step is exactly one of a builtin or a script. A step can also set
`osFamily` to restrict itself to one target OS family.

The builtins are:

| Builtin | Action |
|---|---|
| `mkswap` | Runs `mkswap` with the saved UUID of the source partition. |
| `efibootmgr` | Creates a UEFI NVRAM boot entry on the ESP of the target. |
| `growLastPartition` | Grows the last partition and its file system when the target disk is larger than the source. |
| `install-removable-fallback` | Copies a shim or grub bootloader onto the removable-media fallback path of the ESP. |

The boot entry that `efibootmgr` writes points only at the
removable-media fallback path: `\EFI\BOOT\BOOTX64.EFI` on x86_64, and
`\EFI\BOOT\BOOTAA64.EFI` on aarch64. kezio selects the path from the
architecture of the machine, and never examines the loader path of a
distribution. The image must supply the fallback file. See the
boot-entry contract section of
[`docs/physical-lab-deployment.md`](physical-lab-deployment.md).

A script step takes its content from exactly one of `script` (inline),
`configMapRef`, or `secretRef`. The manager fetches and templates the
content, so the agent needs no cluster access.

A script runs in the live environment of the agent, and never in the
deployed OS. The target disk holds its content, but no file system on
it is mounted. The agent gives the device paths to the script through
the environment: `KEZIO_TARGET_DISK`, `KEZIO_PARTITIONS`,
`KEZIO_PART_<number>`, `KEZIO_DATA_DISKS`, and the related
`KEZIO_DATA_DISK_<index>` variables. A script that mounts a device
must unmount it before the script ends. A mount that stays can disturb
the steps that follow, and the reboot into the deployed disk.

Each step has a `timeoutSeconds`. The default is 60.

The steps can reference three reserved names without a declaration:
`machineName`, `imageName`, and `targetDisk`. The plan builder injects
them.

Params merge in this order: the declared defaults of the PostHook,
then `Image.spec.params`, then `MachineClaim.spec.params`. A later
entry overrides an earlier one.

Conditions: `Ready` and `Valid`.

### Hook order, and the shipped default hook

kezio ships one default `PostHook`, named `kezio-default-finalize`, in
the namespace of the manager. Its steps are `mkswap`,
`install-removable-fallback`, and `efibootmgr`, all Linux-only.

Hooks run in this order: the `postHookRefs` of the OS `Image` first,
then the `postHookRefs` of the `MachineClaim`.

kezio substitutes the default hook only when **all** of these hold
(`internal/planbuild`, `resolveMachineHooks`):

- `MachineClaim.spec.postHookRefs` is empty, and
- the `postHookRefs` of the OS Image is empty, and
- the claim deploys an OS image.

**A caller that names any hook opts out of the substitution.** This
holds for a hook named on the claim, and for a hook named on the
Image. If the caller still wants a boot entry, the caller must name
`kezio-default-finalize` as well, or supply the equivalent steps.

A run that has only data images and no `postHookRefs` on either side
resolves no hooks at all. Such a run ends at its after-deploy power
state, with no OS to boot, so the boot-entry builtins would have no
ESP to act on.

## 4. The workloads kezio creates

| Workload | Scope | Owner | Network |
|---|---|---|---|
| bootd Deployment | One per `Subnet` that declares a boot half | The `Subnet` | Attached to `bootdNetworkRef`, pinned at `bootdServerIP` |
| Seeder Deployment | One per (`Image`, `Site`) | The `Image` | Single-homed on the seeding network of the Site |
| Tracker Deployment | One per `Site` that sets `tracker.ip` | The `Site` | Single-homed on the seeding network of the Site |
| Ingest Job | One per `ImageImport` | The `ImageImport` | The cluster network |
| Ingest scratch PVC | One per `ImageImport` | The `ImageImport` | - |
| Publish Job | One per `PartitionContent` | The `PartitionContent` | The cluster network |
| Content PVC | One per `PartitionContent` | The `PartitionContent` | - |

The ingest scratch PVC outlives the ingest Job. The publish Job of a
`PartitionContent` reads the already-sliced content out of the scratch
PVC of the `ImageImport` that `spec.source.importName` names. Only the
deletion of the `ImageImport` reclaims that PVC, so do not delete an
import until every content it created is Ready.

Both the ingest Job and every content's publish Job set
`ttlSecondsAfterFinished` (an hour by default; `IMAGE_INGEST_JOB_TTL`
and `PARTITIONCONTENT_PUBLISH_JOB_TTL` on the manager override it), so
a completed Job's pod does not linger forever. A `Succeeded`
publish-Job pod still mounts the ingest scratch PVC (read-only) it
read content from, and `pvc-protection` refuses to finish deleting
that PVC while any pod still references it - deleting the
`PartitionContent` itself does not wait on this TTL (see the
`PartitionContent` section above), but deleting the `ImageImport`
before every content it produced has published, or before those
contents' own publish Jobs have aged out, can still leave the scratch
PVC `Terminating` until they do.

**The ingest Job is privileged by default.** It attaches its source
image to a kernel `nbd` device with `qemu-nbd` and reads partitions
straight off that device, so it runs as root with
`securityContext.privileged: true` (the container's device cgroup admits
`/dev/nbd*` only for a privileged pod) and a hostPath mount of the
node's `/dev` - and every node the ingest Job
can land on must have the `nbd` kernel module loaded with partition
support, e.g. `modprobe nbd max_part=16`. Without that module loaded,
the ingest Job fails fast with a clear error rather than hanging. Set
`IMAGE_INGEST_UNPRIVILEGED=true` on the manager to opt out: the ingest
Job then runs with no elevated privilege at all (the original
raw-conversion-and-file-copy pipeline), at the cost of a larger scratch
PVC (see `spec.scratchSize` above).

A seeder Deployment is per (`Image`, `Site`), and not per Site. A Site
that deploys several Images at the same time therefore needs that many
addresses on its seeding network attachment at the same time, plus the
pinned address of the tracker.

**No Service fronts a seeder or a tracker.** A ClusterIP would DNAT
the connection. A BitTorrent peer connects to the address that the
announce response gives it, so that address must be the address the
pod listens on. Both workloads are single-homed with a Multus
default-network annotation for the same reason. See
`docs/network-model.md` for the full no-NAT rule.

## Summary of the rules that are easy to get wrong

- The `Site` of a Machine is derived through
  `Machine.spec.subnetRef` and `Subnet.spec.siteRef`. There is no
  Site field on a Machine.
- `dhcp.gateway` is mandatory in `lease` mode. An empty string is a
  decision. An absent field was an oversight.
- A slot cannot set both `contentRef` and `fsType`.
- The specs of `ImageImport`, `PartitionContent`, `Image`, and
  `DeployRun` are all immutable after creation.
- Any `postHookRefs` on a MachineClaim or on its OS Image stops the
  substitution of `kezio-default-finalize`.
- A Machine carries no deploy intent, and no power intent field. Both
  live on the MachineClaim bound to it.
- Deleting a MachineClaim releases its Machine to `status.state`
  `Released`; the re-inspect annotation is the only way back to
  `Available`.
- A `Subnet` must declare a boot half, or `seederNetworkRef`, or both.
- A seeder is per (`Image`, `Site`). A tracker is per `Site`.
- `spec.bootMACAddress` must normally be set before a Machine can
  PXE-boot at all - inspection cannot discover it unless the Subnet
  disables the MAC gate (`BOOTD_ANSWER_ALL`).
- A lease-mode Subnet's `status.dhcp.reservations` only ever holds
  Machines currently net-booting or deployed - never every enrolled
  Machine.

## See also

- [`docs/network-model.md`](network-model.md): what a `Site`
  guarantees and does not guarantee, the no-NAT rule, and the
  address-pool sizing rule.
- [`docs/physical-lab-deployment.md`](physical-lab-deployment.md): the
  operational setup, including both DHCP scenarios.
- The types themselves, in `api/v1alpha3/`. Their doc comments are the
  authority for anything this document does not cover.
