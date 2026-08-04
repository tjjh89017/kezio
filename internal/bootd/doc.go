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
package bootd
