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

// DefaultDHCPAddr and DefaultPXEAddr are the standard proxyDHCP and PXE
// boot-server ports (see the package doc comment). Neither is
// overridable to something other than the well-known port in practice -
// firmware does not let an operator configure which port it PXE-boots
// against - but Config still exposes them as fields (rather than
// hard-coded literals in server.go) so tests can bind ephemeral ports
// instead of fighting over 67/4011 on the machine running them.
const (
	DefaultDHCPAddr = ":67"
	DefaultPXEAddr  = ":4011"
)

// DefaultBootFilename is the second-stage loader every gate in this
// package ultimately hands out: shim, chainloading into GRUB, which then
// fetches its real config from the boot config server
// (internal/bootserver) over HTTP. bootd never hands out grubx64.efi
// directly as the PXE boot filename - shim is what firmware loads first.
const DefaultBootFilename = "shimx64.efi"

// Config configures a Server and its TFTP counterpart.
type Config struct {
	// DHCPAddr is the local address the proxyDHCP listener binds, for
	// example ":67". Empty means DefaultDHCPAddr.
	DHCPAddr string
	// PXEAddr is the local address the PXE boot-server listener binds,
	// for example ":4011". Empty means DefaultPXEAddr.
	PXEAddr string

	// ServerIP is bootd's own IP address on the boot network: the
	// fallback for both the DHCP Server Identifier option and, when
	// NextServerIP is unset, the next-server (siaddr) firmware TFTPs
	// shimx64.efi/grubx64.efi from. It is a fallback rather than the
	// primary source because each reply prefers the IPv4 address of the
	// interface the request actually arrived on (see BuildResponse's
	// ifaceIP parameter) - ServerIP applies when that interface's
	// address is unknown, for example an L2-only attachment that
	// carries no IPv4 address of its own. It must still be set - there
	// is no meaningful default for an address that names this host.
	ServerIP net.IP
	// NextServerIP overrides the next-server (siaddr) advertised to
	// clients, when the TFTP service is reachable at a different
	// address than bootd's own (for example, a Service or virtual IP
	// fronting this pod). Empty means the reply's Server Identifier
	// address (the arrival interface's address, or ServerIP).
	NextServerIP net.IP

	// BootFilename is the PXE boot filename handed out to a gated
	// client. Empty means DefaultBootFilename.
	BootFilename string

	// HTTPBootURL is the full HTTP(S) URL handed out, as the boot
	// filename, to a gated client that advertises the HTTPClient
	// vendor class (UEFI HTTP Boot) instead of PXEClient - for example
	// "http://10.0.0.5/boot/http/shimx64.efi". There is no default:
	// empty means HTTP Boot is disabled entirely, and an HTTPClient
	// request is declined (see OutcomeHTTPBootUnconfigured) rather than
	// answered with a TFTP-relative filename it cannot fetch. Setting
	// it does not change the PXEClient path in any way - both coexist.
	//
	// Nothing in this package serves the artifact at that URL - see
	// internal/bootserver's GET /boot/http/<name> route (its package doc
	// comment's endpoint list), which is what this URL should point at.
	HTTPBootURL string

	// TFTPDir is the local filesystem directory the TFTP server (see
	// tftp.go) serves shimx64.efi and grubx64.efi from.
	TFTPDir string

	// AnswerAll disables the MAC gate (see maccache.go / the package
	// doc comment): every architecture-matching client is answered
	// regardless of whether its MAC matches an enrolled Machine. This
	// is off by default; turning it on trades the fail-secure default
	// for answering unconditionally, appropriate only for a site that
	// deliberately wants bootd to net-boot every unknown machine on the
	// segment (for example, an inventory-only lab).
	AnswerAll bool
}

// withDefaults returns a copy of c with every zero-valued optional
// field filled in.
func (c Config) withDefaults() Config {
	if c.DHCPAddr == "" {
		c.DHCPAddr = DefaultDHCPAddr
	}
	if c.PXEAddr == "" {
		c.PXEAddr = DefaultPXEAddr
	}
	if c.BootFilename == "" {
		c.BootFilename = DefaultBootFilename
	}
	// NextServerIP deliberately has no default here: an unset value
	// means "follow the reply's Server Identifier", which BuildResponse
	// resolves per request against the arrival interface's address -
	// collapsing it to ServerIP up front would erase the distinction
	// between "explicitly overridden" and "unset".
	return c
}
