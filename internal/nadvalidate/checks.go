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
	if bootdServerIP == "" {
		// A Subnet with no boot half (Subnet.HasBootPlane false) has no
		// bootdServerIP at all, so there is no address the pool could
		// collide with - a definite conclusion, not an unparseable or
		// unrecognised input, so OK (not Indeterminate) is the honest
		// verdict; the Reason/Message still say why the check found
		// nothing to flag, rather than the caller skipping the call.
		return CheckResult{
			Verdict: OK,
			Reason:  "NoBootdAddress",
			Message: "this Subnet has no boot half, so there is no bootdServerIP the seeder pool could collide with",
		}
	}

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

// CheckTrackerAddress checks that trackerIP - a Site's tracker.ip -
// falls inside subnetCIDR (the seeding Subnet's own CIDR) and outside
// the pool seederConfig - that Subnet's seeder NAD's spec.config - can
// hand to an attaching pod. Both halves matter: an address outside the
// CIDR is not reachable on the seeding Subnet at all, and one inside the
// seeder pool could be handed to a seeder pod and collide with the
// tracker's own pinned address, the same failure CheckSeederOverlap
// guards against for bootdServerIP.
func CheckTrackerAddress(subnetCIDR, seederConfig, trackerIP string) CheckResult {
	ip := parseIPv4(trackerIP)
	if ip == nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "TrackerIPInvalid",
			Message: fmt.Sprintf("tracker.ip %q is not a valid IPv4 address", trackerIP),
		}
	}

	_, cidr, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "SubnetCIDRUnparseable",
			Message: fmt.Sprintf("seeding Subnet cidr %q: %v", subnetCIDR, err),
		}
	}
	if !cidr.Contains(ip) {
		return CheckResult{
			Verdict: Violation,
			Reason:  "TrackerAddressOutsideCIDR",
			Message: fmt.Sprintf("tracker.ip %s falls outside the seeding Subnet's cidr %s, so it is not reachable on that Subnet", trackerIP, subnetCIDR),
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
		return CheckResult{Verdict: OK, Reason: "SeederIPAMEmpty", Message: "seeder NAD has no ipam configured, so it cannot hand out tracker.ip"}
	case KindStatic:
		for _, a := range ipam.Addresses {
			if a.Equal(ip) {
				return CheckResult{
					Verdict: Violation,
					Reason:  "TrackerOverlapStatic",
					Message: fmt.Sprintf("seeder NAD static ipam includes tracker.ip %s: a seeder pod would be handed the tracker's own address", trackerIP),
				}
			}
		}
		return CheckResult{Verdict: OK, Reason: "TrackerAddressAvailable", Message: "seeder NAD static ipam does not include tracker.ip"}
	case KindWhereabouts:
		if ipam.Range.Contains(ip) {
			return CheckResult{
				Verdict: Violation,
				Reason:  "TrackerOverlapWhereabouts",
				Message: fmt.Sprintf("seeder NAD whereabouts range %s includes tracker.ip %s: whereabouts could allocate it to a seeder pod", ipam.Range.String(), trackerIP),
			}
		}
		return CheckResult{Verdict: OK, Reason: "TrackerAddressAvailable", Message: "seeder NAD whereabouts range does not include tracker.ip"}
	case KindHostLocal:
		if ipam.RangeBounded || ipam.Subnet == nil {
			return CheckResult{
				Verdict: Indeterminate,
				Reason:  "TrackerIPAMHostLocalBounded",
				Message: "seeder NAD host-local ipam narrows its subnet with rangeStart/rangeEnd; cannot confirm tracker.ip is outside the pool",
			}
		}
		if ipam.Subnet.Contains(ip) {
			return CheckResult{
				Verdict: Violation,
				Reason:  "TrackerOverlapHostLocal",
				Message: fmt.Sprintf("seeder NAD host-local subnet %s includes tracker.ip %s", ipam.Subnet.String(), trackerIP),
			}
		}
		return CheckResult{Verdict: OK, Reason: "TrackerAddressAvailable", Message: "seeder NAD host-local subnet does not include tracker.ip"}
	default:
		return CheckResult{
			Verdict: Indeterminate,
			Reason:  "SeederIPAMUnrecognised",
			Message: fmt.Sprintf("seeder NAD ipam type %q is not recognised; cannot confirm tracker.ip %s is outside its pool", ipam.Kind, trackerIP),
		}
	}
}

// CheckSeederStaticWithTracker checks that seederConfig - the seeding
// Subnet's seeder NAD spec.config - does not use static ipam while
// trackerIP names a Site-managed tracker (Site.spec.tracker.ip). The
// tracker pod single-homes on that same NAD and pins trackerIP through
// a per-pod "ips" override; Multus hands that override to the ipam
// plugin as the CNI_ARGS "IP" entry, and the static plugin ADDS such an
// entry to the address list its own config declares. The tracker
// therefore comes up with the NAD's configured address as well as its
// pinned one, and that configured address is the address every seeder
// pod on the same NAD also holds.
//
// An empty trackerIP means the Site points at a tracker of its own
// (Site.spec.tracker.externalURL) or has no tracker at all: nothing
// pins a second address on this NAD, so static ipam stays correct.
// CheckSeederStaticMultiImage covers the remaining static-ipam risk
// there, which is seeder concurrency, not the tracker.
func CheckSeederStaticWithTracker(seederConfig, trackerIP string) CheckResult {
	if trackerIP == "" {
		return CheckResult{
			Verdict: OK,
			Reason:  "NoManagedTracker",
			Message: "this Site has no tracker of kezio's own on the seeder NAD, so nothing pins a second address on it",
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

	if ipam.Kind != KindStatic {
		return CheckResult{
			Verdict: OK,
			Reason:  "SeederIPAMNotStatic",
			Message: fmt.Sprintf("seeder NAD ipam type is %q, not static, so it can give the tracker and each seeder pod its own address", ipam.Kind),
		}
	}

	return CheckResult{
		Verdict: Violation,
		Reason:  "SeederStaticIPAMWithTracker",
		Message: fmt.Sprintf("seeder NAD uses static ipam while this Site pins tracker.ip %s on the same NAD: the static plugin adds its own configured address to the pinned one, so the tracker pod also holds the address every seeder pod holds; use a pool-type ipam (host-local or whereabouts) on this NAD", trackerIP),
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
