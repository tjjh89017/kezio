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

// Package subnetdhcp allocates and releases the boot-scoped DHCP address
// reservations recorded on a lease-mode Subnet's status.dhcp.reservations
// table. It is a pure, in-memory library: every function takes a Subnet
// object and mutates its Status.DHCP in place (or reports the address
// pool is exhausted); the caller (internal/deployer, at net-boot arm
// time, and internal/controller's SubnetReconciler, for garbage
// collection) owns every read-modify-write cycle against the API server,
// including the conflict-retry loop.
//
// Reserving a fixed address per booting machine, instead of leaving
// dnsmasq's own dynamic lease pool to arbitrate, is what removes the
// concurrent-OFFER race that produces DHCPNAK "address in use" at fleet
// scale: every address this package hands out is unique and pre-decided
// before the machine is ever powered on for net boot.
package subnetdhcp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// ErrPoolExhausted is returned by Reserve when no free address remains in
// the Subnet's lease range.
var ErrPoolExhausted = errors.New("subnetdhcp: lease-mode address pool is exhausted")

// ResolveNamespace returns ref.Namespace when set, otherwise
// defaultNamespace - the same NameRef default every other kezio
// controller applies.
func ResolveNamespace(ref keziov1alpha3.NameRef, defaultNamespace string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return defaultNamespace
}

// Reserve returns the address reserved for machineName in subnet's
// status.dhcp.reservations table, allocating a fresh one - the lowest
// free address in the Subnet's lease range that is not already held by
// another entry and is not bootdServerIP or a non-empty gateway - if the
// table carries no entry for machineName yet. Idempotent: calling it
// again for the same machineName, with the table unchanged, returns the
// same address and changed=false.
//
// subnet must be a lease-mode Subnet (subnet.Spec.DHCP.Mode ==
// SubnetDHCPModeLease); the caller decides that before calling in, since
// a proxy-mode Subnet has no address pool at all to reserve from.
//
// On success, subnet.Status.DHCP is mutated in place: a fresh allocation
// appends the new entry and recomputes Revision; a reused entry leaves
// the table untouched. changed reports whether the caller must persist
// subnet's status. ErrPoolExhausted leaves subnet.Status.DHCP untouched.
func Reserve(subnet *keziov1alpha3.Subnet, machineName, mac string, now metav1.Time) (address string, changed bool, err error) {
	if dhcp := subnet.Status.DHCP; dhcp != nil {
		for _, r := range dhcp.Reservations {
			if r.Machine == machineName {
				return r.Address, false, nil
			}
		}
	}

	start, end, err := LeaseRange(subnet)
	if err != nil {
		return "", false, err
	}

	taken := ReservedAddresses(subnet)
	var existing []keziov1alpha3.DHCPReservation
	if dhcp := subnet.Status.DHCP; dhcp != nil {
		existing = dhcp.Reservations
	}
	for _, r := range existing {
		taken[r.Address] = true
	}

	addr, ok := nextFree(start, end, taken)
	if !ok {
		return "", false, ErrPoolExhausted
	}

	entry := keziov1alpha3.DHCPReservation{Address: addr.String(), Machine: machineName, MAC: mac, Since: now}
	table := append(slices.Clone(existing), entry)
	if subnet.Status.DHCP == nil {
		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{}
	}
	subnet.Status.DHCP.Reservations = table
	subnet.Status.DHCP.Revision = Revision(table)
	return addr.String(), true, nil
}

// Release removes machineName's entry from subnet's
// status.dhcp.reservations table, if present, and recomputes Revision.
// changed reports whether an entry was actually removed - false means
// subnet is left untouched, so the caller can skip persisting it.
func Release(subnet *keziov1alpha3.Subnet, machineName string) (changed bool) {
	dhcp := subnet.Status.DHCP
	if dhcp == nil {
		return false
	}
	kept := make([]keziov1alpha3.DHCPReservation, 0, len(dhcp.Reservations))
	removed := false
	for _, r := range dhcp.Reservations {
		if r.Machine == machineName {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	if !removed {
		return false
	}
	dhcp.Reservations = kept
	dhcp.Revision = Revision(kept)
	return true
}

// LeaseRange resolves the inclusive IPv4 bounds of subnet's lease-mode
// address pool: subnet.Spec.DHCP.LeaseRangeStart/End when both are set,
// else the first and last host address of subnet.Spec.CIDR. Mirrors
// internal/bootd's own leaseRange, which renders the identical bounds
// into dnsmasq's dhcp-range - the two must never disagree, or a
// reservation could name an address dnsmasq itself would never lease.
func LeaseRange(subnet *keziov1alpha3.Subnet) (start, end net.IP, err error) {
	if subnet.Spec.DHCP == nil {
		return nil, nil, fmt.Errorf("subnetdhcp: Subnet %q has no dhcp configuration", subnet.Name)
	}

	_, ipnet, err := net.ParseCIDR(subnet.Spec.CIDR)
	if err != nil {
		return nil, nil, fmt.Errorf("subnetdhcp: Subnet %q cidr %q is not a valid IPv4 CIDR: %w", subnet.Name, subnet.Spec.CIDR, err)
	}
	network := ipnet.IP.Mask(ipnet.Mask).To4()
	mask := net.IP(ipnet.Mask).To4()
	if network == nil || mask == nil {
		return nil, nil, fmt.Errorf("subnetdhcp: Subnet %q cidr %q is not an IPv4 network", subnet.Name, subnet.Spec.CIDR)
	}

	startStr, endStr := subnet.Spec.DHCP.LeaseRangeStart, subnet.Spec.DHCP.LeaseRangeEnd
	if startStr != "" && endStr != "" {
		start = net.ParseIP(startStr).To4()
		end = net.ParseIP(endStr).To4()
		if start == nil || end == nil {
			return nil, nil, fmt.Errorf("subnetdhcp: Subnet %q has an invalid lease range %q-%q", subnet.Name, startStr, endStr)
		}
		return start, end, nil
	}

	n := binary.BigEndian.Uint32(network)
	m := binary.BigEndian.Uint32(mask)
	if ^m-1 < 1 {
		return nil, nil, fmt.Errorf("subnetdhcp: Subnet %q cidr %q has no usable host addresses for an auto-derived lease range", subnet.Name, subnet.Spec.CIDR)
	}
	return intToIPv4(n + 1), intToIPv4((n | ^m) - 1), nil
}

// ReservedAddresses returns the set of addresses subnet's lease pool must
// never hand out as a reservation: bootdServerIP always, and gateway
// when it is set to a non-empty address (the empty string means "no
// exit", not an address to avoid).
func ReservedAddresses(subnet *keziov1alpha3.Subnet) map[string]bool {
	taken := map[string]bool{}
	if subnet.Spec.BootdServerIP != "" {
		taken[subnet.Spec.BootdServerIP] = true
	}
	if subnet.Spec.DHCP != nil && subnet.Spec.DHCP.Gateway != nil && *subnet.Spec.DHCP.Gateway != "" {
		taken[*subnet.Spec.DHCP.Gateway] = true
	}
	return taken
}

// Revision computes a stable digest of reservations - a sha256 of a
// sorted "<machine>=<mac>=<address>" join is enough: it only has to
// change whenever the table changes and agree between the manager (which
// writes it) and bootd (which compares its own last-applied copy against
// it), never to resist tampering.
func Revision(reservations []keziov1alpha3.DHCPReservation) string {
	if len(reservations) == 0 {
		return ""
	}
	pairs := make([]string, len(reservations))
	for i, r := range reservations {
		pairs[i] = r.Machine + "=" + r.MAC + "=" + r.Address
	}
	sort.Strings(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
	return hex.EncodeToString(sum[:])
}

// nextFree returns the lowest IPv4 address in [start,end] not present in
// taken, or ok=false when every address in the range is taken.
func nextFree(start, end net.IP, taken map[string]bool) (net.IP, bool) {
	s := binary.BigEndian.Uint32(start.To4())
	e := binary.BigEndian.Uint32(end.To4())
	for v := s; v <= e; v++ {
		candidate := intToIPv4(v)
		if !taken[candidate.String()] {
			return candidate, true
		}
		if v == e {
			break // avoid overflow when e is uint32 max, unreachable for real IPv4 ranges
		}
	}
	return nil, false
}

func intToIPv4(v uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}
