/*
Copyright 2026 Date Huang.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package bootd implements kezio-bootd: the proxyDHCP and TFTP services
// a UEFI firmware talks to at the very start of a network boot, before
// grub or the boot config server (internal/bootserver) ever come into
// the picture.
//
// The DHCP side is dnsmasq, not a homegrown responder: bootd renders a
// dnsmasq configuration (render.go), supervises dnsmasq as a foreground
// child process (dnsmasq.go), and feeds it a MAC allowlist derived from
// enrolled Machine objects (maccache.go). dnsmasq brings the
// battle-tested parts - the proxyDHCP exchange on ports 67/4011, PXE
// option handling across firmware quirks, and optional DHCP relaying -
// while bootd keeps the parts that are kezio's own:
//
//   - Config rendering (RenderDnsmasqConf): one proxyDHCP dhcp-range
//     for the provisioning subnet, a tag-gated pxe-service handing out
//     shimx64.efi, and dhcp-ignore so unknown MACs get nothing at all.
//     By default bootd is not a DHCP (lease) server: the site's
//     production DHCP server keeps sole ownership of IP assignment.
//     When Config.RelayServerIP is set, the same dnsmasq instance also
//     relays lease traffic to that server (dhcp-relay) - useful when
//     the provisioning segment has no DHCP server of its own; unset,
//     bootd never touches lease traffic. Config.LeaseMode is the third
//     option, for a segment that has no DHCP server at all and cannot
//     get one added: bootd's dnsmasq becomes the segment's own lease
//     server, still MAC-gated exactly as in proxy mode.
//   - The MAC gate (MACCache): a controller-runtime informer over
//     Machine objects maintains the set of enrolled boot MACs and
//     pushes every change into the dnsmasq dhcp-hostsfile
//     (Dnsmasq.SetAllowedMACs), followed by a SIGHUP - dnsmasq re-reads
//     hostsfiles on SIGHUP without restarting, so enrolling or deleting
//     a Machine takes effect in-place. Fail-secure: the hostsfile
//     starts empty and stays empty until the informer's first sync
//     completes, and no snapshot is ever pushed if the sync fails - an
//     unreachable API server means nothing boots, not "everything
//     boots".
//   - Process supervision (Dnsmasq): dnsmasq runs with --no-daemon as
//     a direct child, its log lines (including per-request log-dhcp
//     decisions) forwarded into bootd's logger, restarted with backoff
//     on crash, and escalated to a fatal error (crashing the pod,
//     visibly) when it cannot stay up at all.
//
// TFTP (tftp.go) stays in-process rather than delegated to dnsmasq's
// enable-tftp: it is a strictly read-only server allowlisting exactly
// shimx64.efi, grubx64.efi, and the in-memory grub/grub.cfg bootstrap
// config (GrubConfigPath) and rejecting any other name, including path
// traversal - dnsmasq's TFTP would serve anything under its tftp-root
// and add a second code path to the same directory without removing
// any Go code worth removing.
//
// UEFI HTTP Boot (option 60 "HTTPClient") is not supported: dnsmasq's
// proxyDHCP engine only engages for PXEClient requests, so an
// HTTPClient request receives no proxy answer (lab-verified). Sites
// needing HTTP Boot must configure their production DHCP server to
// hand out the boot URL; internal/bootserver's GET /boot/http/<name>
// route still serves the artifacts themselves.
//
// Capabilities: dnsmasq refuses to serve DHCP without CAP_NET_ADMIN
// and CAP_NET_RAW (checked explicitly at its startup), plus
// CAP_NET_BIND_SERVICE for ports 67/4011. The pod runs bootd as uid 0
// with exactly those three capabilities (all others dropped) because
// Kubernetes grants added capabilities to a non-root container's
// bounding set only, where execve cannot recover them - see caps.go
// and config/bootd's deployment for the full contract, including why
// dnsmasq runs with --user=root (dnsmasq.go's runChild).
package bootd
