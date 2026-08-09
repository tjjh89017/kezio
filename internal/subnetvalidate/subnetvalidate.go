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

// Package subnetvalidate checks invariants that live entirely within a
// Subnet's own spec - no NetworkAttachmentDefinition involved, unlike
// internal/nadvalidate. The CRD's admission rules confirm field shape
// (patterns, presence) but not cross-field consistency, such as a lease
// range being forward-ordered and inside spec.cidr; a range that fails
// either only surfaces at bootd pod runtime as a malformed dnsmasq
// dhcp-range line, so it must be caught here instead.
package subnetvalidate

import (
	"bytes"
	"fmt"
	"net"

	"github.com/tjjh89017/kezio/internal/nadvalidate"
)

// CheckDHCPLeaseRange checks that leaseRangeStart and leaseRangeEnd,
// when present, describe a forward-ordered range fully inside cidr.
// Both empty is the auto-derive case and is OK.
func CheckDHCPLeaseRange(cidr, leaseRangeStart, leaseRangeEnd string) nadvalidate.CheckResult {
	startSet, endSet := leaseRangeStart != "", leaseRangeEnd != ""
	if !startSet && !endSet {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.OK,
			Reason:  "DHCPLeaseRangeAutoDerived",
			Message: "no explicit lease range set; bootd derives one from cidr",
		}
	}
	if startSet != endSet {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "DHCPLeaseRangeIncomplete",
			Message: "leaseRangeStart and leaseRangeEnd must both be set, or both left empty",
		}
	}

	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "SubnetCIDRInvalid",
			Message: fmt.Sprintf("cidr %q is not a valid IPv4 CIDR: %v", cidr, err),
		}
	}

	start := net.ParseIP(leaseRangeStart).To4()
	if start == nil {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "DHCPLeaseRangeStartInvalid",
			Message: fmt.Sprintf("leaseRangeStart %q is not a valid IPv4 address", leaseRangeStart),
		}
	}
	end := net.ParseIP(leaseRangeEnd).To4()
	if end == nil {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "DHCPLeaseRangeEndInvalid",
			Message: fmt.Sprintf("leaseRangeEnd %q is not a valid IPv4 address", leaseRangeEnd),
		}
	}

	if bytes.Compare(start, end) > 0 {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "DHCPLeaseRangeReversed",
			Message: fmt.Sprintf("leaseRangeStart %s is after leaseRangeEnd %s; dnsmasq's dhcp-range would be malformed", leaseRangeStart, leaseRangeEnd),
		}
	}

	if !ipnet.Contains(start) || !ipnet.Contains(end) {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "DHCPLeaseRangeOutsideCIDR",
			Message: fmt.Sprintf("lease range %s-%s is not fully inside cidr %s", leaseRangeStart, leaseRangeEnd, cidr),
		}
	}

	return nadvalidate.CheckResult{
		Verdict: nadvalidate.OK,
		Reason:  "DHCPLeaseRangeValid",
		Message: fmt.Sprintf("lease range %s-%s is ordered and inside cidr %s", leaseRangeStart, leaseRangeEnd, cidr),
	}
}

// CheckBootdServerIPInCIDR checks that bootdServerIP falls inside cidr.
// Nothing upstream of this check catches bootdServerIP naming an
// address on a different segment: a PXE client on cidr told a
// next-server outside it gets a silent mid-boot timeout, with no error
// anywhere in Kubernetes.
func CheckBootdServerIPInCIDR(cidr, bootdServerIP string) nadvalidate.CheckResult {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "SubnetCIDRInvalid",
			Message: fmt.Sprintf("cidr %q is not a valid IPv4 CIDR: %v", cidr, err),
		}
	}

	ip := net.ParseIP(bootdServerIP).To4()
	if ip == nil {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Indeterminate,
			Reason:  "BootdServerIPInvalid",
			Message: fmt.Sprintf("bootdServerIP %q is not a valid IPv4 address", bootdServerIP),
		}
	}

	if !ipnet.Contains(ip) {
		return nadvalidate.CheckResult{
			Verdict: nadvalidate.Violation,
			Reason:  "BootdServerIPOutsideCIDR",
			Message: fmt.Sprintf("bootdServerIP %s is not inside cidr %s; PXE clients on cidr would be handed a next-server outside their own segment", bootdServerIP, cidr),
		}
	}

	return nadvalidate.CheckResult{
		Verdict: nadvalidate.OK,
		Reason:  "BootdServerIPInCIDR",
		Message: fmt.Sprintf("bootdServerIP %s is inside cidr %s", bootdServerIP, cidr),
	}
}
