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
// UEFI firmware talks to at the very start of a network boot, before
// grub or the boot config server (internal/bootserver) ever come into
// the picture.
//
// bootd is deliberately not a DHCP server: the site's production DHCP
// server keeps sole ownership of IP lease assignment. bootd only ever
// answers the PXE portion of the exchange - "which file do I TFTP, and
// from which next-server" - as defined by the Preboot Execution
// Environment (PXE) Specification and RFC 4578 (DHCP options for PXE).
// Concretely:
//
//   - UDP/67 (proxyDHCPServer in the PXE spec's terms): listens
//     alongside the production DHCP server for DHCPDISCOVER broadcasts
//     that carry PXE options (option 60 "PXEClient", option 93 client
//     system architecture). It answers with a DHCPOFFER-shaped packet
//     that carries no yiaddr (it never leases an address) but does carry
//     the next-server and boot filename the firmware needs.
//   - UDP/4011 (the PXE "boot server" port): handles the second-phase
//     DHCPREQUEST a PXE client sends once it already has a real lease
//     from production DHCP but still needs the boot server's file list.
//
// Both listeners share one packet-handling core (BuildResponse in
// dhcp.go), which is a pure function from a parsed request to an
// optional response: it opens no socket and blocks on nothing, so the
// PXE/DHCP option logic is unit-testable without a network namespace.
// server.go is the thin socket loop around that core.
//
// Two gates apply before bootd ever answers a discovering client:
//
//   - Client System Architecture (option 93) must name x86-64 UEFI
//     (arch 7, EFI_X86_64) or EFI Byte Code (arch 9, EFI_BC) - every
//     other architecture is logged and ignored, since this deployment
//     only ships shimx64.efi/grubx64.efi.
//   - The requesting MAC address must match an enrolled Machine's
//     spec.bootMACAddress, unless MACGate is configured to answer all
//     (AnswerAllMode - default off). See maccache.go: bootd runs
//     per-site, outside the manager, so this is a locally cached watch
//     of Machine objects, not a per-packet API call. The cache
//     fail-secure defaults to "answer nothing" until its first sync
//     completes and for as long as the watch cannot reach the API
//     server - a stale "yes" that boots a machine which was since
//     removed is a worse failure mode than a delayed "no".
//
// TFTP (tftp.go) is a strictly read-only, two-file server: it serves
// exactly shimx64.efi and grubx64.efi from a configured directory and
// accepts no writes and no other filename, including any path-traversal
// attempt - see ReadHandler's doc comment.
//
// # UEFI HTTP Boot (alternative to PXE+TFTP)
//
// Some UEFI firmware supports fetching its boot loader directly over
// HTTP(S) instead of PXE+TFTP: it advertises option 60 as "HTTPClient"
// (rather than "PXEClient", optionally suffixed per the UEFI spec, for
// example "HTTPClient:Arch:00016") and expects the DHCP reply's boot
// filename to be a full HTTP(S) URL, with option 60 echoing "HTTPClient"
// back - firmware requires that exact echo to accept the offer at all.
// BuildResponse recognizes this alongside PXEClient (see its doc
// comment); it is enabled per-deployment by setting Config.HTTPBootURL,
// and is otherwise off - an unset HTTPBootURL leaves the PXEClient path
// completely unaffected, and any HTTPClient request is declined rather
// than answered with a TFTP filename that firmware asking for HTTP Boot
// cannot use.
//
// Not every UEFI implementation supports HTTP Boot, and it generally
// requires a routed L3 network to be worth enabling (PXE+TFTP works on
// a flat L2 segment without any routing consideration); PXE+TFTP remains
// the default and the one every deployment can rely on. Where HTTP Boot
// is enabled, this package still only decides *which URL to hand out* -
// it does not itself serve the EFI binary at that URL. Wire
// Config.HTTPBootURL at whatever HTTP endpoint actually serves
// shimx64.efi (or the deployment's HTTP-Boot-specific artifact); as of
// this package, no such endpoint exists yet, so enabling HTTP Boot
// without one first stood up leaves firmware unable to fetch what the
// URL points at even though the DHCP exchange itself succeeds.
package bootd
