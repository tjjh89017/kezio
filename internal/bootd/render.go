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

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// kezioTag is the dnsmasq tag the MAC gate pivots on: the hostsfile
// sets it per allowed MAC (see RenderHostsfile) and the rendered config
// only hands out PXE boot info to requests carrying it.
const kezioTag = "kezio"

// httpClientTag is the dnsmasq tag the UEFI HTTP Boot answer pivots on,
// set by a dhcp-match on client architecture archX64UEFIHTTP.
//
// Only architecture 16 is matched. The IANA processor architecture list
// (RFC 4578 and its updates) gives 15 to x86 UEFI HTTP and 16 to x64 UEFI
// HTTP; a machine sending 15 is 32-bit firmware, which cannot load the
// x86-64 shim this hands out, so answering it would trade a boot loop for
// a load failure.
const (
	httpClientTag   = "httpclient"
	archX64UEFIHTTP = 16
)

// httpBootVendorClass is the DHCP option 60 value UEFI firmware requires
// an HTTP Boot answer to carry: EDK2 classifies an offer as an HTTP Boot
// offer only when option 60 starts with "HTTPClient"
// (HttpBootParseDhcp4Packet, NetworkPkg/HttpBootDxe), and treats anything
// else - even one carrying a perfectly good URL - as a plain DHCP offer
// with no boot method, which is where the DISCOVER loop comes from. It is
// also the vendor class the client itself sends, as
// "HTTPClient:Arch:00016:UNDI:003001".
const httpBootVendorClass = "HTTPClient"

// pxeVendorClass is dnsmasq's own default dhcp-pxe-vendor value. Naming
// it explicitly matters because setting dhcp-pxe-vendor at all replaces
// that default: dropping PXEClient from the list would stop dnsmasq's
// proxyDHCP engine answering ordinary PXE clients.
const pxeVendorClass = "PXEClient"

// HostsfileName and leasefileName are the file names RenderDnsmasqConf
// points dnsmasq at inside the run directory. The hostsfile is the one
// file rewritten at runtime (see Dnsmasq.SetAllowedMACs): dnsmasq
// re-reads dhcp-hostsfile on SIGHUP but never the main config, so
// everything dynamic must live here and everything else in the config
// is fixed for the process's lifetime.
const (
	HostsfileName = "dhcp-hosts.conf"
	leasefileName = "dnsmasq.leases"
)

// HostsfilePath returns the path of the dhcp-hostsfile inside runDir.
func HostsfilePath(runDir string) string {
	return filepath.Join(runDir, HostsfileName)
}

// RenderDnsmasqConf renders the complete dnsmasq configuration for cfg,
// with dnsmasq's writable files (hostsfile, leasefile) placed under
// runDir. It is a pure function: no file touched, no process consulted.
//
// Default is proxyDHCP (dhcp-range ...,proxy): never assigns addresses,
// only answers the PXE portion on ports 67/4011; lease assignment stays
// with the site's own DHCP server.
//
// Config.LeaseMode switches to a full lease-serving dhcp-range instead.
// pxe-service requires proxyDHCP (fails UEFI secure netboot otherwise),
// so lease mode hands out the boot file via dhcp-boot, gated by client
// architecture (DHCP option 93, dhcp-match=...,option:client-arch,N).
// Everything else is unchanged between the two modes.
//
// MAC gate via dnsmasq tags: dhcp-hostsfile sets tag:kezio on enrolled
// MACs, pxe-service matches only tag:kezio, dhcp-ignore=tag:!kezio drops
// everything else outright. Lab-verified: dhcp-ignore suppresses the
// content-free proxy OFFER dnsmasq would otherwise still broadcast to
// unknown MACs, on both ports 67 and 4011.
//
// pxe-service names x86-64_EFI as the client architecture: lab-verified
// as the spelling that makes dnsmasq answer an arch-7 (RFC 4578 EFI
// x86-64, what EDK2 sends) client's port-4011 REQUEST with the boot file
// in the BOOTP file field. BC_EFI or numeric 9 instead produce a PXE
// boot menu UEFI firmware does not process.
//
// Config.Gateway carries what the lease says about leaving the segment:
// an address renders the router option (3), the empty string renders
// dnsmasq's suppression form so no default route is handed out at all,
// and nil renders neither - see that field for the three states and for
// which layer rejects the third.
//
// Config.HTTPBootURL adds a second, parallel answer for UEFI HTTP Boot
// clients in both modes, keyed on client architecture 16 - see that
// field's doc comment for the boot loop its absence produces, and the
// two branches below for what each mode needs to make dnsmasq answer.
//
// TFTP stays deliberately absent (no enable-tftp): artifact serving is
// the in-process server (tftp.go), a fixed allowlist rejecting path
// traversal - dnsmasq's TFTP would serve anything under tftp-root.
func RenderDnsmasqConf(cfg Config, runDir string) (string, error) {
	cfg = cfg.withDefaults()

	if cfg.HTTPBootURL != "" {
		if err := validateHTTPBootURL(cfg.HTTPBootURL); err != nil {
			return "", err
		}
	}

	serverIP := cfg.ServerIP.To4()
	if serverIP == nil {
		return "", fmt.Errorf("ServerIP %v is not a valid IPv4 address", cfg.ServerIP)
	}
	if cfg.ProvisioningNet == nil {
		return "", fmt.Errorf("ProvisioningNet is required")
	}
	network := cfg.ProvisioningNet.IP.Mask(cfg.ProvisioningNet.Mask)
	if network.To4() == nil {
		return "", fmt.Errorf("ProvisioningNet %v is not an IPv4 network", cfg.ProvisioningNet)
	}
	mask := net.IP(cfg.ProvisioningNet.Mask)
	if len(mask) != net.IPv4len {
		return "", fmt.Errorf("ProvisioningNet %v does not carry an IPv4 netmask", cfg.ProvisioningNet)
	}
	nextServer := cfg.NextServerIP.To4()
	if nextServer == nil {
		nextServer = serverIP
	}

	var leaseStart, leaseEnd net.IP
	if cfg.LeaseMode {
		var err error
		leaseStart, leaseEnd, err = leaseRange(cfg, network, mask)
		if err != nil {
			return "", err
		}
	}

	var b strings.Builder
	b.WriteString("# Generated by kezio-bootd - do not edit.\n")
	b.WriteString("# dnsmasq re-reads only dhcp-hostsfile on SIGHUP; any change here\n")
	b.WriteString("# requires a dnsmasq restart (bootd restarts it on crash only).\n")
	// port=0 disables dnsmasq's DNS server - bootd wraps dnsmasq for DHCP alone.
	b.WriteString("port=0\n")
	// log-dhcp: per-request decision logging, forwarded into bootd's log
	// stream by the supervisor (Dnsmasq.Start).
	b.WriteString("log-dhcp\n")
	if cfg.Interface != "" {
		b.WriteString("bind-interfaces\n")
		fmt.Fprintf(&b, "interface=%s\n", cfg.Interface)
	}
	fmt.Fprintf(&b, "dhcp-leasefile=%s\n", filepath.Join(runDir, leasefileName))
	if cfg.LeaseMode {
		fmt.Fprintf(&b, "dhcp-range=%s,%s\n", leaseStart, leaseEnd)
	} else {
		fmt.Fprintf(&b, "dhcp-range=%s,proxy,%s\n", network, mask)
	}
	fmt.Fprintf(&b, "dhcp-hostsfile=%s\n", HostsfilePath(runDir))
	if cfg.LeaseMode && cfg.Gateway != nil {
		if *cfg.Gateway == "" {
			// dnsmasq's documented suppression form: the option is
			// declared with no value, so nothing is sent and dnsmasq does
			// not substitute its own address.
			b.WriteString("dhcp-option=3\n")
		} else {
			gateway := net.ParseIP(*cfg.Gateway)
			if gateway == nil || gateway.To4() == nil {
				return "", fmt.Errorf("Config.Gateway %q is not a valid IPv4 address", *cfg.Gateway)
			}
			fmt.Fprintf(&b, "dhcp-option=3,%s\n", gateway.To4())
		}
	}
	if cfg.LeaseMode {
		// dhcp-boot is the non-proxy equivalent of pxe-service: a plain
		// default followed by an architecture-tagged override. Only
		// x86-64 EFI today, so both lines carry the same file - a second
		// architecture is one more dhcp-match/dhcp-boot pair.
		fmt.Fprintf(&b, "dhcp-boot=%s,,%s\n", cfg.BootFilename, nextServer)
		fmt.Fprintf(&b, "dhcp-match=set:efi-x86_64,option:client-arch,7\n")
		fmt.Fprintf(&b, "dhcp-boot=tag:efi-x86_64,%s,,%s\n", cfg.BootFilename, nextServer)
		if cfg.HTTPBootURL != "" {
			fmt.Fprintf(&b, "dhcp-match=set:%s,option:client-arch,%d\n", httpClientTag, archX64UEFIHTTP)
			// Lease mode never takes dnsmasq's PXE paths (no
			// pxe-service means enable_pxe stays off, so pxe_misc never
			// runs), and those are the only paths that put option 60 in
			// a reply on their own. Forced rather than plain
			// dhcp-option, so firmware that leaves 60 out of its
			// parameter request list still receives it.
			fmt.Fprintf(&b, "dhcp-option-force=tag:%s,60,%s\n", httpClientTag, httpBootVendorClass)
			// A tag-matched dhcp-boot outranks the untagged default
			// above (dnsmasq's find_boot tries tagged entries first), so
			// an architecture-16 client gets the URL rather than the
			// bare TFTP filename every other client gets.
			fmt.Fprintf(&b, "dhcp-boot=tag:%s,%s\n", httpClientTag, cfg.HTTPBootURL)
		}
	} else if cfg.AnswerAll {
		// No tag guard: every PXE client is answered - see
		// Config.AnswerAll for when that trade is acceptable.
		fmt.Fprintf(&b, "pxe-service=x86-64_EFI,\"kezio network boot\",%s,%s\n", cfg.BootFilename, nextServer)
	} else {
		fmt.Fprintf(&b, "pxe-service=tag:%s,x86-64_EFI,\"kezio network boot\",%s,%s\n", kezioTag, cfg.BootFilename, nextServer)
	}
	if !cfg.LeaseMode && cfg.HTTPBootURL != "" {
		// dnsmasq's proxyDHCP engine only engages for a client whose
		// vendor class is one of dhcp-pxe-vendor's (is_pxe_client), and
		// that list defaults to PXEClient alone - which is why an HTTP
		// Boot client used to receive nothing here at all. The matched
		// vendor is what dnsmasq echoes back as the offer's option 60
		// (pxe_misc), so an HTTPClient client is answered with the
		// option 60 EDK2 requires, and a PXEClient one still with
		// PXEClient; proxy mode therefore needs no dhcp-option-force.
		fmt.Fprintf(&b, "dhcp-pxe-vendor=%s,%s\n", pxeVendorClass, httpBootVendorClass)
		fmt.Fprintf(&b, "dhcp-match=set:%s,option:client-arch,%d\n", httpClientTag, archX64UEFIHTTP)
		// pxe-service cannot express a URL, and only answers the one
		// architecture it names, so an architecture-16 client falls
		// through to dhcp-boot - which dnsmasq's proxy path does consult
		// (find_boot), even though the PXE side of dhcp-boot above is
		// lease mode only. Tagged, so a PXE client finds no dhcp-boot at
		// all and keeps taking the pxe-service path unchanged.
		fmt.Fprintf(&b, "dhcp-boot=tag:%s,%s\n", httpClientTag, cfg.HTTPBootURL)
	}
	if !cfg.AnswerAll {
		// dhcp-ignore is the actual MAC gate in both modes, independent
		// of how the boot file is selected above.
		fmt.Fprintf(&b, "dhcp-ignore=tag:!%s\n", kezioTag)
	}
	// dhcp-no-override: keep the boot file and server name in the
	// BOOTP header fields (file/sname) instead of moving them into
	// options 67/66 - the header fields are what PXE firmware reads.
	b.WriteString("dhcp-no-override\n")
	// dhcp-ignore-clid: identify a client by MAC alone, ignoring any
	// DHCP client-id option (61). A DUT presents up to three different
	// client identities across one deploy - UEFI PXE firmware sends
	// none, the live image's dhcpcd sends an RFC4361 client-id, the
	// installed OS sends its own DUID - and kezio's identity
	// (spec.bootMACAddress, the dhcp-hostsfile entries) is MAC-keyed;
	// in proxy mode this still governs how dnsmasq identifies clients
	// for its proxy replies.
	b.WriteString("dhcp-ignore-clid\n")
	return b.String(), nil
}

// validateHTTPBootURL rejects a Config.HTTPBootURL firmware could not
// fetch. It is checked at render time rather than left to the client:
// dnsmasq copies whatever it is given into the offer's file field, so a
// relative path or a stray tftp:// URL would surface only as the same
// silent DISCOVER loop HTTPBootURL exists to end. https is allowed - UEFI
// HTTPS Boot exists, on firmware carrying the matching certificate -
// though bootd's own proxy in front of internal/bootserver serves plain
// http.
func validateHTTPBootURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("HTTPBootURL %q is not a valid URL: %w", rawURL, err)
	}
	if u.Scheme != httpScheme && u.Scheme != "https" {
		return fmt.Errorf("HTTPBootURL %q must be an http or https URL, firmware fetches nothing else", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("HTTPBootURL %q names no host", rawURL)
	}
	return nil
}

// leaseRange resolves the dhcp-range bounds LeaseMode renders: explicit
// Config.LeaseRangeStart/End if both set, else network+1 through
// broadcast-1. network and mask are already-validated, already-masked
// values from cfg.ProvisioningNet.
func leaseRange(cfg Config, network, mask net.IP) (start, end net.IP, err error) {
	startSet, endSet := cfg.LeaseRangeStart != nil, cfg.LeaseRangeEnd != nil
	if startSet != endSet {
		return nil, nil, fmt.Errorf("LeaseRangeStart and LeaseRangeEnd must both be set, or both left empty for auto-derivation")
	}
	if startSet {
		start, end = cfg.LeaseRangeStart.To4(), cfg.LeaseRangeEnd.To4()
		if start == nil {
			return nil, nil, fmt.Errorf("LeaseRangeStart %v is not a valid IPv4 address", cfg.LeaseRangeStart)
		}
		if end == nil {
			return nil, nil, fmt.Errorf("LeaseRangeEnd %v is not a valid IPv4 address", cfg.LeaseRangeEnd)
		}
		return start, end, nil
	}

	n := binary.BigEndian.Uint32(network.To4())
	m := binary.BigEndian.Uint32(mask.To4())
	usableHosts := ^m - 1 // network and broadcast addresses excluded
	if usableHosts < 1 {
		return nil, nil, fmt.Errorf("ProvisioningNet %s/%d has no usable host addresses for an auto-derived lease range; set LeaseRangeStart/LeaseRangeEnd explicitly", network, prefixLen(mask))
	}
	start = intToIPv4(n + 1)
	end = intToIPv4((n | ^m) - 1)
	return start, end, nil
}

// intToIPv4 renders a big-endian uint32 host address as a 4-byte
// net.IP.
func intToIPv4(v uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

// prefixLen reports the prefix length of an already-validated IPv4
// netmask, for the auto-derivation error message only.
func prefixLen(mask net.IP) int {
	ones, _ := net.IPMask(mask.To4()).Size()
	return ones
}

// RenderGrubConfig renders the GRUB bootstrap config TFTPServer serves at
// GrubConfigPath. Deliberately minimal: one configfile line handing
// control to the boot config server (internal/bootserver) at
// bootConfigURL, whose per-MAC /boot/grub.cfg-<mac> response carries all
// actual boot logic - duplicating any of it here would create a second
// source of truth. ${net_default_mac} is a GRUB variable: the booting
// NIC's MAC, the same identity bootserver's MAC gate resolves. Uses
// GRUB's network file syntax (GrubNetPath - GRUB cannot open bare URLs),
// so an unfetchable bootConfigURL is an error here rather than a silent
// boot-time dead end.
func RenderGrubConfig(bootConfigURL string) (string, error) {
	configPath, err := GrubNetPath(bootConfigURL, "/boot/grub.cfg-${net_default_mac}")
	if err != nil {
		return "", fmt.Errorf("rendering %s: %w", GrubConfigPath, err)
	}
	return fmt.Sprintf(`# Generated by kezio-bootd - do not edit.
configfile %s
`, configPath), nil
}

// RenderHostsfile renders the dhcp-hostsfile contents for the given
// allowed MACs: one "<mac>,set:kezio" line per MAC, sorted and
// deduplicated so identical MAC sets always render byte-identical
// (letting the supervisor skip a rewrite-plus-SIGHUP when nothing
// changed). An empty or nil macs renders an empty file - the
// fail-secure state in which no machine receives boot info.
//
// reservations optionally maps a normalized MAC to the fixed IPv4
// address a lease-mode Subnet's DHCP reservation table holds for it
// (SubnetDHCPCache derives it from Subnet status.dhcp.reservations): a
// MAC found there renders "<mac>,set:kezio,<address>" instead, which
// dnsmasq's dhcp-hostsfile syntax reads as a pinned lease for that host.
// A reservation naming a MAC absent from macs is silently dropped - the
// MAC gate is macs alone, so a reservation can never be a second way to
// admit a MAC that is not enrolled.
func RenderHostsfile(macs []string, reservations map[string]string) string {
	if len(macs) == 0 {
		return ""
	}
	sorted := slices.Clone(macs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var b strings.Builder
	for _, mac := range sorted {
		if addr, ok := reservations[mac]; ok && addr != "" {
			fmt.Fprintf(&b, "%s,set:%s,%s\n", mac, kezioTag, addr)
		} else {
			fmt.Fprintf(&b, "%s,set:%s\n", mac, kezioTag)
		}
	}
	return b.String()
}
