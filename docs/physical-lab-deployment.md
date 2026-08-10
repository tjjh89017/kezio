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

Read this guide together with:

- `config/bootd/README.md` - the full bootd option list.
- `config/bootserver/README.md` and `config/agentserver/README.md` -
  the boot config and agent registration servers bootd fronts.
- `config/seeder/README.md` - the BitTorrent data plane and its no-NAT
  rule.
- `docs/bmc.md` - the BMC drivers.
- `docs/secure-boot.md` - the UEFI Secure Boot chain.

Every claim in this guide is checked against the code and manifests
that implement it. Where a behavior is planned but not built yet, this
guide says so.

## 1. The two network scenarios

kezio-bootd never assigns an IP address itself. It answers only the
PXE part of a boot exchange: which file to fetch over TFTP, and from
which server. A separate DHCP source must always hand out the IP
lease. The two scenarios below cover the two ways an operator can
supply that DHCP source. Pick the scenario that matches each
provisioning segment before you deploy `config/bootd` for it.

### Scenario 1: existing DHCP on the same L2 (default, lab-proven)

Use this scenario when the site already runs a DHCP server on the same
broadcast domain as the provisioning segment.

bootd's proxyDHCP answers PXE requests only. It never touches lease
traffic. The site's own DHCP server keeps sole ownership of leases, and
both services run on the segment at the same time with no conflict
(`config/bootd/README.md`, "No IP leases: bootd coexists with
production DHCP").

Setup: do not set `BOOTD_LEASE_MODE`. Leave it unset. This is the
default.

This shape is exercised by the in-repo packet lab
(`internal/bootd/lab_test.go`'s `TestDnsmasqLab`, `BOOTD_LAB=1`), which
runs the real dnsmasq supervisor against a real netns topology -
driven end to end by `hack/bootd-packet-lab.sh`, which also sends a
real PXE-shaped DHCPDISCOVER from the far end of the veth pair
(`internal/bootd/lab_client_test.go`'s `TestDnsmasqLabClient`) and
asserts an enrolled MAC receives a proxyDHCP DHCPOFFER while a denied
one receives nothing at all. It is also exercised end to end by
kezio's KubeVirt-based GitHub Actions lane - see section 8 for what
that lane covers.

### Scenario 2: isolated segment (own DHCP server mode)

Use this scenario when the provisioning segment has no DHCP server of
its own - bootd itself must hand out IP leases.

Set `BOOTD_LEASE_MODE=true`. bootd's dnsmasq then renders a
lease-serving `dhcp-range` (start/end host addresses, no `proxy` flag)
instead of the proxyDHCP range, and becomes the segment's own DHCP
authority (`internal/bootd/config.go`'s `LeaseMode` field;
`internal/bootd/render.go`). The lease range defaults to the
provisioning subnet's first and last host addresses; set
`BOOTD_LEASE_RANGE_START` and `BOOTD_LEASE_RANGE_END` together to
override it.

The MAC gate does **not** relax in this mode: only enrolled MACs
receive a lease, the same `dhcp-hostsfile`/`dhcp-ignore` pair scenario
1 uses. A device that is not an enrolled Machine gets nothing at all,
even in an otherwise DHCP-server-less segment - it is out of scope for
bootd to serve it, and this mode does not turn bootd into the
segment's general-purpose DHCP server. An operator who also needs to
hand out addresses to unenrolled devices must put those devices on a
different segment; kezio does not support mixing enrolled and
unenrolled DHCP traffic on the same lease-mode segment.

PXE delivery differs from proxy mode's `pxe-service`, which does not
work once dnsmasq is not a proxy (it breaks UEFI secure netboot):
lease mode hands out the boot file via `dhcp-boot`, matched to the
client's architecture through `dhcp-match` on DHCP option 93 (the same
signal a future non-x86-64 architecture would extend with one more
match line).

Setup:

- Set `BOOTD_LEASE_MODE=true`.
- Leave `BOOTD_LEASE_RANGE_START`/`BOOTD_LEASE_RANGE_END` unset to
  auto-derive the range, or set both to an explicit sub-range.

This mode is exercised by the same in-repo packet lab as scenario 1
(`internal/bootd/lab_test.go`'s `TestDnsmasqLab`, with
`BOOTD_LAB_LEASE_MODE=1`, also driven by `hack/bootd-packet-lab.sh`):
an enrolled MAC's DHCPDISCOVER gets back a DHCPOFFER carrying a real,
non-zero leased address and the boot filename; a denied MAC gets
nothing, the same MAC-gate outcome as scenario 1. It is not covered by
a KubeVirt e2e lane - see section 8 for what those lanes cover instead.

## 2. Network prerequisites the operator owns

kezio models the network as two Kubernetes objects, `Site` and
`Subnet` (`api/v1alpha1/site_types.go`, `api/v1alpha1/subnet_types.go`).
Read their meaning before working through the rest of this section -
every prerequisite below is a consequence of it.

**A Site is a maximal routable domain.** Every Subnet inside one Site
is mutually routable. Anything separated by a VRF, firewall, or other
barrier that breaks reachability is, by definition, a different Site.
A Subnet is one broadcast domain - the provisioning L2 segment a group
of target machines' boot NICs share - and names its Site through
`Subnet.spec.siteRef`.

This split follows directly from what each service needs:

- **bootd binds to a broadcast domain.** proxyDHCP and TFTP only work
  on the segment they answer on, so `SubnetReconciler`
  (`internal/controller/subnet_controller.go`) creates one bootd
  Deployment per Subnet, in that Subnet's own namespace.
- **The seeder binds to locality and bandwidth**, not a broadcast
  domain - dragging image content across a WAN link defeats the point
  of a local seeder. kezio instead runs one seeder Deployment per
  (Image, Site) (`config/seeder/README.md`, "Per-Image, on-demand
  seeding"); every Subnet inside that Site routes to it.

A machine's Site is never hand-entered; it is derived:
`Machine.spec.subnetRef -> Subnet.spec.siteRef -> Site`
(`internal/sitederive`). This closes a class of silent
misconfiguration that a hand-entered site field would otherwise allow:
a machine could physically boot from one segment's bootd while
declaring a different site, and leech from a distant seeder instead of
the one right beside it on its own segment. Under this model that
mismatch cannot happen - a machine's Site always follows from which
Subnet its boot NIC is actually wired to.

**Reassigning a Subnet to a different Site re-resolves the seeder for
every machine on that segment.** Treat this as accepted behavior, not
something to guard against: a Subnet's Site is what decides which
seeder its machines reach, so changing `Subnet.spec.siteRef` changes
that resolution immediately, for every Machine referencing the Subnet.

**bootd's address is pinned; the seeder's need not be.**
`Subnet.spec.bootdServerIP` is explicit and never IPAM-allocated: it
doubles as the PXE next-server and TFTP target address firmware caches
mid-boot, so a reallocation on pod restart would strand an in-flight
GRUB/TFTP fetch. A seeder pod's address carries no such constraint -
BitTorrent's tracker-based peer discovery tolerates it changing between
seeder Deployments - so the seeder Subnet's `spec.seederNetworkRef` is
free to use an IPAM plugin that reallocates.

A Site with no seeder Subnet of its own (`Site.spec.seederSubnetRef`
unset) is fine for a Site whose Images carry no torrent content - it
simply runs no local seeder. A Site whose Images do carry torrent
content needs its own seeder Subnet: leeching across a routed link from
another Site's seeder is not currently supported by the deploy-plan
path, so a Machine at such a Site would otherwise wait forever
(`internal/controller/seeder_deployment.go`'s `ImageConditionSeederDegraded`
surfaces this with Reason `SeederSubnetRefUnset`).

### 2.1 Provisioning L2 per Subnet

Each broadcast domain a group of target machines' boot NICs share (a
VLAN or a dedicated switch/bridge) needs its own `Subnet` object.
`SubnetReconciler` creates and keeps current exactly one bootd
Deployment per Subnet, in that Subnet's own namespace, pinned to
`replicas: 1` - two bootd pods answering the same broadcast domain
would both reply to every DHCPDISCOVER, and firmware cannot prefer one
answer over the other (`config/bootd/README.md`, "One replica per boot
segment").

A production deployment with more than one site or VLAN creates one
Subnet per segment, each carrying its own `bootdServerIP` / `cidr` /
`bootdNetworkRef`. Every Subnet's bootd Deployment proxies to the same
cluster-wide agent and boot config servers, once
`BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL` and
`BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` are set on the controller-manager
(`config/bootd/README.md`, "Per-site addressing").

### 2.2 Multus NAD wiring for bootd

bootd's pod needs a second network interface on the provisioning
segment, attached with Multus, not `hostNetwork`
(`config/bootd/networkattachmentdefinition.example.yaml`). Reasons
given in that file: bootd must see the exact broadcast domain the
booting machine's NIC is on, and its proxyDHCP replies must reach only
that segment, not every network the node's `eth0` touches.

Steps, per Subnet:

1. Copy `config/bootd/networkattachmentdefinition.example.yaml`, strip
   the `.example` suffix, fill in the segment's real CNI plugin
   (macvlan, ipvlan, or a bridge onto a VLAN-tagged host interface),
   and apply it in the Subnet's own namespace under its final name.
2. Point `Subnet.spec.bootdNetworkRef` at that name. The controller
   stamps the `k8s.v1.cni.cncf.io/networks` annotation for you from
   this field - there is no `deployment.yaml` to hand-edit or
   uncomment anything on.
3. Set `Subnet.spec.bootdServerIP` and `spec.cidr` to that segment's
   real address and subnet; the controller derives
   `BOOTD_SERVER_IP`/`BOOTD_PROVISIONING_CIDR` from these fields.
   `BOOTD_DHCP_INTERFACE` needs no per-Subnet setting - it is a
   controller default (`net1`).

Target machines attach only to this provisioning bridge - they must
not share it with unrelated cluster or data-plane traffic.

### 2.3 Per-Subnet namespace prerequisites: ServiceAccount and Pod Security Admission

Every namespace that holds a Subnet needs two things the controller
does not create for you, alongside the NAD from 2.2:

- **The `pod-security.kubernetes.io/enforce=privileged` label.**
  dnsmasq refuses to serve DHCP without `NET_ADMIN` and `NET_RAW`.
  Those capabilities are outside both the `restricted` and `baseline`
  Pod Security Admission profiles, so a namespace missing this label
  admission-rejects the bootd pod outright (`config/bootd/README.md`,
  "Capabilities and Pod Security Admission"). This relaxes
  admission-time enforcement only; the bootd pod itself still grants
  nothing beyond those three capabilities plus root, with every other
  capability dropped.
- **A `bootd` ServiceAccount, bound to `rbac.yaml`'s ClusterRole.**
  `config/bootd/rbac.yaml` provisions this ServiceAccount in
  `kezio-system` only, and every bootd Deployment stamps
  `serviceAccountName: bootd` unconditionally without creating it. A
  Subnet namespace other than `kezio-system` needs its own `bootd`
  ServiceAccount, bound to the same ClusterRole (a subject on
  `rbac.yaml`'s ClusterRoleBinding, or its own RoleBinding), before its
  bootd pod can start (`config/bootd/README.md`, "RBAC scope").

### 2.4 Reverse-proxying the agent and boot APIs (recommended)

Without further setup, a booting machine needs to reach the boot
config server (`internal/bootserver`, ClusterIP-only Service by
default) and the agent registration server (`internal/agentserver`,
also ClusterIP-only by default) directly on the provisioning segment.
That is usually not possible from outside the cluster network.

The prescribed fix is bootd's own reverse proxy: set
`BOOTD_BOOT_UPSTREAM_URL` and `BOOTD_AGENT_UPSTREAM_URL` on bootd's
Deployment to those two Services' in-cluster cluster-DNS URLs (for
example
`http://kezio-boot-server.kezio-system.svc.cluster.local:8090` and
`http://kezio-agent-server.kezio-system.svc.cluster.local:8091`).
bootd then proxies every `/boot/...` and `/agent/...` request it
receives to the matching Service, listening on `BOOTD_PROXY_ADDR`
(default: `BOOTD_SERVER_IP` with port 80). Point `BOOT_SERVER_URL` and
`BOOT_AGENT_SERVER_URL` (on the controller-manager Deployment) at that
same bootd address instead of a separately exposed Service
(`config/bootserver/README.md`, `config/agentserver/README.md`). With
this in place, a machine on the provisioning segment needs exactly one
reachable address for the whole boot-to-registration flow: bootd's own.

A site that does not use bootd's proxy must expose the boot config and
agent servers itself (a NodePort, a LoadBalancer, or `hostNetwork`) and
point `BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL` at that address
instead. bootd's proxy is the default path, not the only one.

### 2.5 BMC network reachability

`spec.bmc` is required on every `Machine` (`api/v1alpha1/machine_types.go`'s
`BMC` field; the validating webhook rejects a `Machine` with an empty
`spec.bmc.address`). The controller-manager needs to reach that BMC
endpoint over the cluster's own network path - `internal/bmc` picks a
`redfish://`, `ipmi://`, or `ipmitool://` driver from the address's URL
scheme (`docs/bmc.md`). Whatever L3 path connects the cluster to the
site (see section 2.6) must also carry BMC traffic to every machine's
board management controller. A machine with no reachable BMC cannot be
enrolled at all: kezio has no mode that inspects or deploys a machine
without one.

### 2.6 Cluster to site L3 baseline

kezio assumes the cluster already has routed L3 connectivity to every
site's provisioning segment, BMC network, and data network. It does
not ship or require any particular VPN or overlay technology
(`config/seeder/README.md`, "Cross-network baseline: routed L3, not
overlay/NAT"). If sites are not
already connected at L3, set up a routed VPN mesh (for example
WireGuard) between each site's gateway before deploying kezio there -
this is the operator's own network design, and kezio only assumes it
already exists.

## 3. Tracker and seeder connectivity

BitTorrent is not NAT-friendly: a peer connects directly to the
IP:port its tracker announce advertised. kezio's hard requirement is:
**announce IP:port == reachable IP:port, no NAT/SNAT/DNAT anywhere on
that path** (`config/seeder/README.md`, "The no-NAT rule"). Two
concrete requirements follow from this, whichever option below is
used:

- Every target machine's leecher must be able to reach the tracker's
  announce URL (`SEEDER_TRACKER_URL`) and every seeder's BitTorrent
  listen port (`EZIO_BT_PORT`, fixed at `16881` -
  `internal/controller/seeder_deployment.go`'s `seederBTPort`).
- The address the tracker hands back to peers must be the seeder's
  real, reachable address. Nothing on the path may rewrite it.

Neither option below is "the" answer for every site. Pick per the
trade-offs.

### Option 1: routed L3 to a stable cluster Service address

Give the tracker and each seeder a stable, routable address reachable
from every provisioning segment over ordinary L3 routing - a
LoadBalancer, NodePort, or hostPort in front of the tracker's Service,
plus a routable address on each seeder pod itself, with the operator
responsible for provisioning-to-service routing.

As shipped, `config/seeder/opentracker-service.yaml` is `type:
ClusterIP`, deliberately: its own comment states this is
cluster-internal reachability only, because a Service's ClusterIP
DNATs traffic, which breaks the no-NAT rule for the tracker's announce
port. **kezio does not ship a LoadBalancer/NodePort/hostPort variant of
this Service, and it creates no Service at all for a seeder pod.** Each
per-Image, per-site seeder Deployment
(`internal/controller/seeder_deployment.go`'s `buildSeederDeployment`)
is reached only through its own `pod.Status.PodIP` on the data network
- there is no Service to overlay for it. An operator choosing Option 1
must patch a further overlay that exposes the tracker's announce port
(`6969`) at a stable address reachable from every site, without any NAT
rewriting that address on the path - a plain ClusterIP does not satisfy
that; only a genuinely routable exposure (for example `hostNetwork`,
`hostPort`, or an external L3 LoadBalancer with no SNAT/DNAT between it
and the pod) does. Each seeder's BitTorrent port (`16881`) needs the
same kind of routable exposure directly on the pod's own IP, since no
Service exists to route around.

This option centralizes routing decisions at the cluster edge and
needs no per-site bridge attachment for the tracker/seeder pods
themselves, at the cost of building and maintaining that exposure
yourself.

### Option 2: same subnet/bridge as the targets, via Multus

Attach the tracker and every per-Image seeder pod to the same
data-network bridge the target machines' data NICs live on, as a
Multus secondary interface, the same way `config/bootd` attaches its
own provisioning interface
(`config/seeder/networkattachmentdefinition.example.yaml`,
`config/seeder/networkattachmentdefinition-whereabouts.example.yaml`).
A seeder pod's whole default network attachment is derived per Site,
not set through one cluster-wide variable: `Site.spec.seederSubnetRef`
names the Subnet seeder pods attach to, and that Subnet's own
`spec.seederNetworkRef` names the NAD (`seederPodAnnotations`,
`internal/controller/seeder_deployment.go`). `eth0` stays for
cluster-internal traffic only; the data network carries BitTorrent peer
connections and tracker announce/response traffic directly, with no
Service and no NAT in the path at all. A Site whose seeder Subnet
carries no `seederNetworkRef` still gets its seeder Deployment, just
with pods on the ordinary cluster network only. A Site with no
`seederSubnetRef` at all gets no seeder Deployment created for it (see
section 2's network prerequisites above).

This is the shape kezio's own end-to-end lanes use today
(`.github/workflows/main.yaml`'s `e2e-bmc` job creates
`NetworkAttachmentDefinition`s for the boot network, the tracker, and
the seeder, all as static-IPAM Multus attachments on the same
provisioning bridge).

**This option needs one address per concurrently active (Image, site)
seeder Deployment, not one address per site.** The operator creates one
seeder Deployment - one pod, one address - per Image currently
deploying at a site (`config/seeder/README.md`'s "Per-Image, on-demand
seeding"); a site where several Images can be deploying at once needs
that many addresses available at once. See
`config/seeder/README.md`'s "Provisioning-segment address pool" section
for the sizing rule and why a real site should use the `whereabouts`
example NAD, not the single-static-address one, once more than one
Image can be active at a time.

### Common ground: routing, not a default-route flip

Whichever option is used, do not make the data-network interface the
pod's default route. Give it specific routes scoped to the data
network only, through the NetworkAttachmentDefinition's own routing
configuration; leave `eth0` carrying the cluster's own pod/service
CIDRs untouched (`config/seeder/README.md`, "The no-NAT rule").

### The tracker is not replicated per site

There is exactly one tracker in a kezio deployment
(`config/seeder/README.md`'s introduction: "The tracker is the only
always-on part of the seeding data plane"; opentracker is deployed
once, by `config/seeder`). It must be reachable, by whichever option
above, from every site. Seeder Deployments, by contrast, are per
(Image, site): the operator creates and removes one on demand for each
site actively deploying that Image
(`config/seeder/README.md`'s "Per-Image, on-demand seeding") - see that
section for the full pattern before adding a second site.

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
| 67 | UDP | bootd proxyDHCP | `internal/bootd/render.go` (`dhcp-range=...,proxy`); `config/bootd/deployment.yaml` container port `proxydhcp` |
| 4011 | UDP | bootd PXE boot-server (`pxe-service`) | `internal/bootd/render.go`'s `pxe-service` line; `config/bootd/deployment.yaml` container port `pxe` |
| 69 | UDP | bootd TFTP (shim/GRUB artifacts) | `config/bootd/deployment.yaml` container port `tftp`; `internal/bootd` package doc comment |
| 8090 | TCP | Boot config server (`internal/bootserver`, GRUB config + live artifacts) | `config/bootserver/manager-port-patch.yaml`, `config/bootserver/service.yaml` |
| 8091 | TCP | Agent registration server (`internal/agentserver`) | `config/agentserver/manager-port-patch.yaml`, `config/agentserver/service.yaml` |
| 6969 | TCP+UDP | opentracker announce | `config/seeder/opentracker-deployment.yaml` (`args: -p 6969`); `config/seeder/opentracker-service.yaml` |
| 16881 | TCP | ezio-seeder BitTorrent listen port | `internal/controller/seeder_deployment.go` (`seederBTPort`, wired to the `EZIO_BT_PORT` env var) |

This matches the port list this guide was scoped against exactly - no
discrepancy was found between the expected list and the code.

Two more ports exist in the system but were out of the original list
and are worth knowing about when building firewall rules:

| Port | Protocol | Service | Verified against |
|---|---|---|---|
| 80 | TCP | bootd's own reverse proxy front (`BOOTD_PROXY_ADDR`, section 2.4) - only listened on once `BOOTD_AGENT_UPSTREAM_URL` or `BOOTD_BOOT_UPSTREAM_URL` is set | `config/bootd/deployment.yaml` container port `proxy-http`; `cmd/bootd/main.go`'s `bootdConfigFromEnv` |
| 50051 | TCP | ezio-seeder gRPC control port (pod-local only; the `seeder-register` container in the same pod dials it at `127.0.0.1:50051`, not a booting machine) | `internal/controller/seeder_deployment.go` (`EZIO_GRPC_LISTEN`, `EZIO_TARGET`) |

BMC ports are not listed above because they depend on the driver
chosen per machine (`docs/bmc.md`): `redfish://` uses whatever HTTPS
port the BMC's own address names, and `ipmi://`/`ipmitool://` default
to IPMI's standard port 623 (`internal/bmc/ipmi/ipmi.go`'s
`defaultPort`). Confirm each BMC's actual listening port against its
own documentation.

## 7. Bring-up order

1. Deploy `config/default` (CRDs and controller-manager).
2. Deploy `config/bootserver` and `config/agentserver`; set
   `BOOT_SERVER_ADDR`, `AGENT_SERVER_ADDR`, `BOOT_SERVER_URL`,
   `BOOT_AGENT_SERVER_URL`, and `DEPLOYER=agent` on the
   controller-manager Deployment (see their READMEs).
3. Deploy `config/seeder` (opentracker); set `SEEDER_DEPLOYMENT_IMAGE`
   and `SEEDER_TRACKER_URL` on the controller-manager Deployment to
   enable per-Image seeder Deployments (`config/seeder/README.md`,
   "Wiring the operator side").
4. Set `BOOTD_DEPLOYMENT_IMAGE` and
   `BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE` on the controller-manager
   Deployment to enable bootd Deployment reconciliation
   (`config/bootd/README.md`, "Who creates the bootd Deployment").
5. Create a `Site` object for each maximal routable domain (section 2
   above defines what makes two segments the same Site).
6. For each provisioning L2 segment: create its NAD (section 2.2), its
   `bootd` ServiceAccount and PSA label (section 2.3), and a `Subnet`
   object referencing its Site - choosing one of the two DHCP scenarios
   in section 1 through `spec.dhcp.mode`.
7. Choose and wire one tracker/seeder connectivity option from section
   3, per Site if needed. To give a Site its own local seeder, set
   `Site.spec.seederSubnetRef` to one of its Subnets, and that Subnet's
   own `spec.seederNetworkRef` to the data-network NAD.
8. Enroll each `Machine`: set `spec.bootMACAddress`, `spec.bmc.address`
   and `spec.bmc.credentialsSecretRef` (required - see section 2.5),
   and `spec.subnetRef`, naming the Subnet whose segment the machine's
   boot NIC is physically wired to. The machine's Site is derived from
   this reference (`Machine.spec.subnetRef -> Subnet.spec.siteRef ->
   Site`, `internal/sitederive`), never set directly - see section 2's
   introduction.
9. Let kezio power on the machine through its BMC and confirm it PXE
   boots, registers with the agent server, and reaches Ready.

## 8. e2e lanes measured against this matrix

kezio's KubeVirt-based GitHub Actions lane (`main.yaml`'s `e2e-bmc` job)
already labels its CI-only shortcuts in its own comments. This section
restates the ones that matter for the scenarios above, for an operator
comparing a lab run against a CI run. Some points below also reference
the now-retired multi-site lane (`docs/e2e-scale-multisite-kubevirt.md`),
kept for historical comparison.

- **The `e2e-bmc` lane exercises scenario 1, end to end.** The job sets
  `DHCP_SCENARIO: no-relay` (`main.yaml`'s `e2e-bmc` job). The
  `deploy-existing-dhcp` action stands up a separate dnsmasq pod that
  answers DHCP leases directly on the same segment, while bootd runs
  pure proxyDHCP beside it - the same shape scenario 1 describes for a
  real site. Scenario 2 (lease mode) uses the same tooling
  (`DHCP_SCENARIO: lease`), but no job in `main.yaml` sets it today, so
  a KubeVirt lane does not exercise lease mode end to end yet.
- **The dnsmasq stand-in for the site's DHCP server is a pinned,
  third-party image, not kezio's own.** Documented in
  `.github/actions/deploy-existing-dhcp`'s own description; this is
  intentional so the two dnsmasq instances stay distinguishable in
  logs, and is already labeled as a stand-in.
- **Tracker/seeder connectivity always uses Option 2** (Multus,
  same-bridge, static IPAM) - see section 3. This matches a real
  Option 2 deployment's shape; it does not exercise Option 1 (a routed
  L3 Service address) at all, since kezio ships no LoadBalancer/
  NodePort/hostPort variant of the tracker/seeder Services to exercise.
- **The BMC lane's Redfish endpoint is plain HTTP** (KubeVirtBMC's
  generated Service does not terminate TLS), reached with the
  `redfish+http://` scheme instead of `redfish://`. This is already
  commented in the workflow and is now also documented in
  `docs/bmc.md` as a lab/test-only scheme - a production BMC's Redfish
  endpoint should be reached with `redfish://` (HTTPS) instead.
- **Secure Boot is off in every VM these lanes boot** - already
  documented in `docs/secure-boot.md`, restated here because it
  intersects this guide's own scope: none of the network scenarios
  above interact with Secure Boot, so this gap does not change any
  guidance in this document.
- **The multi-site lane's data plane is not actually multi-network** -
  both simulated sites' BitTorrent/gRPC traffic crosses one flat pod
  network, documented and scoped explicitly in
  `docs/e2e-scale-multisite-kubevirt.md`'s "What it deliberately does
  NOT claim" section.

No lane was found presenting a CI-only shortcut as ordinary operator
practice without already labeling it as such. No workflow or action
file needed a labeling change for this review; `docs/bmc.md` gained the
`redfish+http://` note above since that gap was in the documentation,
not in the workflow's own comments.

### Coverage note: which scenarios are proven by the packet lab, and which by the KubeVirt lane

`hack/bootd-packet-lab.sh` gives scenario 1 (existing DHCP) and
scenario 2 (lease mode) a repeatable, real-packet assertion: a fresh
netns/veth topology, the real dnsmasq supervisor, and a PXE-shaped
client sending an actual DHCPDISCOVER, asserting the DHCPOFFER (or its
absence for a denied MAC) each scenario should produce.

What it does **not** claim: this is a DHCP/PXE packet-level assertion,
not a full boot-to-registration KubeVirt run - it stops once the
DHCPOFFER is verified, before TFTP, GRUB, the boot config server, or
agent registration. The `e2e-bmc` KubeVirt lane closes that gap for
scenario 1: it runs `DHCP_SCENARIO: no-relay` end to end, from PXE
through agent registration (see the bullet above). Scenario 2 (lease
mode) has no KubeVirt lane today; extending `e2e-bmc` (or adding a new
lane) with a `DHCP_SCENARIO: lease` variant remains open work.

## 9. Fact-check table

| Claim | File verified against |
|---|---|
| bootd never assigns IP leases; every `dhcp-range` carries `proxy` | `internal/bootd/render.go` |
| `BOOTD_LEASE_MODE` renders a lease-serving `dhcp-range` and `dhcp-boot`/`dhcp-match` instead of `pxe-service`; the MAC gate is unchanged | `internal/bootd/config.go` (`LeaseMode`), `internal/bootd/render.go`, `internal/bootd/render_test.go` |
| One bootd replica per segment | `config/bootd/deployment.yaml` (`replicas: 1`) |
| bootd needs a Multus attachment, not `hostNetwork` | `config/bootd/networkattachmentdefinition.example.yaml` |
| Namespace needs `pod-security.kubernetes.io/enforce=privileged` | `config/bootd/README.md`, `config/bootd/deployment.yaml` |
| bootd reverse-proxies `/agent/...` and `/boot/...` via `BOOTD_AGENT_UPSTREAM_URL` / `BOOTD_BOOT_UPSTREAM_URL` | `config/bootd/README.md`, `cmd/bootd/main.go` |
| Boot config server / agent server default Services are ClusterIP | `config/bootserver/service.yaml`, `config/agentserver/service.yaml` |
| Ports 8090 / 8091 | `config/bootserver/manager-port-patch.yaml`, `config/agentserver/manager-port-patch.yaml` |
| No-NAT rule for tracker/seeder | `config/seeder/README.md` |
| Tracker Service is ClusterIP-only, no LoadBalancer/NodePort variant shipped; no Service at all exists for a seeder pod | `config/seeder/opentracker-service.yaml`, `internal/controller/seeder_deployment.go` |
| e2e lanes use Multus same-bridge attachment for tracker/seeder | `.github/workflows/main.yaml` (`e2e-bmc` job) |
| Fixed BT port 16881 | `internal/controller/seeder_deployment.go` (`seederBTPort`) |
| Tracker port 6969 | `config/seeder/opentracker-deployment.yaml`, `config/seeder/opentracker-service.yaml` |
| Tracker is singular, not replicated per site | `config/seeder/README.md`'s introduction |
| A Machine's Site is derived (`spec.subnetRef` -> `Subnet.spec.siteRef` -> `Site`), never set directly; `spec.networkSite` does not exist | `internal/sitederive/sitederive.go`; `api/v1alpha1/machine_types.go` (no `networkSite` field) |
| BMC driver selection by URL scheme; IPMI default port 623 | `docs/bmc.md`, `internal/bmc/ipmi/ipmi.go` |
| Secure Boot chain and CI gap | `docs/secure-boot.md` |
| `e2e-bmc` runs `DHCP_SCENARIO: no-relay`, exercising scenario 1 end to end | `.github/workflows/main.yaml`, `.github/actions/deploy-bootd/action.yml`, `.github/actions/deploy-existing-dhcp/action.yml` |
| Scenario 1 and scenario 2 are both covered by the local packet lab's real-packet assertions; only scenario 1 also has a KubeVirt e2e lane | `internal/bootd/lab_test.go`, `internal/bootd/lab_client_test.go`, `hack/bootd-packet-lab.sh` |
| `redfish+http://` exists and is documented as a lab/test-only scheme | `internal/bmc/redfish/redfish.go` |
| KubeVirtBMC's Redfish Service is plain HTTP, reached via `redfish+http://` | `.github/workflows/main.yaml` (`BMC_REDFISH_ADDRESS`) |
| Multi-site lane's data plane is one flat pod network, not isolated per site | `docs/e2e-scale-multisite-kubevirt.md` |
