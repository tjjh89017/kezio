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

package bootd

import "net"

// DefaultBootFilename is the PXE boot filename the rendered pxe-service
// hands out by default: shim, chainloading into GRUB, which then
// fetches its real config from the boot config server
// (internal/bootserver) over HTTP. bootd never hands out grubx64.efi
// directly as the PXE boot filename - shim is what firmware loads
// first.
const DefaultBootFilename = "shimx64.efi"

// Config configures the dnsmasq instance bootd renders and supervises
// (see RenderDnsmasqConf / Dnsmasq) and the in-process TFTP server.
type Config struct {
	// Interface optionally names the network interface dnsmasq binds
	// its DHCP sockets to exclusively (bind-interfaces +
	// interface=<name>) - the pod's provisioning-network attachment,
	// for example "net1". Empty means dnsmasq listens on every
	// interface, appropriate only when the process's network namespace
	// carries nothing but the provisioning network.
	Interface string

	// ServerIP is bootd's own IPv4 address on the provisioning
	// network: the PXE boot-server address advertised in pxe-service
	// (unless NextServerIP overrides it) and the local relay address
	// when RelayServerIP is set. Required - there is no meaningful
	// default for an address that names this host.
	ServerIP net.IP

	// NextServerIP overrides the PXE boot-server address advertised to
	// clients, when the TFTP service is reachable at a different
	// address than bootd's own (for example, a Service or virtual IP
	// fronting this pod). Empty means ServerIP.
	NextServerIP net.IP

	// ProvisioningNet is the provisioning network's IPv4 subnet,
	// rendered as the proxyDHCP dhcp-range (network address +
	// netmask). Required: dnsmasq only answers proxyDHCP for subnets
	// it has a matching dhcp-range for.
	ProvisioningNet *net.IPNet

	// BootFilename is the PXE boot filename handed out to a gated
	// client. Empty means DefaultBootFilename.
	BootFilename string

	// RelayServerIP optionally enables DHCP relaying: every DHCP
	// request heard on the provisioning network is also relayed
	// (dhcp-relay) to this existing site DHCP server, which then owns
	// IP lease assignment for the segment. Empty (the default) means
	// proxyDHCP only - bootd never touches lease traffic and the
	// site's DHCP server must be reachable on the segment by other
	// means (its own presence or an external relay).
	RelayServerIP net.IP

	// TFTPDir is the local filesystem directory the in-process TFTP
	// server (see tftp.go) serves shimx64.efi and grubx64.efi from.
	TFTPDir string

	// AnswerAll disables the MAC gate: the rendered pxe-service (or, in
	// LeaseMode, dhcp-boot) drops its tag:kezio guard and the
	// dhcp-ignore line is omitted, so every PXE client on the segment
	// is answered regardless of whether its MAC matches an enrolled
	// Machine. This is off by default; turning it on trades the
	// fail-secure default for answering unconditionally, appropriate
	// only for a site that deliberately wants bootd to net-boot every
	// unknown machine on the segment (for example, an inventory-only
	// lab).
	AnswerAll bool

	// LeaseMode switches the rendered dnsmasq instance from proxyDHCP
	// to serving full DHCP leases: the segment's own DHCP authority,
	// for a provisioning segment that has no other DHCP server at all.
	// Off by default, matching the assumption everywhere else in this
	// package that lease assignment belongs to a site's existing DHCP
	// server, reached either directly or via RelayServerIP.
	//
	// The MAC gate does not change: dhcp-hostsfile/dhcp-ignore still
	// answer only enrolled MACs, exactly as in proxy mode. A site that
	// also needs to hand out addresses to unenrolled devices on the
	// same segment runs its own DHCP server for them and points bootd
	// at it with RelayServerIP instead - kezio never becomes the
	// segment's general-purpose DHCP authority. Mutually exclusive
	// with RelayServerIP: relaying to an existing lease server makes
	// no sense once bootd's own dnsmasq is the lease server.
	LeaseMode bool

	// LeaseRangeStart and LeaseRangeEnd optionally bound the dhcp-range
	// LeaseMode renders. Leaving both empty (the default) auto-derives
	// the range from ProvisioningNet: the subnet's first and last host
	// addresses, e.g. 192.0.2.1-192.0.2.254 for 192.0.2.0/24. Setting
	// only one of the two is an error. Ignored unless LeaseMode is set.
	LeaseRangeStart net.IP
	LeaseRangeEnd   net.IP
}

// withDefaults returns a copy of c with every zero-valued optional
// field filled in.
func (c Config) withDefaults() Config {
	if c.BootFilename == "" {
		c.BootFilename = DefaultBootFilename
	}
	// NextServerIP deliberately has no default here: RenderDnsmasqConf
	// falls back to ServerIP, keeping "explicitly overridden" and
	// "unset" distinguishable.
	return c
}
