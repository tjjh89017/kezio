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

import (
	"encoding/binary"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tjjh89017/kezio/internal/bootserver"
)

// kezioTag is the dnsmasq tag the MAC gate pivots on: the hostsfile
// sets it per allowed MAC (see RenderHostsfile) and the rendered config
// only hands out PXE boot info to requests carrying it.
const kezioTag = "kezio"

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
// runDir. It is a pure function: no file is touched and no process
// consulted, so every rendered line is unit-testable.
//
// The rendered instance is a proxyDHCP server by default (dhcp-range
// ...,proxy): it never assigns addresses, it only answers the PXE
// portion of the exchange on ports 67 and 4011. Lease assignment
// stays with the site's own DHCP server, optionally reached via
// dhcp-relay (see Config.RelayServerIP).
//
// Config.LeaseMode switches to a full lease-serving dhcp-range
// instead, for a segment with no DHCP server of its own. Proxy mode's
// pxe-service does not work once dnsmasq stops being a proxy - it
// fails to support UEFI secure netboot outside proxyDHCP - so lease
// mode hands out the boot file through dhcp-boot instead, gated by
// the client architecture dnsmasq reads from DHCP option 93
// (dhcp-match=...,option:client-arch,N). Everything else (port=0,
// bind-interfaces/interface, log-dhcp, dhcp-no-override, leasefile,
// hostsfile, and the MAC gate below) is unchanged between the two
// modes.
//
// The MAC gate works through dnsmasq tags: dhcp-hostsfile sets
// tag:kezio on enrolled MACs, pxe-service only matches tag:kezio, and
// dhcp-ignore=tag:!kezio drops every other request entirely. That last
// line is what suppresses the content-free proxy OFFER dnsmasq would
// otherwise still broadcast to unknown MACs (an offer carrying no
// boot file, but enough for a strange firmware to latch onto);
// lab-verified: with dhcp-ignore in place a denied MAC receives
// nothing from bootd on either port 67 or 4011, while dhcp-relay
// forwarding for that same MAC keeps working - dnsmasq relays before
// its local DHCP engine evaluates dhcp-ignore, so relayed lease
// OFFERs still reach denied clients unharmed.
//
// pxe-service names x86-64_EFI as the client system architecture:
// lab-verified as the spelling that makes dnsmasq answer an arch-7
// (RFC 4578 EFI x86-64, what EDK2 firmware sends) client's port-4011
// REQUEST with the boot file in the BOOTP file field, via dnsmasq's
// UEFI PXE workaround. BC_EFI and a numeric 9 instead produce a PXE
// boot menu UEFI firmware does not process.
//
// TFTP stays deliberately absent (no enable-tftp): artifact serving
// remains the in-process pin/tftp server (tftp.go), which serves a
// fixed allowlist and rejects path traversal - dnsmasq's TFTP
// would serve anything under tftp-root and add nothing but a second
// code path to the same directory.
func RenderDnsmasqConf(cfg Config, runDir string) (string, error) {
	cfg = cfg.withDefaults()

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
	if cfg.LeaseMode && cfg.RelayServerIP != nil {
		return "", fmt.Errorf("LeaseMode and RelayServerIP are mutually exclusive: " +
			"bootd cannot both serve leases itself and relay to another lease server")
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
	// port=0 disables dnsmasq's DNS server entirely - bootd wraps
	// dnsmasq for DHCP alone.
	b.WriteString("port=0\n")
	// log-dhcp: per-request decision logging, forwarded into bootd's
	// own log stream by the supervisor (see Dnsmasq.Start) - the
	// equivalent of the per-packet outcome logging the previous
	// in-process DHCP responder had.
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
	if cfg.LeaseMode {
		// pxe-service is a proxyDHCP-only mechanism; outside proxy
		// mode it fails to support UEFI secure netboot. dhcp-boot is
		// the non-proxy equivalent: a plain default (used until a
		// dhcp-match tags the request) followed by an
		// architecture-tagged override. kezio serves only x86-64 EFI
		// today, so both lines carry the same file - the default line
		// exists so a second architecture (aarch64, client-arch 11)
		// is one more dhcp-match/dhcp-boot pair, not a restructure.
		fmt.Fprintf(&b, "dhcp-boot=%s,,%s\n", cfg.BootFilename, nextServer)
		fmt.Fprintf(&b, "dhcp-match=set:efi-x86_64,option:client-arch,7\n")
		fmt.Fprintf(&b, "dhcp-boot=tag:efi-x86_64,%s,,%s\n", cfg.BootFilename, nextServer)
	} else if cfg.AnswerAll {
		// No tag guard: every PXE client is answered - see
		// Config.AnswerAll for when that trade is acceptable.
		fmt.Fprintf(&b, "pxe-service=x86-64_EFI,\"kezio network boot\",%s,%s\n", cfg.BootFilename, nextServer)
	} else {
		fmt.Fprintf(&b, "pxe-service=tag:%s,x86-64_EFI,\"kezio network boot\",%s,%s\n", kezioTag, cfg.BootFilename, nextServer)
	}
	if !cfg.AnswerAll {
		// dhcp-ignore is the actual MAC gate in both modes: it drops
		// the request outright for anything but an enrolled MAC,
		// independent of how the boot file itself gets selected above
		// - see Config.LeaseMode and this function's package doc
		// comment for why lease mode does not relax it.
		fmt.Fprintf(&b, "dhcp-ignore=tag:!%s\n", kezioTag)
	}
	if cfg.RelayServerIP != nil {
		relayTo := cfg.RelayServerIP.To4()
		if relayTo == nil {
			return "", fmt.Errorf("RelayServerIP %v is not a valid IPv4 address", cfg.RelayServerIP)
		}
		fmt.Fprintf(&b, "dhcp-relay=%s,%s\n", serverIP, relayTo)
	}
	// dhcp-no-override: keep the boot file and server name in the
	// BOOTP header fields (file/sname) instead of moving them into
	// options 67/66 - the header fields are what PXE firmware reads.
	b.WriteString("dhcp-no-override\n")
	return b.String(), nil
}

// leaseRange resolves the dhcp-range bounds LeaseMode renders: the
// explicit Config.LeaseRangeStart/End if both are set, otherwise the
// network's first and last host addresses (network+1 through
// broadcast-1 - the same HostMin/HostMax an IP calculator would report
// for the subnet). network and mask are the already-validated,
// already-masked values RenderDnsmasqConf computed from
// cfg.ProvisioningNet.
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

// RenderGrubConfig renders the GRUB bootstrap config TFTPServer serves
// at GrubConfigPath (see that constant's doc comment for how the
// netboot GRUB image comes to fetch it). It stays deliberately minimal:
// one configfile line handing control to the boot config server
// (internal/bootserver) at bootConfigURL, whose per-MAC
// /boot/grub.cfg-<mac> response carries all actual boot logic -
// duplicating any of it here would give the same decision two sources
// of truth. ${net_default_mac} is a GRUB variable expanded on the
// client: the booting NIC's own MAC, the same identity bootserver's
// MAC gate resolves. The path uses GRUB's network file syntax (see
// bootserver.GrubNetPath - GRUB cannot open bare URLs), so a
// bootConfigURL GRUB could never fetch from is an error here, at
// startup, rather than a silent boot-time dead end.
func RenderGrubConfig(bootConfigURL string) (string, error) {
	configPath, err := bootserver.GrubNetPath(bootConfigURL, "/boot/grub.cfg-${net_default_mac}")
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
func RenderHostsfile(macs []string) string {
	if len(macs) == 0 {
		return ""
	}
	sorted := slices.Clone(macs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var b strings.Builder
	for _, mac := range sorted {
		fmt.Fprintf(&b, "%s,set:%s\n", mac, kezioTag)
	}
	return b.String()
}
