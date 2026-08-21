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

import "testing"

func TestCheckBootdAddress(t *testing.T) {
	tests := []struct {
		name          string
		bootdConfig   string
		bootdServerIP string
		want          Verdict
		wantReason    string
	}{
		{
			name:          "static address matches bootdServerIP",
			bootdConfig:   `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.2/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "bootdServerIP with an out-of-range octet passes the CRD's digit-shape pattern but must fail IPv4 parsing here",
			bootdConfig:   `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.2/32"}]}}`,
			bootdServerIP: "999.1.1.1",
			want:          Indeterminate,
			wantReason:    "BootdServerIPInvalid",
		},
		{
			name:          "static address list with bootdServerIP as a non-first entry: a loop trimmed to Addresses[0] would miss this",
			bootdConfig:   `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"},{"address":"192.0.2.2/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
			wantReason:    "BootdAddressMatch",
		},
		{
			name:          "static ipam with an empty address list is distinct from no ipam configured at all: both must still be Violation",
			bootdConfig:   `{"ipam":{"type":"static","addresses":[]}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
			wantReason:    "BootdAddressMismatch",
		},
		{
			name:          "static address mismatch is the failure this check exists for: dnsmasq binds one address while advertising another as PXE next-server",
			bootdConfig:   `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.3/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "plugin-chain config with matching static address",
			bootdConfig:   bootdStaticConfig,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "empty ipam cannot pin any address",
			bootdConfig:   `{"ipam":{}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "whereabouts ipam never pins an address, so it violates the static requirement",
			bootdConfig:   seederWhereaboutsConfig,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "host-local ipam over its full subnet never pins an address, so it violates the static requirement",
			bootdConfig:   `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "host-local ipam bounded by rangeStart/rangeEnd could pin a single address, so it cannot be confirmed either way",
			bootdConfig:   `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24","rangeStart":"192.0.2.2","rangeEnd":"192.0.2.2"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
		{
			name:          "unknown ipam kind is cannot-determine, not a violation",
			bootdConfig:   `{"ipam":{"type":"dhcp"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
		{
			name:          "unparseable config is cannot-determine, not a violation",
			bootdConfig:   `{not json`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckBootdAddress(tt.bootdConfig, tt.bootdServerIP)
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %v, want %v (message: %s)", got.Verdict, tt.want, got.Message)
			}
			if got.Reason == "" {
				t.Fatalf("Reason is empty, want a CamelCase reason token")
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Fatalf("Reason = %v, want %v", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestCheckSeederOverlap(t *testing.T) {
	tests := []struct {
		name          string
		seederConfig  string
		bootdServerIP string
		want          Verdict
		wantReason    string
	}{
		{
			name:          "bootdServerIP inside a whereabouts range is a violation",
			seederConfig:  `{"ipam":{"type":"whereabouts","range":"192.0.2.0/24"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "bootdServerIP with an out-of-range octet passes the CRD's digit-shape pattern but must fail IPv4 parsing here",
			seederConfig:  `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"}]}}`,
			bootdServerIP: "999.1.1.1",
			want:          Indeterminate,
			wantReason:    "BootdServerIPInvalid",
		},
		{
			name:          "static address list with bootdServerIP as a non-first entry: a loop trimmed to Addresses[0] would miss this",
			seederConfig:  `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"},{"address":"192.0.2.2/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
			wantReason:    "SeederOverlapStatic",
		},
		{
			name:          "static ipam with an empty address list is distinct from no ipam configured at all: it still cannot hand out bootdServerIP",
			seederConfig:  `{"ipam":{"type":"static","addresses":[]}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
			wantReason:    "SeederOverlapNone",
		},
		{
			name:          "bootdServerIP outside a whereabouts range passes",
			seederConfig:  `{"ipam":{"type":"whereabouts","range":"192.0.2.128/25"}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "shared NAD: whereabouts range excludes bootdServerIP even though bootd also attaches through this NAD",
			seederConfig:  seederWhereaboutsConfig,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "static seeder address equal to bootdServerIP is a violation",
			seederConfig:  `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.2/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "static seeder address distinct from bootdServerIP passes",
			seederConfig:  `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"}]}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "host-local subnet containing bootdServerIP is a violation",
			seederConfig:  `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Violation,
		},
		{
			name:          "host-local narrowed by rangeStart/rangeEnd cannot be determined",
			seederConfig:  `{"ipam":{"type":"host-local","subnet":"192.0.2.0/24","rangeStart":"192.0.2.10","rangeEnd":"192.0.2.20"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
		{
			name:          "host-local subnet excluding bootdServerIP passes",
			seederConfig:  `{"ipam":{"type":"host-local","subnet":"198.51.100.0/24"}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "empty ipam cannot hand out any address",
			seederConfig:  `{"ipam":{}}`,
			bootdServerIP: "192.0.2.2",
			want:          OK,
		},
		{
			name:          "unknown ipam kind is cannot-determine, not a false violation",
			seederConfig:  `{"ipam":{"type":"dhcp"}}`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
		{
			name:          "unparseable config is cannot-determine, not a false violation",
			seederConfig:  `{not json`,
			bootdServerIP: "192.0.2.2",
			want:          Indeterminate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckSeederOverlap(tt.seederConfig, tt.bootdServerIP)
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %v, want %v (message: %s)", got.Verdict, tt.want, got.Message)
			}
			if got.Reason == "" {
				t.Fatalf("Reason is empty, want a CamelCase reason token")
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Fatalf("Reason = %v, want %v", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestCheckSeederStaticMultiImage(t *testing.T) {
	tests := []struct {
		name             string
		seederConfig     string
		concurrentImages int
		want             Verdict
	}{
		{
			name:             "static ipam with zero concurrent Images, the normal state of a freshly created Subnet, is fine",
			seederConfig:     `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"}]}}`,
			concurrentImages: 0,
			want:             OK,
		},
		{
			name:             "static ipam with a single concurrent Image is fine",
			seederConfig:     `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"}]}}`,
			concurrentImages: 1,
			want:             OK,
		},
		{
			name:             "static ipam with several concurrent Images is an advisory, not a rejection",
			seederConfig:     `{"ipam":{"type":"static","addresses":[{"address":"192.0.2.9/32"}]}}`,
			concurrentImages: 3,
			want:             Advisory,
		},
		{
			name:             "whereabouts ipam is unaffected by concurrency",
			seederConfig:     seederWhereaboutsConfig,
			concurrentImages: 5,
			want:             OK,
		},
		{
			name:             "unknown ipam kind is not confirmed static, so no advisory",
			seederConfig:     `{"ipam":{"type":"dhcp"}}`,
			concurrentImages: 5,
			want:             OK,
		},
		{
			name:             "unparseable config is cannot-determine",
			seederConfig:     `{not json`,
			concurrentImages: 5,
			want:             Indeterminate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckSeederStaticMultiImage(tt.seederConfig, tt.concurrentImages)
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %v, want %v (message: %s)", got.Verdict, tt.want, got.Message)
			}
		})
	}
}
