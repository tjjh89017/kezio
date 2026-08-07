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

// Package bootd implements kezio-bootd: the proxyDHCP and TFTP services a
// UEFI firmware talks to at the very start of a network boot, before grub
// or the boot config server (internal/bootserver) come into play.
//
// DHCP is dnsmasq (render.go builds its config, dnsmasq.go supervises it
// as a child, maccache.go feeds it a MAC allowlist from enrolled Machine
// objects), not a homegrown responder.
//
//   - Config rendering: by default bootd is proxyDHCP only and never
//     touches lease traffic - the site's DHCP server keeps sole ownership
//     of IP assignment. Config.RelayServerIP additionally relays lease
//     traffic to an existing site DHCP server (dhcp-relay), for a
//     provisioning segment with no DHCP server of its own. Config.LeaseMode
//     instead makes bootd's dnsmasq the segment's own lease server (for a
//     segment with none at all) - still MAC-gated exactly as in proxy mode;
//     mutually exclusive with RelayServerIP.
//   - MAC gate (MACCache): an informer over Machine objects pushes the
//     enrolled-MAC set into dnsmasq's dhcp-hostsfile then SIGHUPs it
//     in-place. Fail-secure: the hostsfile starts and stays empty until the
//     informer's first sync succeeds - an unreachable API server means
//     nothing boots, not everything boots.
//   - Process supervision (Dnsmasq): runs with --no-daemon, restarted with
//     backoff on crash, escalated to a fatal (pod-crashing) error if it
//     can't stay up.
//
// TFTP (tftp.go) stays in-process rather than dnsmasq's enable-tftp: a
// strictly read-only server allowlisting exactly shimx64.efi, grubx64.efi,
// and the in-memory grub.cfg (GrubConfigPath), rejecting any other name
// including path traversal - dnsmasq's TFTP would serve anything under its
// tftp-root.
//
// UEFI HTTP Boot (DHCP option 60 "HTTPClient") is not supported: dnsmasq's
// proxyDHCP engine only engages for PXEClient requests (lab-verified), so
// sites needing HTTP Boot must configure their production DHCP server to
// hand out the boot URL; internal/bootserver's GET /boot/http/<name> still
// serves the artifacts.
//
// Capabilities: dnsmasq requires CAP_NET_ADMIN, CAP_NET_RAW, and
// CAP_NET_BIND_SERVICE (checked at its own startup). The pod runs bootd as
// uid 0 with exactly those three because Kubernetes grants added
// capabilities to a non-root container's bounding set only, which execve
// cannot recover - see caps.go and config/bootd's deployment.
package bootd
