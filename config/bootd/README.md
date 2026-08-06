# kezio-bootd

This kustomization deploys kezio-bootd: the proxyDHCP and TFTP services a
UEFI firmware talks to at the very start of a network boot, before grub
or the boot config server (`config/bootserver`, `internal/bootserver`)
ever come into the picture. See `internal/bootd`'s package doc comment
for the full protocol-level design; this README covers what to set up
before applying it.

It is a standalone kustomization, applied as an addition to
`config/default` (like `config/image-service` and `config/seeder`), not
composed with it the way `config/bootserver` is - bootd is not part of
the `controller-manager` process at all, it is its own binary
(`cmd/bootd`) and its own Deployment:

```sh
kustomize build config/default | kubectl apply -f -
kustomize build config/bootd    | kubectl apply -f -
```

Both apply into the `kezio-system` namespace.

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

## DHCP relay support (optional)

When the boot segment has no DHCP server of its own, set
`BOOTD_DHCP_RELAY_SERVER` to the site's existing DHCP server address:
bootd's dnsmasq then also relays every DHCP request heard on the
segment to that server (`dhcp-relay`), whose replies are relayed back
to the client. The relay path is independent of the MAC gate below -
denied MACs still get their leases relayed, they just receive no PXE
boot information. Leave it unset (the default) for proxyDHCP only:
bootd never touches lease traffic, and the site must make its DHCP
server reachable on the segment by other means.

## Before applying

1. **Set `BOOTD_SERVER_IP` and `BOOTD_PROVISIONING_CIDR`.**
   `deployment.yaml`'s env has `REPLACE_WITH_BOOTD_BOOT_NETWORK_IP` and
   `REPLACE_WITH_BOOT_NETWORK_CIDR` placeholders - the pod's actual
   reachable IP address on the boot L2 segment (the Multus attachment
   below, not a cluster-internal address) and that segment's IPv4
   subnet. Firmware reads the address back as the PXE boot server and
   (unless `BOOTD_NEXT_SERVER_IP` overrides it) the TFTP next-server;
   the subnet becomes dnsmasq's proxyDHCP `dhcp-range`.
2. **Attach the boot L2 segment.** See
   `networkattachmentdefinition.example.yaml` and its extended comments
   on why this uses Multus rather than `hostNetwork`. Copy it, fill in
   the site-specific interface/IPAM values, add it to
   `kustomization.yaml`'s resources, and uncomment the
   `k8s.v1.cni.cncf.io/networks` annotation on `deployment.yaml`'s pod
   template.
3. **TFTP artifacts volume is already populated for you.**
   `deployment.yaml` mounts an `emptyDir` at `/tftp`, filled in by a
   `fetch-boot-artifacts` initContainer that downloads `shimx64.efi` and
   `grubx64.efi` (see `internal/bootd.ShimFilename` /
   `internal/bootd.GrubFilename`) from the repository's published
   live-image release (see `.github/workflows/build-live-image.yml`,
   which now bundles the signed shim/grub alongside the kernel/initrd/
   squashfs) before `bootd` itself starts - no PVC, ConfigMap, or custom
   initContainer to write yourself. By default it fetches the
   repository's *latest* release; pin a specific one with `kubectl set
   env deployment/kezio-bootd -c fetch-boot-artifacts
   BOOT_ARTIFACTS_VERSION=v0.1.0` (or a further kustomize patch)
   instead. An unpopulated volume (for example, the initContainer
   failing before the main container ever starts) leaves the TFTP
   server unable to serve either file - a clean per-request error, not
   a startup crash - see `internal/bootd.TFTPServer`'s doc comment.
   Both files are the real Debian-signed shim/GRUB binaries, verified as
   signed when they are built - see `../../docs/secure-boot.md` for the
   full signature chain and what that means for a machine that keeps
   UEFI Secure Boot on.
4. **One replica per boot segment.** `deployment.yaml` is pinned to
   `replicas: 1` - two bootd pods answering the same broadcast domain
   would both reply to every DHCPDISCOVER, and firmware has no way to
   prefer one proxyDHCP answer over the other. Deploy this
   kustomization once per site/segment (each with its own Multus
   attachment and, if the segments are on different subnets, its own
   overlay), not by raising this replica count.

## Reverse-proxying the agent and boot config servers

bootd can also front the two in-cluster HTTP APIs a net-booting machine
needs: `internal/agentserver`'s registration/poll/progress endpoints and
`internal/bootserver`'s GRUB config and live-artifact endpoints (see
`config/agentserver/README.md` and `config/bootserver/README.md`). Set
`BOOTD_AGENT_UPSTREAM_URL` and/or `BOOTD_BOOT_UPSTREAM_URL` to those
servers' in-cluster base URLs (their Services' cluster-DNS names, for
example
`http://kezio-agent-server.kezio-system.svc.cluster.local:8091`) and
bootd reverse-proxies every `/agent/...` request to the first and every
`/boot/...` request to the second, listening on `BOOTD_PROXY_ADDR`
(default: `BOOTD_SERVER_IP` with port 80 - the boot segment's own
address, not every interface the pod happens to have). Point
`BOOTD_BOOT_CONFIG_URL` (above) and the agent's `kezio.server=` source
(`BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL`, see those READMEs) at that
same bootd address instead of a separately-exposed Service, and a
machine on this boot segment needs exactly one reachable address for the
whole boot-to-registration flow: this bootd pod's own.

Both upstream URLs are independent and both default to unset - a bootd
deployment that sets neither behaves exactly as it did before this proxy
existed, still answering DHCP/PXE/TFTP alone and proxying nothing.
Enabling one enables only that route prefix; the other is still whatever
it was before (unproxied, or proxied by a second bootd instance at a
different site - see "Per-site addressing" below).

## Per-site addressing: production VLANs and multisite deploy the same way

This model scales to a production deployment with one provisioning VLAN
per site the same way it does in a single-segment lab: each site's bootd
instance gets its own `BOOTD_SERVER_IP` on its own segment, its own
`BOOTD_PROXY_ADDR`, and its own copy of `BOOTD_AGENT_UPSTREAM_URL` /
`BOOTD_BOOT_UPSTREAM_URL` pointing at the (shared, cluster-wide)
agent/boot Services - there is exactly one `internal/agentserver` and one
`internal/bootserver` for the whole cluster, but every site's bootd
reverse-proxies to them independently, so every site's machines still
only ever need to reach their own local bootd address. Deploying a
second (or third, ...) `config/bootd` overlay with a different
`BOOTD_SERVER_IP`/`BOOTD_PROVISIONING_CIDR`/Multus attachment per site,
each with the proxy env vars above pointing at the same in-cluster
Services, is the whole multisite story - no per-site copy of the agent
or boot server itself is needed.

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
bootd on either DHCP port; if relaying is enabled, its lease traffic is
still relayed.

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
`restricted` and `baseline` profiles' allowed capability lists, so the
namespace bootd deploys into must be labeled
`pod-security.kubernetes.io/enforce=privileged`. That label relaxes
admission-time enforcement only - the pod itself still grants nothing
beyond root plus the three capabilities above: no `privileged: true`,
no `hostNetwork`, no `hostPort`, read-only root filesystem, seccomp
`RuntimeDefault`.

## RBAC scope

`rbac.yaml` grants `bootd`'s ServiceAccount a **ClusterRole** (`get`,
`list`, `watch` on `machines`), not a namespaced Role: bootd's MAC cache
watches every Machine in the cluster, since a site's bootd has no
reliable way to know in advance which namespace(s) the Machines it
should net-boot live in. If every Machine at a site is known to live in
a single namespace, narrow this to a namespaced Role + RoleBinding in an
overlay.
