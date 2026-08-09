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

package nadvalidate

import (
	"fmt"
	"net"
)

// reasonSeederOverlapNone is CheckSeederOverlap's OK reason, shared
// across the static/whereabouts/host-local branches that all reach the
// same conclusion by different means.
const reasonSeederOverlapNone = "SeederOverlapNone"

// parseIPv4 is the shared entry-address parse both checks below start
// from.
func parseIPv4(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	return ip.To4()
}

// CheckBootdAddress checks that bootdConfig - a bootd NAD's spec.config
// - assigns bootdServerIP as a static address. A bootd NAD using
// anything other than static ipam, or one whose static address list
// does not include bootdServerIP, means dnsmasq will bind whatever
// address it is actually handed while bootdServerIP keeps being
// advertised as the PXE next-server: a machine then PXEs to an address
// nothing is listening on, mid-boot, with no error anywhere in
// Kubernetes.
func CheckBootdAddress(bootdConfig, bootdServerIP string) CheckResult {
	ip := parseIPv4(bootdServerIP)
	if ip == nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "BootdServerIPInvalid",
			Message: fmt.Sprintf("bootdServerIP %q is not a valid IPv4 address", bootdServerIP),
		}
	}

	ipam, err := ParseIPAM(bootdConfig)
	if err != nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "BootdNADConfigUnparseable",
			Message: fmt.Sprintf("bootd NAD config: %v", err),
		}
	}

	switch ipam.Kind {
	case KindStatic:
		for _, a := range ipam.Addresses {
			if a.Equal(ip) {
				return CheckResult{
					Verdict: OK,
					Reason:  "BootdAddressMatch",
					Message: fmt.Sprintf("bootd NAD static ipam includes bootdServerIP %s", bootdServerIP),
				}
			}
		}
		return CheckResult{
			Verdict: Violation,
			Reason:  "BootdAddressMismatch",
			Message: fmt.Sprintf("bootd NAD static ipam does not include bootdServerIP %s: dnsmasq would bind a different address than the one advertised as PXE next-server", bootdServerIP),
		}
	case KindNone:
		return CheckResult{
			Verdict: Violation,
			Reason:  "BootdIPAMEmpty",
			Message: "bootd NAD has no ipam configured, so no address is pinned; bootdServerIP requires static ipam",
		}
	case KindWhereabouts:
		// whereabouts always hands out an address from a range, so no
		// address is pinned - unlike KindHostLocal below, which can bound
		// its range to bootdServerIP alone.
		return CheckResult{
			Verdict: Violation,
			Reason:  "BootdIPAMNotStatic",
			Message: fmt.Sprintf("bootd NAD ipam type %q hands out an address from a range, so no address is pinned; bootdServerIP requires static ipam", ipam.Kind),
		}
	case KindHostLocal:
		if ipam.RangeBounded {
			// rangeStart/rangeEnd could legitimately bound the pool to
			// bootdServerIP alone; this package does not parse the bounds,
			// so it cannot tell that case apart from a wider one.
			return CheckResult{
				Verdict: Indeterminate,
				Reason:  "BootdIPAMHostLocalBounded",
				Message: fmt.Sprintf("bootd NAD host-local ipam narrows its subnet with rangeStart/rangeEnd; cannot confirm bootdServerIP %s is the only address it can hand out", bootdServerIP),
			}
		}
		return CheckResult{
			Verdict: Violation,
			Reason:  "BootdIPAMNotStatic",
			Message: "bootd NAD host-local ipam allocates from its full subnet, so no address is pinned; bootdServerIP requires static ipam",
		}
	default:
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "BootdIPAMUnrecognised",
			Message: fmt.Sprintf("bootd NAD ipam type %q is not recognised; cannot confirm bootdServerIP %s is the address bootd will actually bind", ipam.Kind, bootdServerIP),
		}
	}
}

// CheckSeederOverlap checks that bootdServerIP falls outside the pool
// seederConfig - a seeder NAD's spec.config - can hand to an attaching
// pod. This holds whether or not the seeder NAD is the same NAD bootd
// uses (sharing one NAD between the two roles is allowed); a pool that
// includes bootdServerIP could hand it to a seeder pod and collide with
// the PXE next-server bootd itself is bound to.
func CheckSeederOverlap(seederConfig, bootdServerIP string) CheckResult {
	ip := parseIPv4(bootdServerIP)
	if ip == nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "BootdServerIPInvalid",
			Message: fmt.Sprintf("bootdServerIP %q is not a valid IPv4 address", bootdServerIP),
		}
	}

	ipam, err := ParseIPAM(seederConfig)
	if err != nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "SeederNADConfigUnparseable",
			Message: fmt.Sprintf("seeder NAD config: %v", err),
		}
	}

	switch ipam.Kind {
	case KindNone:
		return CheckResult{Verdict: OK, Reason: "SeederIPAMEmpty", Message: "seeder NAD has no ipam configured, so it cannot hand out bootdServerIP"}
	case KindStatic:
		for _, a := range ipam.Addresses {
			if a.Equal(ip) {
				return CheckResult{
					Verdict: Violation,
					Reason:  "SeederOverlapStatic",
					Message: fmt.Sprintf("seeder NAD static ipam includes bootdServerIP %s: a seeder pod would be handed bootd's own PXE next-server address", bootdServerIP),
				}
			}
		}
		return CheckResult{Verdict: OK, Reason: reasonSeederOverlapNone, Message: "seeder NAD static ipam does not include bootdServerIP"}
	case KindWhereabouts:
		if ipam.Range.Contains(ip) {
			return CheckResult{
				Verdict: Violation,
				Reason:  "SeederOverlapWhereabouts",
				Message: fmt.Sprintf("seeder NAD whereabouts range %s includes bootdServerIP %s: whereabouts could allocate it to a seeder pod", ipam.Range.String(), bootdServerIP),
			}
		}
		return CheckResult{Verdict: OK, Reason: reasonSeederOverlapNone, Message: "seeder NAD whereabouts range does not include bootdServerIP"}
	case KindHostLocal:
		if ipam.RangeBounded || ipam.Subnet == nil {
			return CheckResult{
				Verdict: Indeterminate,
				Reason:  "SeederIPAMHostLocalBounded",
				Message: "seeder NAD host-local ipam narrows its subnet with rangeStart/rangeEnd; cannot confirm bootdServerIP is outside the pool",
			}
		}
		if ipam.Subnet.Contains(ip) {
			return CheckResult{
				Verdict: Violation,
				Reason:  "SeederOverlapHostLocal",
				Message: fmt.Sprintf("seeder NAD host-local subnet %s includes bootdServerIP %s", ipam.Subnet.String(), bootdServerIP),
			}
		}
		return CheckResult{Verdict: OK, Reason: reasonSeederOverlapNone, Message: "seeder NAD host-local subnet does not include bootdServerIP"}
	default:
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "SeederIPAMUnrecognised",
			Message: fmt.Sprintf("seeder NAD ipam type %q is not recognised; cannot confirm bootdServerIP %s is outside its pool", ipam.Kind, bootdServerIP),
		}
	}
}

// CheckSeederStaticMultiImage flags - but does not reject - a seeder
// NAD using static ipam while its Site runs more than one Image
// concurrently (concurrentImages). Static ipam hands every attaching
// pod the same address, which is exactly correct for a Site that never
// runs more than one Image at a time; it stops being safe once a
// second concurrent per-Image seeder needs its own address from the
// same pool.
func CheckSeederStaticMultiImage(seederConfig string, concurrentImages int) CheckResult {
	ipam, err := ParseIPAM(seederConfig)
	if err != nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "SeederNADConfigUnparseable",
			Message: fmt.Sprintf("seeder NAD config: %v", err),
		}
	}

	if ipam.Kind != KindStatic {
		return CheckResult{Verdict: OK, Reason: "SeederIPAMNotStatic", Message: fmt.Sprintf("seeder NAD ipam type is %q, not static", ipam.Kind)}
	}

	if concurrentImages <= 1 {
		return CheckResult{
			Verdict: OK,
			Reason:  "SeederStaticIPAMSingleImage",
			Message: "seeder NAD uses static ipam; safe while at most one Image deploys at this Site concurrently",
		}
	}

	return CheckResult{
		Verdict: Advisory,
		Reason:  "SeederStaticIPAMMultiImage",
		Message: fmt.Sprintf("seeder NAD uses static ipam but %d Images are deploying concurrently at this Site; static ipam hands every seeder pod the same address, so a range-based pool sized for that concurrency is needed instead", concurrentImages),
	}
}
