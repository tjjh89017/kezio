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

package subnetvalidate

import (
	"testing"

	"github.com/tjjh89017/kezio/internal/nadvalidate"
)

func TestCheckDHCPLeaseRange(t *testing.T) {
	tests := []struct {
		name       string
		cidr       string
		start      string
		end        string
		want       nadvalidate.Verdict
		wantReason string
	}{
		{
			name:       "no lease range set is the auto-derive case",
			cidr:       "192.0.2.0/24",
			start:      "",
			end:        "",
			want:       nadvalidate.OK,
			wantReason: "DHCPLeaseRangeAutoDerived",
		},
		{
			name:       "forward-ordered range inside cidr",
			cidr:       "192.0.2.0/24",
			start:      "192.0.2.10",
			end:        "192.0.2.250",
			want:       nadvalidate.OK,
			wantReason: "DHCPLeaseRangeValid",
		},
		{
			name:       "reversed range",
			cidr:       "192.0.2.0/24",
			start:      "192.0.2.250",
			end:        "192.0.2.10",
			want:       nadvalidate.Violation,
			wantReason: "DHCPLeaseRangeReversed",
		},
		{
			name:       "forward-ordered range outside cidr",
			cidr:       "192.0.2.0/24",
			start:      "10.0.0.5",
			end:        "10.0.0.50",
			want:       nadvalidate.Violation,
			wantReason: "DHCPLeaseRangeOutsideCIDR",
		},
		{
			name:       "start inside cidr but end outside it",
			cidr:       "192.0.2.0/24",
			start:      "192.0.2.10",
			end:        "192.0.3.10",
			want:       nadvalidate.Violation,
			wantReason: "DHCPLeaseRangeOutsideCIDR",
		},
		{
			name:       "unparseable start - digit shape passes the CRD Pattern but the octet is out of range",
			cidr:       "192.0.2.0/24",
			start:      "999.1.1.1",
			end:        "192.0.2.250",
			want:       nadvalidate.Indeterminate,
			wantReason: "DHCPLeaseRangeStartInvalid",
		},
		{
			name:       "unparseable end",
			cidr:       "192.0.2.0/24",
			start:      "192.0.2.10",
			end:        "999.1.1.1",
			want:       nadvalidate.Indeterminate,
			wantReason: "DHCPLeaseRangeEndInvalid",
		},
		{
			name:       "unparseable cidr",
			cidr:       "not-a-cidr",
			start:      "192.0.2.10",
			end:        "192.0.2.250",
			want:       nadvalidate.Indeterminate,
			wantReason: "SubnetCIDRInvalid",
		},
		{
			name:       "only start set - the CRD's XValidation should never let this reach here, but handle it directly rather than assuming",
			cidr:       "192.0.2.0/24",
			start:      "192.0.2.10",
			end:        "",
			want:       nadvalidate.Indeterminate,
			wantReason: "DHCPLeaseRangeIncomplete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckDHCPLeaseRange(tt.cidr, tt.start, tt.end)
			if got.Verdict != tt.want {
				t.Errorf("Verdict = %v, want %v (reason %q)", got.Verdict, tt.want, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestCheckBootdServerIPInCIDR(t *testing.T) {
	tests := []struct {
		name          string
		cidr          string
		bootdServerIP string
		want          nadvalidate.Verdict
		wantReason    string
	}{
		{
			name:          "bootdServerIP inside cidr",
			cidr:          "192.0.2.0/24",
			bootdServerIP: "192.0.2.2",
			want:          nadvalidate.OK,
			wantReason:    "BootdServerIPInCIDR",
		},
		{
			name:          "bootdServerIP outside cidr - the reachable production defect: two matching static addresses, wrong segment",
			cidr:          "192.0.2.0/24",
			bootdServerIP: "203.0.113.5",
			want:          nadvalidate.Violation,
			wantReason:    "BootdServerIPOutsideCIDR",
		},
		{
			name:          "unparseable cidr",
			cidr:          "not-a-cidr",
			bootdServerIP: "192.0.2.2",
			want:          nadvalidate.Indeterminate,
			wantReason:    "SubnetCIDRInvalid",
		},
		{
			name:          "unparseable bootdServerIP - digit shape passes the CRD Pattern but the octet is out of range",
			cidr:          "192.0.2.0/24",
			bootdServerIP: "999.1.1.1",
			want:          nadvalidate.Indeterminate,
			wantReason:    "BootdServerIPInvalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckBootdServerIPInCIDR(tt.cidr, tt.bootdServerIP)
			if got.Verdict != tt.want {
				t.Errorf("Verdict = %v, want %v (reason %q)", got.Verdict, tt.want, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}
