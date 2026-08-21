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
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// ipam kind strings, as they appear verbatim in a CNI config's
// ipam.type field.
const (
	KindStatic      = "static"
	KindWhereabouts = "whereabouts"
	KindHostLocal   = "host-local"
	// KindNone is not a real ipam.type value - it is what ParseIPAM
	// reports for "ipam": {} (present but empty), the KubeVirt-only
	// shape that deliberately hands a pod's NIC no address at all (see
	// .github/actions/create-provisioning-nads's kezio-boot-network-vm).
	KindNone = ""
)

// ErrConfigUnparseable classifies a ParseIPAM failure caused by
// spec.config not being valid JSON, or its ipam section not matching
// the shape ParseIPAM expects.
var ErrConfigUnparseable = errors.New("nad config: cannot parse spec.config")

// ErrNoIPAMSection classifies a ParseIPAM failure caused by neither the
// top-level config nor any entry of a plugin chain carrying an "ipam"
// key at all - distinct from KindNone, which is an ipam key present but
// empty.
var ErrNoIPAMSection = errors.New("nad config: no ipam section present")

// IPAM is the parsed ipam section of a NetworkAttachmentDefinition's
// spec.config, holding only what CheckBootdAddress/CheckSeederOverlap/
// CheckSeederStaticMultiImage need from each recognised kind.
type IPAM struct {
	// Kind is KindStatic, KindWhereabouts, KindHostLocal, KindNone, or
	// - for an ipam.type this package does not recognise - that type
	// string verbatim.
	Kind string
	// Addresses holds the static plugin's fixed address list
	// (KindStatic only).
	Addresses []net.IP
	// Range is the whereabouts plugin's pool CIDR (KindWhereabouts
	// only).
	Range *net.IPNet
	// Subnet is the host-local plugin's pool CIDR (KindHostLocal only),
	// nil when RangeBounded is true.
	Subnet *net.IPNet
	// RangeBounded is true when a KindHostLocal ipam narrows its subnet
	// with rangeStart/rangeEnd. This package does not model that
	// narrowing, so Subnet is left nil and callers must treat pool
	// membership as indeterminate rather than assume the full subnet.
	RangeBounded bool
}

// cniIPAM is the raw shape of a CNI plugin's "ipam" object, covering
// every field the static, whereabouts, and host-local plugins use.
type cniIPAM struct {
	Type      string `json:"type"`
	Addresses []struct {
		Address string `json:"address"`
	} `json:"addresses"`
	Range      string `json:"range"`
	Subnet     string `json:"subnet"`
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`
}

// ParseIPAM extracts and interprets the ipam section of a
// NetworkAttachmentDefinition's spec.config, handling both a
// single-plugin config (a top-level "ipam" object) and a plugin chain
// (an "ipam" object nested in one element of a "plugins" list).
func ParseIPAM(configJSON string) (IPAM, error) {
	var top struct {
		IPAM    json.RawMessage `json:"ipam"`
		Plugins []struct {
			IPAM json.RawMessage `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(configJSON), &top); err != nil {
		return IPAM{}, fmt.Errorf("%w: %v", ErrConfigUnparseable, err)
	}

	raw := top.IPAM
	if raw == nil {
		for _, plugin := range top.Plugins {
			if plugin.IPAM != nil {
				raw = plugin.IPAM
				break
			}
		}
	}
	if raw == nil {
		return IPAM{}, ErrNoIPAMSection
	}

	var parsed cniIPAM
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return IPAM{}, fmt.Errorf("%w: ipam: %v", ErrConfigUnparseable, err)
	}

	switch parsed.Type {
	case KindStatic:
		addrs := make([]net.IP, 0, len(parsed.Addresses))
		for _, a := range parsed.Addresses {
			ip, err := parseHostAddress(a.Address)
			if err != nil {
				return IPAM{}, fmt.Errorf("%w: static address %q: %v", ErrConfigUnparseable, a.Address, err)
			}
			addrs = append(addrs, ip)
		}
		return IPAM{Kind: KindStatic, Addresses: addrs}, nil
	case KindWhereabouts:
		_, ipnet, err := net.ParseCIDR(parsed.Range)
		if err != nil {
			return IPAM{}, fmt.Errorf("%w: whereabouts range %q: %v", ErrConfigUnparseable, parsed.Range, err)
		}
		return IPAM{Kind: KindWhereabouts, Range: ipnet}, nil
	case KindHostLocal:
		if parsed.RangeStart != "" || parsed.RangeEnd != "" {
			return IPAM{Kind: KindHostLocal, RangeBounded: true}, nil
		}
		_, ipnet, err := net.ParseCIDR(parsed.Subnet)
		if err != nil {
			return IPAM{}, fmt.Errorf("%w: host-local subnet %q: %v", ErrConfigUnparseable, parsed.Subnet, err)
		}
		return IPAM{Kind: KindHostLocal, Subnet: ipnet}, nil
	case KindNone:
		return IPAM{Kind: KindNone}, nil
	default:
		return IPAM{Kind: parsed.Type}, nil
	}
}

// parseHostAddress reads the address portion out of a static ipam
// entry, which the "static" CNI plugin accepts either as a bare IPv4
// address or as "address/prefix" (see
// config/bootd/networkattachmentdefinition.example.yaml's
// "REPLACE_WITH_STATIC_CIDR/32").
func parseHostAddress(s string) (net.IP, error) {
	if ip := net.ParseIP(s); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
		return nil, fmt.Errorf("%s is not an IPv4 address", s)
	}
	ip, _, err := net.ParseCIDR(s)
	if err != nil {
		return nil, err
	}
	if v4 := ip.To4(); v4 != nil {
		return v4, nil
	}
	return nil, fmt.Errorf("%s is not an IPv4 address", s)
}
