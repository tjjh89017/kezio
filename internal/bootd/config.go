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

package bootd

import "net"

// httpScheme is the URL scheme checked against wherever this package
// validates a firmware- or GRUB-facing URL (render.go, grubpath.go,
// proxy.go) - one spelling shared across those checks rather than a
// hand-typed copy in each.
const httpScheme = "http"

// DefaultBootFilename is the PXE boot filename the rendered pxe-service
// hands out by default: shim, chainloading into GRUB, which then
// fetches its real config from the boot config server
// (internal/bootserver) over HTTP. bootd never hands out grubx64.efi
// directly as the PXE boot filename - shim is what firmware loads
// first.
const DefaultBootFilename = ShimFilename

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
	// (unless NextServerIP overrides it). Required - there is no
	// meaningful default for an address that names this host.
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

	// TFTPDir is the local filesystem directory the in-process TFTP
	// server (see tftp.go) serves ShimFilename and GrubFilename from.
	TFTPDir string

	// HTTPBootURL is the absolute URL handed to a UEFI HTTP Boot client
	// (DHCP client architecture 16) in place of the TFTP filename plus
	// next-server a PXE client receives - typically bootd's own reverse
	// proxy in front of internal/bootserver's GET /boot/http/<name>
	// route, e.g. "http://192.0.2.2/boot/http/shimx64.efi". Firmware
	// that fails a PXE exchange walks its BootOrder forward into HTTP
	// Boot and asks again as an HTTP Boot client; it discards an answer
	// naming a bare filename and DISCOVERs again indefinitely, never
	// walking the BootOrder back, so leaving this empty turns one lost
	// PXE exchange into a machine that waits for a power cycle. Empty
	// leaves HTTP Boot clients unanswered.
	HTTPBootURL string

	// AnswerAll disables the MAC gate (dhcp-ignore is omitted, tag:kezio
	// guard dropped), answering every PXE client regardless of
	// enrollment. Off by default; trades the fail-secure default for a
	// site that deliberately wants to net-boot every unknown machine
	// (e.g. an inventory-only lab).
	AnswerAll bool

	// LeaseMode switches dnsmasq from proxyDHCP to full DHCP leases -
	// the segment's own DHCP authority, for a segment with no other
	// DHCP server. Off by default, since lease assignment normally
	// belongs to a site's existing server. The MAC gate is unchanged
	// (still enrolled-MAC only).
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
