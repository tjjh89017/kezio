# Manual physical lab deployment guide

This guide is for an operator who builds a kezio lab on real hardware,
with no CI and no GitHub Actions. It lists the network shapes kezio
supports today, the prerequisites the operator must set up, and the
full set of ports and addresses the operator must open and verify.

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

## 1. The three network scenarios

kezio-bootd never assigns an IP address itself. It answers only the
PXE part of a boot exchange: which file to fetch over TFTP, and from
which server. A separate DHCP source must always hand out the IP
lease. The three scenarios below cover the three ways an operator can
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

Setup: do not set `BOOTD_DHCP_RELAY_SERVER`. Leave it unset. This is
the default.

This no-relay shape is exercised by the in-repo packet lab
(`internal/bootd/lab_test.go`'s `TestDnsmasqLab`, `BOOTD_LAB=1`), which
runs the real dnsmasq supervisor against a real netns topology with
`BOOTD_LAB_RELAY` left unset. kezio's KubeVirt-based GitHub Actions
lanes do not separately exercise this no-relay shape end to end - see
section 8 for what those lanes cover instead.

### Scenario 2: existing DHCP reachable but not on the segment (relay mode)

Use this scenario when the site's DHCP server exists but does not sit
on the provisioning segment itself - for example, it lives on a
different VLAN reachable only through routed IP.

Set `BOOTD_DHCP_RELAY_SERVER` to that DHCP server's IP address. bootd's
dnsmasq then relays every DHCP request it hears on the segment to that
address (`dhcp-relay`), and relays the reply back to the requesting
client (`internal/bootd/config.go`'s `RelayServerIP` field;
`internal/bootd/render.go`'s `dhcp-relay=<local>,<remote>` line). The
relay path is independent of bootd's MAC gate: a denied MAC's lease
request still gets relayed, it just receives no PXE boot information
(`config/bootd/README.md`, "DHCP relay support (optional)").

`dhcp-relay` is standard DHCP relay behavior: the remote DHCP server
does not have to be on-link, as long as normal IP routing carries the
relayed packet to it and back. What must be true is that bootd's own
provisioning-network interface (`BOOTD_DHCP_INTERFACE`) has a route to
that address.

Setup:

- Set `BOOTD_DHCP_RELAY_SERVER=<site DHCP server IP>`.
- Confirm bootd's provisioning interface can route to that address.

### Scenario 3: isolated segment - PLANNED, NOT YET AVAILABLE

A provisioning segment with no DHCP server of its own, and no route to
one, needs bootd itself to hand out IP leases. **This mode does not
exist in kezio today.** bootd's `Config` has no field, and `cmd/bootd`
has no environment variable, that turns dnsmasq into a real DHCP lease
server; every rendered `dhcp-range` line always carries the `proxy`
flag (`internal/bootd/render.go`), which makes dnsmasq a proxyDHCP
responder only, never a lease authority.

Do not deploy `config/bootd` against a segment with no DHCP server
reachable by scenario 1 or scenario 2 above - it will not net boot
anything, because no IP lease will ever be handed out. Isolated-segment
support is planned separate work. Until it ships, an isolated segment
needs a temporary DHCP server of the operator's own (making the segment
scenario 1 in practice) as the only working option.

## 2. Network prerequisites the operator owns

### 2.1 Provisioning L2 per site

Each site needs one provisioning L2 segment (a VLAN or a dedicated
switch/bridge) that every target machine's boot NIC is a member of.
Deploy exactly one `config/bootd` instance per segment
(`config/bootd/deployment.yaml` is pinned to `replicas: 1` - two bootd
pods answering the same broadcast domain would both reply to every
DHCPDISCOVER, and firmware cannot prefer one answer over the other).

A production deployment with more than one site or VLAN runs one
`config/bootd` overlay per segment, each with its own
`BOOTD_SERVER_IP` / `BOOTD_PROVISIONING_CIDR` / Multus attachment, all
proxying to the same cluster-wide agent and boot config servers
(`config/bootd/README.md`, "Per-site addressing").

### 2.2 Multus NAD wiring for bootd

bootd's pod needs a second network interface on the provisioning
segment, attached with Multus, not `hostNetwork`
(`config/bootd/networkattachmentdefinition.example.yaml`). Reasons
given in that file: bootd must see the exact broadcast domain the
booting machine's NIC is on, and its unrelayed proxyDHCP replies must
reach only that segment, not every network the node's `eth0` touches.

Steps:

1. Copy `config/bootd/networkattachmentdefinition.example.yaml`, strip
   the `.example` suffix, and fill in the site's real CNI plugin
   (macvlan, ipvlan, or a bridge onto a VLAN-tagged host interface).
2. Add it to `config/bootd/kustomization.yaml`'s `resources`.
3. Uncomment the `k8s.v1.cni.cncf.io/networks` annotation on
   `deployment.yaml`'s pod template.
4. Set `BOOTD_SERVER_IP` and `BOOTD_PROVISIONING_CIDR` to that
   segment's real address and subnet.
5. Set `BOOTD_DHCP_INTERFACE` to the Multus interface name (`net1` in
   the shipped manifest).

Target machines attach only to this provisioning bridge - they must
not share it with unrelated cluster or data-plane traffic.

### 2.3 The namespace needs privileged Pod Security Admission

dnsmasq refuses to serve DHCP without `NET_ADMIN` and `NET_RAW`. Those
capabilities are outside both the `restricted` and `baseline` Pod
Security Admission profiles, so the namespace bootd deploys into must
carry the label `pod-security.kubernetes.io/enforce=privileged`
(`config/bootd/README.md`, "Capabilities and Pod Security Admission").
This relaxes admission-time enforcement only; the bootd pod itself
still grants nothing beyond those three capabilities plus root, with
every other capability dropped.

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

Every `Machine` with a `spec.bmc.address` needs the controller-manager
to reach that BMC endpoint over the cluster's own network path -
`internal/bmc` picks a `redfish://`, `ipmi://`, or `ipmitool://` driver
from the address's URL scheme (`docs/bmc.md`). Whatever L3 path
connects the cluster to the site (see section 2.6) must also carry BMC
traffic to every machine's board management controller. A `Machine`
with no `spec.bmc` set has no reachable BMC at all: it still net
boot-waits for inspection and deployment, but its power state is left
to whoever operates it (`api/v1alpha1/machine_types.go`'s `BMC` field
doc comment).

### 2.6 Cluster to site L3 baseline

kezio assumes the cluster already has routed L3 connectivity to every
site's provisioning segment, BMC network, and data network. It does
not ship or require any particular VPN or overlay technology
(`config/seeder/README.md`, "Cross-network baseline"). If sites are not
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
  `config/seeder/ezio-seeder-deployment.yaml`).
- The address the tracker hands back to peers must be the seeder's
  real, reachable address. Nothing on the path may rewrite it.

Neither option below is "the" answer for every site. Pick per the
trade-offs.

### Option 1: routed L3 to a stable cluster Service address

Give the tracker and seeder a stable, routable address reachable from
every provisioning segment over ordinary L3 routing - a
LoadBalancer, NodePort, or hostPort in front of the tracker/seeder
Services, with the operator responsible for provisioning-to-service
routing.

As shipped, `config/seeder/ezio-seeder-service.yaml` and
`config/seeder/opentracker-service.yaml` are both `type: ClusterIP`,
deliberately: their own comments state this is cluster-internal
reachability only, because a Service's ClusterIP DNATs traffic, which
breaks the no-NAT rule for the BitTorrent data port. **kezio does not
ship a LoadBalancer/NodePort/hostPort variant of these Services.** An
operator choosing Option 1 must patch a further overlay that exposes
the tracker's announce port (`6969`) and each seeder's BitTorrent port
(`16881`) at a stable address reachable from every site, without any
NAT rewriting that address on the path - a plain ClusterIP does not
satisfy that; only a genuinely routable exposure (for example
`hostNetwork`, `hostPort`, or an external L3 LoadBalancer with no
SNAT/DNAT between it and the pod) does.

This option centralizes routing decisions at the cluster edge and
needs no per-site bridge attachment for the tracker/seeder pods
themselves, at the cost of building and maintaining that exposure
yourself.

### Option 2: same subnet/bridge as the targets, via Multus

Attach the tracker and each seeder pod to the same data-network bridge
the target machines' data NICs live on, as a Multus secondary
interface (`net1`), the same way `config/bootd` attaches its own
provisioning interface
(`config/seeder/networkattachmentdefinition.example.yaml`). `eth0`
stays for cluster-internal traffic only; the data network carries
BitTorrent peer connections and tracker announce/response traffic
directly, with no Service and no NAT in the path at all.

This is the shape kezio's own end-to-end lanes use today
(`.github/workflows/e2e-kubevirt-reusable.yml` creates
`NetworkAttachmentDefinition`s for the boot network, the tracker, and
the seeder, all as static-IPAM Multus attachments on the same
provisioning bridge).

This option needs a Multus attachment per site for every tracker/seeder
pod (or, for a single central tracker, at least routed reachability to
whichever site-local bridge it needs to answer from), but avoids
building and maintaining a separate L3 exposure layer.

### Common ground: routing, not a default-route flip

Whichever option is used, do not make the data-network interface the
pod's default route. Give it specific routes scoped to the data
network only, through the NetworkAttachmentDefinition's own routing
configuration; leave `eth0` carrying the cluster's own pod/service
CIDRs untouched (`config/seeder/README.md`, "Routing").

### The tracker is not replicated per site

There is exactly one tracker in a kezio deployment
(`config/seeder/README.md`, "Per-site seeders"). It must be reachable,
by whichever option above, from every site. A site-local seeder is a
second replica of the same `ezio-seeder` component, added as another
endpoint of the same Kubernetes Service - see that section for the
full pattern before adding a second site.

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
| 16881 | TCP | ezio-seeder BitTorrent listen port | `config/seeder/ezio-seeder-deployment.yaml` (`EZIO_BT_PORT: "16881"`) |

This matches the port list this guide was scoped against exactly - no
discrepancy was found between the expected list and the code.

Two more ports exist in the system but were out of the original list
and are worth knowing about when building firewall rules:

| Port | Protocol | Service | Verified against |
|---|---|---|---|
| 80 | TCP | bootd's own reverse proxy front (`BOOTD_PROXY_ADDR`, section 2.4) - only listened on once `BOOTD_AGENT_UPSTREAM_URL` or `BOOTD_BOOT_UPSTREAM_URL` is set | `config/bootd/deployment.yaml` container port `proxy-http`; `cmd/bootd/main.go`'s `bootdConfigFromEnv` |
| 50051 | TCP | ezio-seeder gRPC control port (cluster-internal only; the operator's `SeederReconciler` talks to it, not a booting machine) | `config/seeder/ezio-seeder-deployment.yaml` (`EZIO_GRPC_LISTEN`); `config/seeder/ezio-seeder-service.yaml` |

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
3. Deploy `config/seeder` (opentracker + ezio-seeder); set
   `SEEDER_TRACKER_URL`, `SEEDER_STORE_ROOT`,
   `SEEDER_SERVICE_NAMESPACE`, `SEEDER_SERVICE_NAME` on the
   controller-manager Deployment.
4. For each site: create the provisioning NAD (section 2.2), deploy
   `config/bootd`, and choose one of the three scenarios in section 1.
5. Choose and wire one tracker/seeder connectivity option from
   section 3, per site if needed.
6. Enroll each `Machine`: set `spec.bootMACAddress`, `spec.bmc.address`
   (if a BMC is available), and `spec.networkSite`. `networkSite` is
   descriptive bookkeeping for the operator (it is not read by any
   controller to route a machine to a specific bootd instance - see
   `api/v1alpha1/machine_types.go`'s `NetworkSite` field doc comment);
   the machine's actual bootd instance is whichever one physically
   answers on the segment its boot NIC is wired to.
7. Power on the machine (through its BMC, or manually) and confirm it
   PXE boots, registers with the agent server, and reaches Ready.

## 8. e2e lanes measured against this matrix

kezio's KubeVirt-based GitHub Actions lanes
(`e2e-kubevirt-reusable.yml`, `e2e-boot-path-kubevirt.yml`,
`e2e-scale-multisite-kubevirt.yml`) already label their CI-only
shortcuts in their own comments. This section restates the ones that
matter for the scenarios above, for an operator comparing a lab run
against a CI run.

- **The lanes always exercise relay mode, on-link.** Every lane that
  simulates "the site already has a DHCP server"
  (`.github/actions/deploy-existing-dhcp`) sets
  `BOOTD_DHCP_RELAY_SERVER` to that stand-in server's address, even
  though the stand-in sits on the same `/24` as bootd - a shape where a
  real deployment (scenario 1) would not need relay at all. The
  action's own comment already states this plainly ("a plain on-link
  unicast, since both sit on the same /24, so no host-side routing of
  any kind is needed for the relay leg to work"), so this is a labeled
  CI simplification, not a hidden one. What it proves is that the
  relay code path itself works; it is not a demonstration of scenario 1
  running without relay. Scenario 1 without relay is covered instead by
  the in-repo packet lab (`internal/bootd/lab_test.go`).
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

### Recommendation for later (not done this cycle)

Add a dedicated assertion (in the packet lab, or a future lightweight
CI lane) that scenario 1 - proxyDHCP alongside an on-segment DHCP
server, with `BOOTD_DHCP_RELAY_SERVER` unset - completes a full PXE
boot the same way the existing relay-mode lanes do. Today that shape is
only covered at the dnsmasq-config-rendering unit-test level
(`internal/bootd/render_test.go`) and the local packet lab, not by a
full boot-to-registration KubeVirt run. This is a coverage gap, not a
correctness bug, and restructuring an existing KubeVirt lane to add it
was out of scope for this change while GitHub Actions cannot be used to
verify the result.

## 9. Fact-check table

| Claim | File verified against |
|---|---|
| bootd never assigns IP leases; every `dhcp-range` carries `proxy` | `internal/bootd/render.go` |
| `BOOTD_DHCP_RELAY_SERVER` enables `dhcp-relay`; empty means proxyDHCP only | `internal/bootd/config.go` (`RelayServerIP`), `cmd/bootd/main.go` (`bootdConfigFromEnv`) |
| Relay is independent of the MAC gate | `internal/bootd/render.go`'s doc comment |
| No bootd config field implements a self-serving lease/isolated-segment mode | `internal/bootd/config.go`, `internal/bootd/render.go` (no such field or rendered line) |
| One bootd replica per segment | `config/bootd/deployment.yaml` (`replicas: 1`) |
| bootd needs a Multus attachment, not `hostNetwork` | `config/bootd/networkattachmentdefinition.example.yaml` |
| Namespace needs `pod-security.kubernetes.io/enforce=privileged` | `config/bootd/README.md`, `config/bootd/deployment.yaml` |
| bootd reverse-proxies `/agent/...` and `/boot/...` via `BOOTD_AGENT_UPSTREAM_URL` / `BOOTD_BOOT_UPSTREAM_URL` | `config/bootd/README.md`, `cmd/bootd/main.go` |
| Boot config server / agent server default Services are ClusterIP | `config/bootserver/service.yaml`, `config/agentserver/service.yaml` |
| Ports 8090 / 8091 | `config/bootserver/manager-port-patch.yaml`, `config/agentserver/manager-port-patch.yaml` |
| No-NAT rule for tracker/seeder | `config/seeder/README.md` |
| Tracker/seeder Services are ClusterIP-only, no LoadBalancer/NodePort variant shipped | `config/seeder/ezio-seeder-service.yaml`, `config/seeder/opentracker-service.yaml` |
| e2e lanes use Multus same-bridge attachment for tracker/seeder | `.github/workflows/e2e-kubevirt-reusable.yml` |
| Fixed BT port 16881 | `config/seeder/ezio-seeder-deployment.yaml` |
| Tracker port 6969 | `config/seeder/opentracker-deployment.yaml`, `config/seeder/opentracker-service.yaml` |
| Tracker is singular, not replicated per site | `config/seeder/README.md` ("Per-site seeders") |
| `spec.networkSite` is descriptive only, not consumed by any controller | `api/v1alpha1/machine_types.go`; confirmed no other reference in `*.go` outside that file |
| BMC driver selection by URL scheme; IPMI default port 623 | `docs/bmc.md`, `internal/bmc/ipmi/ipmi.go` |
| Secure Boot chain and CI gap | `docs/secure-boot.md` |
| CI's existing-dhcp fixture always sets `BOOTD_DHCP_RELAY_SERVER`, even on-link | `.github/actions/deploy-existing-dhcp/action.yml`, `.github/workflows/e2e-kubevirt-reusable.yml` |
| No-relay shape is covered by the local packet lab, not a KubeVirt e2e lane | `internal/bootd/lab_test.go` |
| `redfish+http://` exists and is documented as a lab/test-only scheme | `internal/bmc/redfish/redfish.go` |
| KubeVirtBMC's Redfish Service is plain HTTP, reached via `redfish+http://` | `.github/workflows/e2e-kubevirt-reusable.yml` (`BMC_REDFISH_ADDRESS`) |
| Multi-site lane's data plane is one flat pod network, not isolated per site | `docs/e2e-scale-multisite-kubevirt.md` |
