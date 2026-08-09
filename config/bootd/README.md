# kezio-bootd

bootd is the proxyDHCP and TFTP service a UEFI firmware talks to at the
very start of a network boot, before grub or the boot config server
(`config/bootserver`, `internal/bootserver`) ever come into the picture.
See `internal/bootd`'s package doc comment for the full protocol-level
design; this README covers what a Subnet needs before its bootd instance
comes up.

## Who creates the bootd Deployment

`SubnetReconciler` (`internal/controller/subnet_controller.go`) creates
one bootd Deployment per `Subnet` object, in that Subnet's own
namespace, and keeps it matching the Subnet's spec. `deployment.yaml` in
this directory is that Deployment's shape - `buildBootdDeployment`
(`internal/controller/bootd_deployment.go`) builds it directly, filling
in the SITE-SPECIFIC values below from the Subnet instead of the hand
edits this file used to need. **`deployment.yaml` is a reference, not
something you `kubectl apply`** - read it to see exactly what the
controller stamps; do not copy or apply it yourself.

This only happens once `config/default`'s controller-manager has
`BOOTD_DEPLOYMENT_IMAGE` and `BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE` set
(`cmd/main.go`'s `bootdDeploymentConfigFromEnv`); leaving them unset
disables bootd Deployment reconciliation entirely, the same
inert-by-default shape every other optional controller feature uses.

## What you still provide, per Subnet

Creating a `Subnet` object is not, on its own, enough for its bootd pod
to come up. Three things stay the operator's responsibility, once per
Subnet namespace, before (or alongside) creating the Subnet:

1. **The `kezio-bootd` ServiceAccount and the PSA label**, in the
   Subnet's own namespace - see "RBAC scope" and "Capabilities and Pod
   Security Admission" below. The Deployment the controller builds
   stamps `serviceAccountName: kezio-bootd` unconditionally; nothing
   here creates that ServiceAccount for you.
2. **The boot L2 segment's `NetworkAttachmentDefinition`**, also in the
   Subnet's namespace, named by `Subnet.spec.bootdNetworkRef` - see
   `networkattachmentdefinition.example.yaml` below.
3. **The Subnet object itself**, with `spec.bootdServerIP`,
   `spec.cidr`, `spec.bootdNetworkRef`, and `spec.dhcp` filled in - see
   "Per-site addressing" below and `config/samples/kezio_v1alpha1_subnet.yaml`.

`rbac.yaml` in this directory is still something you apply, once per
cluster, for the ClusterRole every bootd ServiceAccount binds to:

```sh
kustomize build config/default | kubectl apply -f -
kustomize build config/bootd    | kubectl apply -f -
```

See "RBAC scope" below for what that kustomization actually provisions,
and what it still leaves for you to repeat per Subnet namespace.

## No IP leases: bootd coexists with production DHCP

bootd never assigns an IP address. The site's production DHCP server
keeps sole ownership of leases; bootd only answers the PXE portion of
the boot exchange (which file to TFTP, and from which next-server) -
see `internal/bootd`'s package doc comment for the exact proxyDHCP
(port 67) and PXE boot-server (port 4011) roles it plays. Both services
can - and are meant to - run on the same broadcast domain without
conflicting.

The DHCP protocol work is done by a dnsmasq child process that bootd
renders the configuration for, supervises, and feeds the MAC allowlist
to (see `internal/bootd`'s package doc comment); TFTP stays served by
bootd itself.

## Own DHCP server mode (optional, isolated segments)

When the boot segment has no DHCP server at all, set
`BOOTD_LEASE_MODE=true`: bootd's dnsmasq then becomes the segment's own
DHCP lease authority, rendering a lease-serving `dhcp-range` instead of
the proxyDHCP one. The lease range defaults to the provisioning
subnet's first and last host addresses; set `BOOTD_LEASE_RANGE_START`
and `BOOTD_LEASE_RANGE_END` together to override it.

The MAC gate below is unchanged: only enrolled MACs receive a lease.
This mode does not turn bootd into a general-purpose DHCP server for
the segment - a device that is not an enrolled Machine still gets
nothing, and a site that also needs to serve unenrolled devices runs
its own DHCP server for them on a different segment instead.

## Before creating a Subnet

1. **`spec.bootdServerIP` and `spec.cidr`.** The pod's actual reachable
   IP address on the boot L2 segment (the Multus attachment below, not a
   cluster-internal address) and that segment's IPv4 subnet. Firmware
   reads the address back as the PXE boot server and (unless
   `BOOTD_NEXT_SERVER_IP` overrides it, a bootd process-level setting
   this Subnet field does not carry) the TFTP next-server; the subnet
   becomes dnsmasq's proxyDHCP `dhcp-range`.
2. **Attach the boot L2 segment.** See
   `networkattachmentdefinition.example.yaml` and its extended comments
   on why this uses Multus rather than `hostNetwork`. Copy it, fill in
   the site-specific interface/IPAM values, and apply it in the Subnet's
   own namespace under its final name. Point `spec.bootdNetworkRef` at
   that name - the controller stamps the
   `k8s.v1.cni.cncf.io/networks` annotation for you from that field, so
   there is no `deployment.yaml` to hand-edit or uncomment anything on.
3. **TFTP artifacts volume is already populated for you.** The
   Deployment the controller builds mounts an `emptyDir` at `/tftp`,
   filled in by a `fetch-boot-artifacts` initContainer that `cp`s
   `shimx64.efi` and `grubx64.efi` (see `internal/bootd.ShimFilename` /
   `internal/bootd.GrubFilename`) straight out of the `kezio-boot-
   artifacts` OCI image (see `.github/workflows/build-live-image.yml`,
   which builds and pushes it with the signed shim/grub bundled
   alongside the kernel/initrd/squashfs) before `bootd` itself starts -
   no PVC, ConfigMap, or custom initContainer to write yourself. The
   image reference comes from the controller-manager's
   `BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE`, set once for the whole
   cluster; pin a specific published version there rather than per
   Subnet. An unpopulated volume (for example, the initContainer failing
   before the main container ever starts) leaves the TFTP server unable
   to serve either file - a clean per-request error, not a startup crash
   - see `internal/bootd.TFTPServer`'s doc comment. Both files are the
   real Debian-signed shim/GRUB binaries, verified as signed when they
   are built - see `../../docs/secure-boot.md` for the full signature
   chain and what that means for a machine that keeps UEFI Secure Boot
   on.
4. **One replica per boot segment.** The Deployment the controller
   builds is pinned to `replicas: 1` - two bootd pods answering the same
   broadcast domain would both reply to every DHCPDISCOVER, and firmware
   has no way to prefer one proxyDHCP answer over the other. One Subnet
   object per site/segment (each with its own Multus attachment) is what
   keeps that true; nothing here raises the replica count per Subnet.

## Reverse-proxying the agent and boot config servers

bootd can also front the two in-cluster HTTP APIs a net-booting machine
needs: `internal/agentserver`'s registration/poll/progress endpoints and
`internal/bootserver`'s GRUB config and live-artifact endpoints (see
`config/agentserver/README.md` and `config/bootserver/README.md`). Set
the controller-manager's `BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL` and/or
`BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` (`cmd/main.go`'s
`bootdDeploymentConfigFromEnv`) to those servers' in-cluster base URLs
(their Services' cluster-DNS names, for example
`http://kezio-agent-server.kezio-system.svc.cluster.local:8091`) and
every Subnet's bootd reverse-proxies every `/agent/...` request to the
first and every `/boot/...` request to the second, listening on
`BOOTD_PROXY_ADDR` (default: that Subnet's own `BOOTD_SERVER_IP` with
port 80 - the boot segment's own address, not every interface the pod
happens to have). Setting `BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` also makes
`buildBootdDeployment` derive `BOOTD_BOOT_CONFIG_URL` (above)
automatically, pointed at that same bootd address - nothing to fill in by
hand. Point the agent's `kezio.server=` source (`BOOT_SERVER_URL` /
`BOOT_AGENT_SERVER_URL`, see those READMEs) at that same bootd address
instead of a separately-exposed Service, and a machine on this boot
segment needs exactly one reachable address for the whole
boot-to-registration flow: this bootd pod's own.

Both upstream URLs are independent and both default to unset - a cluster
that sets neither leaves every Subnet's bootd behaving exactly as it did
before this proxy existed, still answering DHCP/PXE/TFTP alone and
proxying nothing. Enabling one enables only that route prefix, for every
Subnet's bootd at once (there is no per-Subnet override) - the other
route stays whatever it was before.

## Per-site addressing: production VLANs and multisite work the same way

This model scales to a production deployment with one provisioning VLAN
per site the same way it does in a single-segment lab: each Subnet
object carries its own `bootdServerIP` on its own segment, and
`SubnetReconciler` creates that site's bootd Deployment, in that
Subnet's own namespace, from it - so every site's bootd instance ends up
with its own address and Multus attachment without any per-site copy of
`deployment.yaml` or `config/bootd` itself. `BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL`
/ `BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL` (above) point every one of those
Deployments at the same cluster-wide agent/boot Services - there is
exactly one `internal/agentserver` and one `internal/bootserver` for the
whole cluster, but every site's bootd reverse-proxies to them
independently, so every site's machines still only ever need to reach
their own local bootd address. Adding a second (or third, ...) site is
therefore: create its Subnet object (with its own `cidr`,
`bootdServerIP`, and `bootdNetworkRef`), apply its NAD, and provision its
`kezio-bootd` ServiceAccount - see "What you still provide, per Subnet"
above.

## UEFI HTTP Boot is not supported

dnsmasq's proxyDHCP engine only answers `PXEClient` requests; a
firmware asking for UEFI HTTP Boot (option 60 `HTTPClient`) receives no
proxy answer from bootd at all. A site that needs HTTP Boot must
configure its production DHCP server to hand out the boot URL;
`internal/bootserver`'s `GET /boot/http/<name>` route still serves the
signed `shimx64.efi`/`grubx64.efi` artifacts themselves. The former
`BOOTD_HTTP_BOOT_URL` variable is rejected at startup rather than
silently ignored.

## Replies stay on the boot network, even with a second pod interface

Because this pod normally has two network interfaces - the cluster's
default one (used for the Kubernetes API traffic bootd's Machine watch
needs) and the Multus-attached boot L2 segment above - DHCP must not
follow the process's default route, which almost always points at the
cluster interface. `BOOTD_DHCP_INTERFACE` (default `net1` in
`deployment.yaml`) makes dnsmasq bind its DHCP sockets to the boot
segment's interface exclusively (`bind-interfaces`), so every offer
leaves by the same interface the DHCPDISCOVER came in on.

## MAC gating: fail-secure by default

bootd only hands boot information to a client whose MAC address matches
an enrolled Machine's `spec.bootMACAddress` - it never net-boots an
unrecognized device on the segment unless `BOOTD_ANSWER_ALL=true` is
set (off by default). The enrolled-MAC set is a locally cached watch of
Machine objects (`internal/bootd.MACCache`) that bootd renders into a
dnsmasq `dhcp-hostsfile` and applies with a SIGHUP - enrolling or
deleting a Machine takes effect without a dnsmasq restart. The gate
fails secure: the hostsfile starts empty (nothing boots) until the
watch's first sync completes, and stays empty forever if the sync never
completes (API server unreachable at startup) - restart bootd once
connectivity is restored. See `internal/bootd/maccache.go`'s doc
comment for the full reasoning. A denied MAC receives nothing from
bootd on either DHCP port.

## Capabilities and Pod Security Admission

dnsmasq refuses to serve DHCP without `NET_ADMIN` and `NET_RAW`
(checked explicitly at its startup), and binding ports 67/4011/69 needs
`NET_BIND_SERVICE`. `deployment.yaml` drops all capabilities, adds
exactly those three, and - unlike every other kezio container - runs
bootd as uid 0. Root is forced by Kubernetes' capability semantics, not
chosen: added capabilities reach a container's permitted/effective sets
only for uid 0; for a non-root uid they land in the bounding set alone,
execve of a binary without file capabilities clears permitted, and
`allowPrivilegeEscalation: false` forbids regaining them - so a
non-root bootd has nothing to hand its dnsmasq child, which dies at
startup missing `NET_ADMIN` (see `internal/bootd/caps.go`). dnsmasq
itself runs with `--user=root`: its default privilege drop to `nobody`
needs `SETUID`/`SETGID`, which the pod deliberately does not grant.

The Pod Security Admission consequence: `NET_ADMIN` is outside both the
`restricted` and `baseline` profiles' allowed capability lists, so
**every namespace that will hold a Subnet** must be labeled
`pod-security.kubernetes.io/enforce=privileged` before its bootd pod can
start - the controller does not set this label; a namespace missing it
admission-rejects the pod outright. That label relaxes admission-time
enforcement only - the pod itself still grants nothing beyond root plus
the three capabilities above: no `privileged: true`, no `hostNetwork`,
no `hostPort`, read-only root filesystem, seccomp `RuntimeDefault`.

Setting the label is still the operator's own setup step, but a missing
one does not go unnoticed: `SubnetReconciler` checks for it on every
reconcile and writes the result as a `BootdNamespacePSALabel` condition
on the Subnet (`checkBootdNamespacePrerequisites`,
`internal/controller/subnet_controller.go`). A namespace missing the
label gets that condition with reason `BootdNamespacePSALabelMissing`;
run `kubectl get subnet <name> -o yaml` (or `kubectl describe subnet
<name>`) and look at `status.conditions` to find it.

## RBAC scope

`rbac.yaml` grants a **ClusterRole** (`get`, `list`, `watch` on
`machines`), not a namespaced Role: bootd's MAC cache watches every
Machine in the cluster, since a site's bootd has no reliable way to know
in advance which namespace(s) the Machines it should net-boot live in.
Applying `rbac.yaml` once, cluster-wide, provisions that ClusterRole for
good.

**What `rbac.yaml` does not do:** it provisions one `kezio-bootd`
ServiceAccount, in `kezio-system` only, and binds it to the ClusterRole.
Every Subnet's bootd Deployment stamps `serviceAccountName: kezio-bootd`
(`bootdDefaultServiceAccountName`,
`internal/controller/bootd_deployment.go`) unconditionally, without
creating that ServiceAccount - so **a `kezio-bootd` ServiceAccount must
exist in every namespace that will hold a Subnet**, each bound to the same
ClusterRole (add a subject to `rbac.yaml`'s ClusterRoleBinding, or bind
each namespace's ServiceAccount with its own RoleBinding to the same
ClusterRole). Provisioning it stays the operator's own setup step, the
same as the PSA label above - nothing in this kustomization creates it.
But a missing ServiceAccount does not fail silently: `SubnetReconciler`
checks for it on every reconcile and writes the result as a
`BootdServiceAccount` condition on the Subnet
(`checkBootdNamespacePrerequisites`,
`internal/controller/subnet_controller.go`). A Subnet whose namespace
has no `kezio-bootd` ServiceAccount gets that condition with reason
`BootdServiceAccountMissing`; run `kubectl get subnet <name> -o
yaml` (or `kubectl describe subnet <name>`) and look at
`status.conditions` to find it, instead of inspecting the bootd pod by
hand.
