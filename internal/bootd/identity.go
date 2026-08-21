/*
Copyright 2026.

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

// Package bootd holds identity constants that internal/controller's bootd
// Deployment builder and the bootd daemon itself must agree on. It carries
// no daemon logic yet - only the shapes a later port of bootd (the
// proxyDHCP/TFTP process the Deployment runs) must keep matching:
// filenames the TFTP server serves, the capabilities its dnsmasq child
// needs, and the ports it listens on.
package bootd

// ShimFilename and GrubFilename are the on-disk names the bootd
// container's TFTP server serves, and the two files the
// fetch-boot-artifacts initContainer copies out of the boot-artifacts
// image into the pod's shared tftp directory.
const (
	ShimFilename = "shimx64.efi"
	GrubFilename = "grubx64.efi"
)

// DefaultProxyPort is the TCP port bootd's HTTP reverse proxy (fronting
// internal/bootserver and internal/agentserver) listens on.
const DefaultProxyPort = 80

// DefaultHealthProbePort is the port bootd's health/readiness endpoints
// bind to - the same port the bootd container's readinessProbe targets.
const DefaultHealthProbePort = 8081

// DnsmasqCapabilities are the Linux capabilities dnsmasq requires to
// serve DHCP (checked explicitly at its own startup):
//
//   - NET_BIND_SERVICE: binding UDP ports 67 and 4011.
//   - NET_ADMIN: interface enumeration/config queries its DHCP engine
//     performs.
//   - NET_RAW: the raw socket it answers not-yet-addressed clients
//     through.
var DnsmasqCapabilities = []string{"NET_BIND_SERVICE", "NET_ADMIN", "NET_RAW"}
