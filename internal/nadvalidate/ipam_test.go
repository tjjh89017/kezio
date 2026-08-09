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
	"errors"
	"net"
	"testing"
)

const bootdStaticConfig = `{
  "cniVersion": "0.3.1",
  "name": "kezio-boot-network",
  "plugins": [
    {
      "type": "macvlan",
      "master": "eth1",
      "mode": "bridge",
      "ipam": {
        "type": "static",
        "addresses": [
          {"address": "192.0.2.2/32"}
        ]
      }
    },
    {
      "type": "portmap",
      "capabilities": {"portMappings": true}
    }
  ]
}`

const seederWhereaboutsConfig = `{
  "cniVersion": "0.3.1",
  "type": "bridge",
  "bridge": "provisioning-br0",
  "ipam": {
    "type": "whereabouts",
    "range": "192.0.2.128/25",
    "routes": [{"dst": "10.0.0.0/8"}]
  }
}`

func TestParseIPAM(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
		wantKind   string
		wantErr    error
		check      func(t *testing.T, ipam IPAM)
	}{
		{
			name:       "single plugin static ipam",
			configJSON: `{"type":"bridge","ipam":{"type":"static","addresses":[{"address":"192.0.2.5/32"}]}}`,
			wantKind:   KindStatic,
			check: func(t *testing.T, ipam IPAM) {
				if len(ipam.Addresses) != 1 || !ipam.Addresses[0].Equal(net.ParseIP("192.0.2.5")) {
					t.Fatalf("Addresses = %v, want [192.0.2.5]", ipam.Addresses)
				}
			},
		},
		{
			name:       "plugin chain static ipam nested in first plugin",
			configJSON: bootdStaticConfig,
			wantKind:   KindStatic,
			check: func(t *testing.T, ipam IPAM) {
				if len(ipam.Addresses) != 1 || !ipam.Addresses[0].Equal(net.ParseIP("192.0.2.2")) {
					t.Fatalf("Addresses = %v, want [192.0.2.2]", ipam.Addresses)
				}
			},
		},
		{
			name:       "bare IPv4 address without prefix",
			configJSON: `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9"}]}}`,
			wantKind:   KindStatic,
			check: func(t *testing.T, ipam IPAM) {
				if len(ipam.Addresses) != 1 || !ipam.Addresses[0].Equal(net.ParseIP("192.0.2.9")) {
					t.Fatalf("Addresses = %v, want [192.0.2.9]", ipam.Addresses)
				}
			},
		},
		{
			name:       "whereabouts range",
			configJSON: seederWhereaboutsConfig,
			wantKind:   KindWhereabouts,
			check: func(t *testing.T, ipam IPAM) {
				if ipam.Range == nil || ipam.Range.String() != "192.0.2.128/25" {
					t.Fatalf("Range = %v, want 192.0.2.128/25", ipam.Range)
				}
			},
		},
		{
			name:       "host-local subnet without range bounds",
			configJSON: `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24"}}`,
			wantKind:   KindHostLocal,
			check: func(t *testing.T, ipam IPAM) {
				if ipam.Subnet == nil || ipam.Subnet.String() != "192.0.2.0/24" {
					t.Fatalf("Subnet = %v, want 192.0.2.0/24", ipam.Subnet)
				}
				if ipam.RangeBounded {
					t.Fatalf("RangeBounded = true, want false")
				}
			},
		},
		{
			name:       "host-local subnet narrowed by rangeStart/rangeEnd is bounded",
			configJSON: `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24","rangeStart":"192.0.2.10","rangeEnd":"192.0.2.20"}}`,
			wantKind:   KindHostLocal,
			check: func(t *testing.T, ipam IPAM) {
				if !ipam.RangeBounded {
					t.Fatalf("RangeBounded = false, want true")
				}
				if ipam.Subnet != nil {
					t.Fatalf("Subnet = %v, want nil when RangeBounded", ipam.Subnet)
				}
			},
		},
		{
			name:       "empty ipam object",
			configJSON: `{"type":"bridge","ipam":{}}`,
			wantKind:   KindNone,
		},
		{
			name:       "unknown ipam kind degrades without error",
			configJSON: `{"ipam":{"type":"dhcp"}}`,
			wantKind:   "dhcp",
		},
		{
			name:       "unparseable JSON",
			configJSON: `{not json`,
			wantErr:    ErrConfigUnparseable,
		},
		{
			name:       "no ipam section anywhere",
			configJSON: `{"type":"bridge","bridge":"br0"}`,
			wantErr:    ErrNoIPAMSection,
		},
		{
			name:       "malformed static address",
			configJSON: `{"ipam":{"type":"static","addresses":[{"address":"not-an-ip"}]}}`,
			wantErr:    ErrConfigUnparseable,
		},
		{
			name:       "malformed whereabouts range",
			configJSON: `{"ipam":{"type":"whereabouts","range":"not-a-cidr"}}`,
			wantErr:    ErrConfigUnparseable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipam, err := ParseIPAM(tt.configJSON)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is(_, %v)", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if ipam.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q", ipam.Kind, tt.wantKind)
			}
			if tt.check != nil {
				tt.check(t, ipam)
			}
		})
	}
}
