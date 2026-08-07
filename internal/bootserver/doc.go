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

// Package bootserver implements the HTTP endpoints a network-booting
// firmware and its GRUB loader talk to before an agent ever runs:
//
//   - GET /boot/grub.cfg-<mac>: GRUB's own config fetch. Resolves the
//     requesting NIC's MAC to a Machine, decides whether it needs the
//     live boot environment or its local disk, and for the former mints
//     a fresh single-use token embedded in the kernel cmdline.
//   - GET /boot/artifacts/...: serves the live kernel, initramfs, and
//     squashfs from a directory mounted into the manager container out
//     of band.
//   - GET /boot/http/<name>: serves the signed shim/grub EFI binaries
//     (the same allowlist internal/bootd's TFTP server serves) for UEFI
//     HTTP Boot, an alternative to PXE+TFTP some firmware supports;
//     PXE+TFTP firmware never touches this route.
//
// All three endpoints are unauthenticated by design: firmware/GRUB have
// no credential to present, and a machine with nothing installed yet has
// none to give. A request is never trusted to be an operator-owned
// machine just because it presents that machine's MAC - the MAC only
// selects which config to hand back, and the config never contains
// anything more sensitive than a token whose only power is to register
// once as that one machine, for a short bounded window, consumed in the
// process. See Server's doc comment and the STRIDE analysis in its
// package tests for the full threat model.
//
// Server implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable,
// so it runs embedded in the same manager process as the Machine
// reconciler: it reads Machine state straight from the manager's cache
// (through a field indexer on spec.bootMACAddress) instead of needing its
// own client or a second round trip through the API server.
package bootserver
