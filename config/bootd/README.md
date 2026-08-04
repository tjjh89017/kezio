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

## DHCP relay support

bootd answers correctly whether a DHCPDISCOVER/DHCPREQUEST arrived by
direct L2 broadcast or by unicast through a DHCP relay agent (`giaddr`
set): a relayed request's reply is unicast back to the relay's own
address on the DHCP server port, exactly as RFC 2131 requires, and the
relay is responsible for delivering it on to the client from there. See
`internal/bootd.BuildResponse`'s doc comment and `destinationFor` for
the exact rule, and `internal/bootd/dhcp_test.go` for the packet-level
test covering both the relayed and non-relayed cases.

## Before applying

1. **Set `BOOTD_SERVER_IP`.** `deployment.yaml`'s env has a
   `REPLACE_WITH_BOOTD_BOOT_NETWORK_IP` placeholder - this must be the
   pod's actual reachable IP address on the boot L2 segment (the Multus
   attachment below), not a cluster-internal address. Firmware reads it
   back as both the DHCP Server Identifier and (unless
   `BOOTD_NEXT_SERVER_IP` overrides it) the TFTP next-server address.
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

## UEFI HTTP Boot (optional, alternative to PXE+TFTP)

Some UEFI firmware can fetch its boot loader directly over HTTP(S)
instead of PXE+TFTP - useful mainly on a routed L3 boot network, where
TFTP's broadcast-friendly assumptions are more awkward than a plain
HTTP fetch. Set `BOOTD_HTTP_BOOT_URL` to the full URL of the EFI binary
firmware should fetch (for example
`http://10.0.0.5/boot/http/shimx64.efi`) to enable it. Leaving it unset
(the default) disables HTTP Boot entirely and leaves the PXE+TFTP path
above completely unaffected - the two are independent, not a
replacement of one by the other.

Not every UEFI implementation supports HTTP Boot, so PXE+TFTP remains
the path every deployment can rely on; enable HTTP Boot as an addition
for machines/firmware that support it, not as a substitute. See
`internal/bootd`'s package doc comment for the option 60 (Vendor Class
Identifier) negotiation this relies on.

**Setting `BOOTD_HTTP_BOOT_URL` does not, by itself, serve anything at
that URL.** bootd only decides which URL to hand out in the DHCP reply;
nothing in this kustomization currently serves the EFI binary over
HTTP. Point it at an HTTP endpoint you have separately stood up to
serve `shimx64.efi` (or extend `internal/bootserver`'s existing
artifact-serving path to also serve it) before relying on this in
production.

## MAC gating: fail-secure by default

bootd only answers a client whose MAC address matches an enrolled
Machine's `spec.bootMACAddress` - it never net-boots an unrecognized
device on the segment unless `BOOTD_ANSWER_ALL=true` is set (off by
default). The enrolled-MAC set is a locally cached watch of Machine
objects (`internal/bootd.MACCache`), not a per-packet API call, since
bootd runs per-site and a proxyDHCP responder that hit the API server
once per broadcast would be both slow and unwanted load. That cache
fails secure: it answers nothing at all until its first sync completes,
and permanently denies everything if the sync never completes (API
server unreachable at startup) - restart bootd once connectivity is
restored. See `internal/bootd/maccache.go`'s doc comment for the full
reasoning.

## Privileged ports and Pod Security Admission

Ports 67 (proxyDHCP) and 69 (TFTP) are privileged (<1024). Rather than
run as root - which restricted Pod Security Admission forbids outright
- `deployment.yaml` keeps the container `runAsNonRoot` (matching every
other kezio image) and adds only the `NET_BIND_SERVICE` capability,
which restricted PSA still permits a container to add (every other
capability is dropped). No other privilege is granted: no
`hostNetwork`, no `hostPort`, no elevated capability beyond that one.

## RBAC scope

`rbac.yaml` grants `bootd`'s ServiceAccount a **ClusterRole** (`get`,
`list`, `watch` on `machines`), not a namespaced Role: bootd's MAC cache
watches every Machine in the cluster, since a site's bootd has no
reliable way to know in advance which namespace(s) the Machines it
should net-boot live in. If every Machine at a site is known to live in
a single namespace, narrow this to a namespaced Role + RoleBinding in an
overlay.
