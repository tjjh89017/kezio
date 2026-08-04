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
//   - GET /boot/grub.cfg-<mac>: GRUB's own config fetch. It resolves the
//     requesting NIC's MAC address to a Machine, decides whether that
//     machine currently needs to load the live boot environment or boot
//     its local disk, and for the former mints a fresh single-use token
//     embedded in the kernel cmdline.
//   - GET /boot/artifacts/...: serves the live kernel, initramfs, and
//     squashfs GRUB's config points at, from a directory mounted into the
//     manager container out of band.
//
// Both endpoints are unauthenticated by design: a UEFI firmware or a
// GRUB instance has no credential to present, and the whole point of a
// network boot flow is that a machine has nothing installed yet. Every
// design choice in this package follows from that: the grub.cfg
// responses read from Kubernetes are visible to anything that can reach
// this port (any device on the same L2 segment, in the ordinary
// deployment), and a request is never trusted to be an operator-owned
// machine just because it presents that machine's MAC address - the MAC
// only selects which config to hand back, and the config it gets never
// contains anything more sensitive than a token whose only power is to
// register once as that one machine, for a short and bounded window,
// consuming it in the process. See Server's doc comment and the
// STRIDE analysis in its package tests for the full threat model.
//
// Server implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable,
// so it runs embedded in the same manager process as the Machine
// reconciler: it reads Machine state straight from the manager's cache
// (through a field indexer on spec.bootMACAddress) instead of needing its
// own client or a second round trip through the API server.
package bootserver
