# Manual physical lab deployment guide

This guide is for an operator who builds a kezio lab on real hardware,
with no CI and no GitHub Actions. It lists the network shapes kezio
supports today, the prerequisites the operator must set up, and the
full set of ports and addresses the operator must open and verify.

For a lab, read `docs/lab-proxmox-rke2.md` first: it makes one choice at
every point this guide gives options for, and walks through a full
bring-up on Proxmox VE - RKE2, a Redfish shim in front of the Proxmox
API, and a first deployed machine. Come back here for the reasoning
behind a step, or when your site does not match that shape.

Read `docs/network-model.md` before this guide: it defines what a
`Site` and a `Subnet` are and guarantee, and every prerequisite below is
a consequence of that model. Read this guide together with:

- `docs/network-model.md` - the Site/Subnet model, the no-NAT rule, and
  the address-pool sizing rule.
- `docs/bmc.md` - the BMC drivers.
- `docs/secure-boot.md` - the UEFI Secure Boot chain.

Every claim in this guide is checked against the code and manifests
that implement it. Where a behavior is planned but not built yet, this
guide says so.

## 1. The two DHCP scenarios

kezio-bootd never assigns an IP address itself, by default. It answers
only the PXE part of a boot exchange: which file to fetch over TFTP,
and from which server. The two scenarios below cover the two ways an
operator can supply the IP lease. Pick the scenario that matches each
`Subnet` before deploying its boot half.

### Scenario 1: existing DHCP on the same L2 (default, lab-proven)

Use this scenario when the segment already runs a DHCP server on the
same broadcast domain as the provisioning `Subnet`.

bootd's proxyDHCP answers PXE requests only. It never touches lease
traffic. The segment's own DHCP server keeps sole ownership of leases,
and both services run on the segment at the same time with no conflict.
Setup: set `Subnet.spec.dhcp.mode` to `proxy`
(`SubnetDHCPModeProxy`, `api/v1alpha2/subnet_types.go`). This is the
shape kezio's own `e2e-routed-site` and `e2e-two-site-concurrent`
GitHub Actions lanes exercise end to end (see section 8).

### Scenario 2: isolated segment (own DHCP server mode)

Use this scenario when the provisioning segment has no DHCP server of
its own - bootd itself must hand out IP leases.

Set `Subnet.spec.dhcp.mode` to `lease`
(`SubnetDHCPModeLease`). bootd's dnsmasq then renders a lease-serving
`dhcp-range` (start/end host addresses, no `proxy` flag) instead of the
proxyDHCP range, and becomes the segment's own DHCP authority
(`api/v1alpha2/subnet_types.go`'s `SubnetDHCP`). The lease range
defaults to the `Subnet`'s CIDR-derived first and last host addresses;
set `spec.dhcp.leaseRangeStart` and `spec.dhcp.leaseRangeEnd` together to
override it (the CRD schema rejects setting only one).

The MAC gate does **not** relax in this mode: only enrolled MACs
receive a lease. A device that is not an enrolled Machine gets nothing
at all, even in an otherwise DHCP-server-less segment - it is out of
scope for bootd to serve it, and this mode does not turn bootd into the
segment's general-purpose DHCP server. An operator who also needs to
hand out addresses to unenrolled devices must put those devices on a
different segment; kezio does not support mixing enrolled and
unenrolled DHCP traffic on the same lease-mode segment.

This mode is proven at packet level by `hack/bootd-packet-lab.sh`
(`internal/bootd/lab_test.go`'s `TestDnsmasqLab`, with
`BOOTD_LAB_LEASE_MODE=1`): an enrolled MAC's DHCPDISCOVER gets back a
DHCPOFFER carrying a real, non-zero leased address and the boot
filename; a denied MAC gets nothing, the same MAC-gate outcome as
scenario 1. No KubeVirt e2e lane exercises lease mode end to end today.

## 2. Network prerequisites the operator owns

Read `docs/network-model.md` first for the full Site/Subnet model.
Every prerequisite below is a consequence of it.

- **bootd binds to a broadcast domain.** proxyDHCP and TFTP only work on
  the segment they answer on, so `SubnetReconciler`
  (`internal/controller/subnet_controller.go`) creates one bootd
  Deployment per `Subnet` that declares a boot half, in that `Subnet`'s
  own namespace.
- **The seeder binds to locality and bandwidth**, not a broadcast
  domain - dragging image content across a WAN link defeats the point
  of a local seeder. kezio instead runs one seeder Deployment per
  `(Image, Site)` (`internal/controller/image_seeder.go`); every
  `Subnet` inside that Site routes to it.

A machine's Site is never hand-entered; it is derived:
`Machine.spec.subnetRef -> Subnet.spec.siteRef -> Site`
(`internal/sitederive`). Reassigning a `Subnet` to a different `Site`
re-resolves the seeder for every machine on that segment - treat this
as accepted behavior, not something to guard against.

### 2.1 Provisioning L2 per Subnet

Each broadcast domain a group of target machines' boot NICs share (a
VLAN or a dedicated switch/bridge) needs its own `Subnet` object.
`SubnetReconciler` creates and keeps current exactly one bootd
Deployment per boot-half `Subnet`, in that `Subnet`'s own namespace,
pinned to `replicas: 1` - two bootd pods answering the same broadcast
domain would both reply to every DHCPDISCOVER, and firmware cannot
prefer one answer over the other.

A production deployment with more than one site or VLAN creates one
`Subnet` per segment, each carrying its own `bootdServerIP` / `cidr` /
`bootdNetworkRef`. Every `Subnet`'s bootd Deployment proxies to the same
cluster-wide agent and boot config servers, once
`BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL` and
`BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` are set on the controller-manager.

### 2.2 Multus NAD wiring for bootd

bootd's pod needs a second network interface on the provisioning
segment, attached with Multus, not `hostNetwork`
(`config/bootd/networkattachmentdefinition.example.yaml`). Reasons
given in that file: bootd must see the exact broadcast domain the
booting machine's NIC is on, and its proxyDHCP replies must reach only
that segment, not every network the node's `eth0` touches.

Steps, per `Subnet`:

1. Copy `config/bootd/networkattachmentdefinition.example.yaml`, strip
   the `.example` suffix, fill in the segment's real CNI plugin
   (macvlan, ipvlan, or a bridge onto a VLAN-tagged host interface),
   and apply it in the `Subnet`'s own namespace under its final name.
2. Point `Subnet.spec.bootdNetworkRef` at that name. The controller
   stamps the `k8s.v1.cni.cncf.io/networks` annotation for you from
   this field.
3. Set `Subnet.spec.bootdServerIP` and `spec.cidr` to that segment's
   real address and subnet; the controller derives
   `BOOTD_SERVER_IP`/`BOOTD_PROVISIONING_CIDR` from these fields.

`bootdServerIP` requires static IPAM on the bootd NAD - a dynamic
allocator such as `whereabouts` cannot satisfy this, and
`internal/nadvalidate.CheckBootdAddress` raises a Violation on any
non-static IPAM it recognizes on this NAD.

Target machines attach only to this provisioning bridge - they must
not share it with unrelated cluster or data-plane traffic.

### 2.3 Per-Subnet namespace prerequisites: ServiceAccount and Pod Security Admission

Every namespace that holds a boot-half `Subnet` needs two things the
controller does not create for you, alongside the NAD from 2.2:

- **The `pod-security.kubernetes.io/enforce=privileged` label.**
  dnsmasq refuses to serve DHCP without `NET_ADMIN` and `NET_RAW`.
  Those capabilities are outside both the `restricted` and `baseline`
  Pod Security Admission profiles, so a namespace missing this label
  admission-rejects the bootd pod outright.
- **A `bootd` ServiceAccount, bound to `config/bootd/rbac.yaml`'s
  ClusterRole.** That file provisions this ServiceAccount in
  `kezio-system` (kustomized from the base name `bootd`) only, and
  every bootd Deployment stamps `serviceAccountName: kezio-bootd`
  unconditionally without creating it
  (`internal/controller/subnet_bootd_config.go`'s
  `bootdDefaultServiceAccountName`). A `Subnet` namespace other than
  `kezio-system` needs its own `kezio-bootd` ServiceAccount, bound to
  the same ClusterRole (a subject on `config/bootd/rbac.yaml`'s
  ClusterRoleBinding, or its own RoleBinding), before its bootd pod can
  start.

### 2.4 Reverse-proxying the agent and boot APIs (recommended)

Without further setup, a booting machine needs to reach the boot
config server (`internal/bootserver`, ClusterIP-only Service by
default) and the agent registration server (`internal/agentserver`,
also ClusterIP-only by default) directly on the provisioning segment.
That is usually not possible from outside the cluster network.

The prescribed fix is bootd's own reverse proxy: set
`BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` and
`BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL` on the controller-manager
Deployment to those two Services' in-cluster cluster-DNS URLs (for
example
`http://boot-server.kezio-system.svc.cluster.local:8090` and
`http://agent-server.kezio-system.svc.cluster.local:8091`). Every
`Subnet`'s bootd then proxies every `/boot/...` and `/agent/...`
request it receives to the matching Service. Point `BOOT_SERVER_URL`
and `BOOT_AGENT_SERVER_URL` (also on the controller-manager Deployment)
at each `Subnet`'s own `bootdServerIP` instead of a separately exposed
Service - a single manager-wide value, so a lab with more than one
`Subnet` must pick one `Subnet`'s bootd as the address every other
segment routes to for boot config and agent registration (this is the
shape the routed multi-subnet case in section 2.7 uses). With this in
place, a machine on any provisioning segment needs exactly one
reachable address for the whole boot-to-registration flow: bootd's own.

A site that does not use bootd's proxy must expose the boot config and
agent servers itself (a NodePort, a LoadBalancer, or `hostNetwork`) and
point `BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL` at that address
instead. bootd's proxy is the default path, not the only one.

### 2.5 BMC network reachability

`spec.bmc` is required on every `Machine` (`api/v1alpha2/machine_types.go`'s
`MachineBMC` field; the validating webhook rejects a `Machine` with an
empty `spec.bmc.address`). The controller-manager needs to reach that
BMC endpoint over the cluster's own network path - `internal/bmc` picks
a `redfish://` or `ipmi://` driver from the address's URL scheme
(`docs/bmc.md`). Whatever L3 path connects the cluster to the site (see
section 2.6) must also carry BMC traffic to every machine's board
management controller. A machine with no reachable BMC cannot be
enrolled at all: kezio has no mode that inspects or deploys a machine
without one.

### 2.6 Cluster to site L3 baseline

kezio assumes the cluster already has routed L3 connectivity to every
site's provisioning segment, BMC network, and data network. It does
not ship or require any particular VPN or overlay technology. If sites
are not already connected at L3, set up a routed VPN mesh (for example
WireGuard) between each site's gateway before deploying kezio there -
this is the operator's own network design, and kezio only assumes it
already exists.

### 2.7 The routed multi-subnet case: machines and seeder on different segments, one Site

A `Site` is not required to place its machines and its seeder/tracker
on the same broadcast domain. The model in `docs/network-model.md`
makes this explicit: a `Subnet` with `seederNetworkRef` set and no boot
half at all is a valid, data-plane-only `Subnet`, and any number of
boot-half `Subnet`s can name the same `Site` as that data-plane `Subnet`
so long as the cluster's own routed L3 (section 2.6) actually connects
them.

A minimal routed shape looks like this: two boot-half `Subnet`s (each
with its own `bootdServerIP`, `bootdNetworkRef`, and `dhcp`), plus one
data-plane-only `Subnet` for the Site's seeder and tracker, all naming
the same `Site`:

```yaml
apiVersion: kezio.kojuro.date/v1alpha2
kind: Site
metadata:
  name: lab-site
  namespace: kezio-system
spec:
  seederSubnetRef:
    name: lab-subnet-data
  tracker:
    ip: 198.51.102.3
---
apiVersion: kezio.kojuro.date/v1alpha2
kind: Subnet
metadata:
  name: lab-subnet-a
  namespace: kezio-system
spec:
  siteRef: { name: lab-site }
  cidr: 198.51.100.0/24
  bootdServerIP: 198.51.100.2
  bootdNetworkRef: { name: kezio-boot-network-a }
  dhcp: { mode: proxy }
---
apiVersion: kezio.kojuro.date/v1alpha2
kind: Subnet
metadata:
  name: lab-subnet-b
  namespace: kezio-system
spec:
  siteRef: { name: lab-site }
  cidr: 198.51.101.0/24
  bootdServerIP: 198.51.101.2
  bootdNetworkRef: { name: kezio-boot-network-b }
  dhcp: { mode: proxy }
---
apiVersion: kezio.kojuro.date/v1alpha2
kind: Subnet
metadata:
  name: lab-subnet-data
  namespace: kezio-system
spec:
  siteRef: { name: lab-site }
  cidr: 198.51.102.0/24
  seederNetworkRef: { name: kezio-seeder-network }
```

Machines on `lab-subnet-a` and `lab-subnet-b` both resolve to
`lab-site`, and both leech from the one seeder Deployment placed on
`lab-subnet-data`. Two things the operator must still provide, beyond
the objects above:

- **Routes, not a default-route flip**, on the seeder/tracker pod's own
  network attachment: specific routes toward each boot `Subnet`'s CIDR
  through the data segment's own gateway, so the single-homed
  seeder/tracker pod (it carries no cluster network at all - see
  `docs/network-model.md`'s no-NAT rule) can actually reach a leecher on
  either boot segment. `eth0`, where it still exists on a machine, stays
  untouched.
- One `BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL` pair on the
  controller-manager, pointed at whichever boot `Subnet`'s bootd fronts
  the reverse proxy (section 2.4) - a routed segment's machines reach it
  over the same L3 hop their own bootd already assumes exists.

This exact shape - two boot segments plus one data-plane segment, one
Site - is what kezio's own `e2e-routed-site` GitHub Actions lane builds
and asserts end to end; see section 8.

## 3. Tracker and seeder connectivity

BitTorrent is not NAT-friendly: a peer connects directly to the
`IP:port` its tracker announce advertised. kezio's hard requirement is:
**announce `IP:port` == reachable `IP:port`, no NAT/SNAT/DNAT anywhere
on that path** (`docs/network-model.md`'s "no-NAT rule"). Two concrete
requirements follow from this:

- Every target machine's leecher must be able to reach the Site's
  tracker announce URL (`Site.status.trackerURL`) and every seeder's
  BitTorrent listen port (fixed at `16881`,
  `internal/seederdeploy/identity.go`'s `EzioBTPort`).
- The address the tracker hands back to peers must be the seeder's
  real, reachable address. Nothing on the path may rewrite it.

### The tracker is Site-owned, not a manifest you deploy

`SiteReconciler` creates and keeps current one tracker Deployment per
Site using `Site.spec.tracker.ip`, single-homed on that Site's seeding
`Subnet` at the pinned address
(`internal/controller/site_tracker_deployment.go`). There is no
`kustomize build .../opentracker | kubectl apply` step for a Site's own
tracker: setting `Site.spec.tracker.ip` and `TRACKER_DEPLOYMENT_IMAGE`
(on the controller-manager) is enough. `config/opentracker` still
exists, but only as a standalone manifest for an operator who wants a
tracker running independently of any Site and points a Site's
`tracker.externalURL` at it instead (`config/opentracker/kustomization.yaml`'s
own header comment) - it is not the deployment path for a Site's own
tracker.

No Service fronts either the Site-owned tracker or a seeder pod: each
is reached only through its own `pod.Status.PodIP` on the data network -
a ClusterIP would DNAT the connection and break the no-NAT rule.

### Attaching seeder and tracker pods: same subnet/bridge as the targets, via Multus

Attach the seeding `Subnet`'s network the same way `config/bootd`
attaches its own provisioning interface: a Multus secondary interface,
never `hostNetwork`. A seeder pod's whole default network attachment is
derived per Site, not set through one cluster-wide variable:
`Site.spec.seederSubnetRef` names the `Subnet` seeder pods attach to,
and that `Subnet`'s own `spec.seederNetworkRef` names the NAD
(`seederPodAnnotations`, `internal/controller/image_seeder.go`;
`trackerPodAnnotations`, `internal/controller/site_tracker_deployment.go`).
`eth0` stays for cluster-internal traffic only; the data network
carries BitTorrent peer connections and tracker announce/response
traffic directly, with no Service and no NAT in the path at all. A Site
whose seeder `Subnet` carries no `seederNetworkRef` still gets its
seeder Deployment, just with pods on the ordinary cluster network only.
A Site with no `seederSubnetRef` at all gets no seeder Deployment
created for it, and no tracker either (see section 2 above).

This is the shape kezio's own end-to-end lanes use today
(`.github/workflows/main.yaml`'s `e2e-routed-site` and
`e2e-two-site-concurrent` jobs create `NetworkAttachmentDefinition`s for
the boot network(s), the tracker, and the seeder, as static-IPAM Multus
attachments).

**This shape needs one address per concurrently active `(Image, Site)`
seeder Deployment, not one address per Site**, plus one more for the
Site's own tracker. See `docs/network-model.md`'s "address-pool sizing
rule" section for the full sizing rule and why a real site should move
to a `whereabouts` pool (with its `ip-reconciler` CronJob deployed
alongside it) once more than one Image can be active at a time.
`internal/nadvalidate.CheckSeederStaticMultiImage` raises an Advisory
condition on the Subnet once a Site crosses that line.

### Routing, not a default-route flip

Whichever segment the seeder/tracker pod lands on, do not make the
data-network interface the pod's default route. Give it specific
routes scoped to the data network only, through the
`NetworkAttachmentDefinition`'s own routing configuration; leave `eth0`
carrying the cluster's own pod/service CIDRs untouched.

### The tracker is not replicated per site-with-no-seeder

A Site with `seederSubnetRef` set runs exactly one tracker of its own.
A Site with no `seederSubnetRef` runs none - it has nothing to
announce for. Two Sites that route to each other may still point their
`tracker.externalURL` at one operator-run tracker if the operator
chooses to share one; kezio never assumes this, since two Sites are
never guaranteed to route to each other at all
(`docs/network-model.md`'s "why a single global tracker cannot exist"
section).

## 4. Secure Boot

The shim and GRUB binaries kezio serves over TFTP are Debian-signed
release artifacts. A production machine can keep UEFI Secure Boot
turned on through the whole boot chain. See `docs/secure-boot.md` for
the full trust chain, the kernel-signing choice an operator must make,
and the explicit statement that no kezio CI lane exercises Secure Boot
end to end - this guide's own network scenarios are unaffected by
whether Secure Boot is on or off at a site.

## 5. Image boot-entry contract

kezio-agent creates a firmware NVRAM boot entry for a deployed machine
(`efibootmgr --create`) once a deploy finishes, labelled
`kezio:<machine name>`. It never opens or edits a deployed image's file
system to do this - it only points the new entry at a fixed loader path
on the machine's own EFI System Partition (ESP).

**Contract: every bootable Image must already carry its own fallback
bootloader at the fixed path firmware falls back to on its own.** This
matches the shape of Clonezilla's `update-efi-nvram-boot-entry` and
Ironic IPA's `efi_utils`: both write an NVRAM entry only, and both leave
the fallback file to the image. kezio does the same.

The fixed path is per architecture. kezio supports exactly two
architectures today:

| Architecture | Fallback bootloader path on the ESP |
|---|---|
| x86_64 | `\EFI\BOOT\BOOTX64.EFI` |
| aarch64 | `\EFI\BOOT\BOOTAA64.EFI` (declared, not implemented yet) |

arm32 and RISC-V are out of scope; kezio-agent fails an aarch64 deploy
explicitly rather than writing an x86_64 path onto it.

A machine's NVRAM can lose a boot entry on its own - a factory reset, a
dead CMOS battery, a hypervisor's EFI variable store not surviving a
reboot. When that happens, firmware falls back to the fixed path above
with no NVRAM entry involved at all. The contract exists so that
fallback still finds a working bootloader.

kezio adds no check for this contract anywhere: no ingest-time check on
an uploaded Image, no Image status warning, no deploy-time gate. The
operator who builds or picks a golden image carries the responsibility
to make sure it already ships the fallback bootloader for its
architecture; this guide and the contract table above are the whole of
the documentation for it.

A golden image that does not already carry the fallback file can use
the `install-removable-fallback` builtin PostHook step instead of
carrying one by hand. That step copies a shim or GRUB binary it finds
under another `EFI/<name>/` directory on the ESP into the fallback path.
It is opt-in - a `PostHook` must name it explicitly - and it is scoped
to the same two architectures as the table above.

## 6. Full port table

Every port below is checked against the manifests and code that
declare it.

| Port | Protocol | Service | Verified against |
|---|---|---|---|
| 67 | UDP | bootd proxyDHCP | `internal/bootd/render.go` (`dhcp-range=...,proxy`) |
| 4011 | UDP | bootd PXE boot-server (`pxe-service`) | `internal/bootd/render.go` |
| 69 | UDP | bootd TFTP (shim/GRUB artifacts) | `internal/bootd` package doc comment |
| 8090 | TCP | Boot config server (`internal/bootserver`, GRUB config + live artifacts) | `internal/bootserver/server.go` |
| 8091 | TCP | Agent registration server (`internal/agentserver`) | `internal/agentserver` |
| 6969 | TCP+UDP | tracker announce | `internal/controller/site_tracker_deployment.go` (`trackerAnnouncePort`) |
| 16881 | TCP | ezio-seeder BitTorrent listen port | `internal/seederdeploy/identity.go` (`EzioBTPort`) |
| 8080 | TCP | ezio-seeder `.torrent`-by-info-hash HTTP server | `internal/seederdeploy/identity.go` (`TorrentHTTPPort`) |

Two more ports exist in the system and are worth knowing about when
building firewall rules:

| Port | Protocol | Service | Verified against |
|---|---|---|---|
| 80 | TCP | bootd's own reverse proxy front, listened on only once `BOOTD_AGENT_UPSTREAM_URL` or `BOOTD_BOOT_UPSTREAM_URL` is set on the Subnet's bootd Deployment | `cmd/bootd/main.go` |
| 50051 | TCP | ezio-seeder gRPC control port (pod-local only; the `seeder-register` container in the same pod dials it at `127.0.0.1:50051`, not a booting machine) | `internal/seederdeploy/identity.go` (`EzioGRPCPort`) |

BMC ports are not listed above because they depend on the driver
chosen per machine (`docs/bmc.md`): `redfish://` uses whatever HTTPS
port the BMC's own address names, and `ipmi://` defaults to IPMI's
standard port 623 (`internal/bmc/ipmi/ipmi.go`'s `defaultPort`). Confirm
each BMC's actual listening port against its own documentation.

## 7. Bring-up order

1. Deploy `config/default` (CRDs and controller-manager).
2. Set `BOOT_SERVER_ADDR`, `AGENT_SERVER_ADDR`, `BOOT_SERVER_URL`,
   `BOOT_AGENT_SERVER_URL`, and `DEPLOYER=agent` on the
   controller-manager Deployment.
3. Set `TRACKER_DEPLOYMENT_IMAGE` on the controller-manager to enable
   per-Site tracker Deployment reconciliation, and
   `PARTITIONCONTENT_SEEDER_IMAGE` to enable per-`(Image, Site)` seeder
   Deployments.
4. Set `BOOTD_DEPLOYMENT_IMAGE` and
   `BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE` on the controller-manager
   Deployment to enable bootd Deployment reconciliation.
5. Create a `Site` object for each maximal routable domain (section 2
   above defines what makes two segments the same Site).
6. For each provisioning L2 segment: create its NAD (section 2.2), its
   `kezio-bootd` ServiceAccount and PSA label (section 2.3), and a
   `Subnet` object referencing its Site - choosing one of the two DHCP
   scenarios in section 1 through `spec.dhcp.mode`.
7. Choose and wire the tracker/seeder connectivity from section 3, per
   Site if needed. To give a Site its own local seeder and tracker, set
   `Site.spec.seederSubnetRef` to one of its Subnets (a data-plane-only
   `Subnet` is fine - section 2.7), that Subnet's own
   `spec.seederNetworkRef` to the data-network NAD, and
   `Site.spec.tracker.ip` to the tracker's pinned address on that same
   NAD.
8. Enroll each `Machine`: set `spec.bootMACAddress`, `spec.bmc.address`
   and `spec.bmc.credentialsSecretRef` (required - see section 2.5),
   and `spec.subnetRef`, naming the `Subnet` whose segment the
   machine's boot NIC is physically wired to. The machine's Site is
   derived from this reference, never set directly - see section 2's
   introduction.
9. Let kezio power on the machine through its BMC and confirm it PXE
   boots, registers with the agent server, and reaches Available.

## 8. e2e lanes measured against this matrix

kezio's KubeVirt-based GitHub Actions lanes in `main.yaml` already
label their CI-only shortcuts in their own comments. This section
restates the ones that matter for the scenarios above, for an operator
comparing a lab run against a CI run.

- **`e2e-fast-lane` and the image-path/deploy lanes exercise scenario 1
  (existing DHCP)**, one Site with one boot `Subnet`.
- **`e2e-routed-site` exercises the routed multi-subnet case in section
  2.7 end to end**: two boot-half `Subnet`s (segments A and B) plus one
  data-plane-only `Subnet` (segment C, hosting the Site's tracker and
  seeder), all naming one `Site`, with two target VMs deploying the
  same Image concurrently off the one shared seeder Deployment. It
  asserts the seeder/tracker's L3 reachability from both boot segments,
  that exactly one seeder Deployment exists for the shared `(Image,
  Site)` pair, and that `PartitionContent.status.seeders[]` reports
  `machineCount: 2` for that Site while both deploys are in flight.
- **`e2e-two-site-concurrent` exercises two Sites that deliberately
  cannot route to each other**, each with its own single-segment boot
  and data plane, deploying the same Image concurrently. It asserts
  network-level isolation (each Site's machine can reach only its own
  Site's tracker and seeder, both before and during the deploy), that
  exactly two seeder Deployments exist (one per Site), and that
  injecting a fault into one Site's seeding `Subnet` degrades only that
  Site's status, leaving the other Site's status unaffected.
- **Tracker/seeder connectivity always uses the Multus, same-bridge,
  static-IPAM shape** described in section 3 - none of these lanes
  exercises a routed-L3-to-a-cluster-Service shape, since kezio ships
  no LoadBalancer/NodePort/hostPort variant of a tracker or seeder
  Service to exercise.
- **The BMC lanes' Redfish endpoints are plain HTTP** (KubeVirtBMC's
  generated Service does not terminate TLS), reached with the
  `redfish+http://` scheme instead of `redfish://`. A production BMC's
  Redfish endpoint should be reached with `redfish://` (HTTPS) instead
  - see `docs/bmc.md`.
- **Secure Boot is off in every VM these lanes boot** - see
  `docs/secure-boot.md`; none of the network scenarios above interact
  with Secure Boot, so this gap does not change any guidance in this
  document.

See `docs/e2e-scale-multisite-kubevirt.md` for the full detail on what
`e2e-routed-site` and `e2e-two-site-concurrent` each prove and what they
deliberately do not claim.

### Coverage note: which scenarios are proven by the packet lab, and which by the KubeVirt lanes

`hack/bootd-packet-lab.sh` gives scenario 1 (existing DHCP) and
scenario 2 (lease mode) a repeatable, real-packet assertion: a fresh
netns/veth topology, the real dnsmasq supervisor, and a PXE-shaped
client sending an actual DHCPDISCOVER, asserting the DHCPOFFER (or its
absence for a denied MAC) each scenario should produce.

What it does **not** claim: this is a DHCP/PXE packet-level assertion,
not a full boot-to-registration KubeVirt run - it stops once the
DHCPOFFER is verified, before TFTP, GRUB, the boot config server, or
agent registration. The KubeVirt lanes close that gap for scenario 1.
Scenario 2 (lease mode) has no KubeVirt lane today; extending an
existing lane (or adding a new one) with a lease-mode variant remains
open work.

## 9. Fact-check table

| Claim | File verified against |
|---|---|
| bootd never assigns IP leases in proxy mode; every `dhcp-range` carries `proxy` | `internal/bootd/render.go` |
| `dhcp.mode: lease` renders a lease-serving `dhcp-range` and `dhcp-boot`/`dhcp-match` instead of `pxe-service`; the MAC gate is unchanged | `api/v1alpha2/subnet_types.go` (`SubnetDHCP`), `internal/bootd/render.go` |
| One bootd replica per boot-half Subnet | `internal/controller/subnet_controller.go` |
| bootd needs a Multus attachment, not `hostNetwork` | `config/bootd/networkattachmentdefinition.example.yaml` |
| Namespace needs `pod-security.kubernetes.io/enforce=privileged` | `config/bootd/networkattachmentdefinition.example.yaml`, `config/bootd/rbac.yaml` |
| Boot config server / agent server default Services are ClusterIP | `internal/bootserver`, `internal/agentserver` |
| No-NAT rule for tracker/seeder | `docs/network-model.md` |
| Tracker Service (`config/opentracker`) is ClusterIP-only and is the externalURL-only escape hatch, not the Site-owned tracker's deployment path; no Service at all exists for a seeder pod or a Site-owned tracker | `config/opentracker/kustomization.yaml`, `config/opentracker/opentracker-service.yaml`, `internal/controller/site_tracker_deployment.go`, `internal/controller/image_seeder.go` |
| e2e lanes use Multus same-bridge attachment for tracker/seeder | `.github/workflows/main.yaml` (`e2e-routed-site`, `e2e-two-site-concurrent`) |
| Fixed BT port 16881; torrent HTTP port 8080; gRPC port 50051 | `internal/seederdeploy/identity.go` |
| Tracker port 6969 | `internal/controller/site_tracker_deployment.go` (`trackerAnnouncePort`) |
| A Site runs at most one tracker of its own, only when it has a seederSubnetRef | `api/v1alpha2/site_types.go`, `internal/controller/site_controller.go` |
| A Machine's Site is derived (`spec.subnetRef` -> `Subnet.spec.siteRef` -> `Site`), never set directly | `internal/sitederive` |
| BMC driver selection by URL scheme; IPMI default port 623 | `docs/bmc.md`, `internal/bmc/ipmi/ipmi.go` |
| Secure Boot chain and CI gap | `docs/secure-boot.md` |
| Scenario 1 and scenario 2 are both covered by the local packet lab's real-packet assertions; only scenario 1 also has a KubeVirt e2e lane | `internal/bootd/lab_test.go`, `hack/bootd-packet-lab.sh` |
| `redfish+http://` exists and is documented as a lab/test-only scheme | `internal/bmc/redfish/redfish.go`, `docs/bmc.md` |
| `e2e-routed-site` builds two boot Subnets plus one data-plane-only Subnet, one Site | `.github/workflows/main.yaml` (`e2e-routed-site` job) |
| `e2e-two-site-concurrent` builds two Sites that cannot route to each other and asserts isolation | `.github/workflows/main.yaml` (`e2e-two-site-concurrent` job) |
| whereabouts needs its `ip-reconciler` CronJob deployed alongside it | `docs/lab-proxmox-rke2.md` |
