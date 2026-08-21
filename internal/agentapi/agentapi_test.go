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

package agentapi

import (
	"encoding/json"
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestRouteAndHeaderConstants(t *testing.T) {
	if RegisterPath != "/agent/register" {
		t.Fatalf("RegisterPath = %q, want /agent/register", RegisterPath)
	}
	if NextPath != "/agent/next" {
		t.Fatalf("NextPath = %q, want /agent/next", NextPath)
	}
	if AgentSchemaVersionHeader != "X-Kezio-Agent-Schema-Version" {
		t.Fatalf("AgentSchemaVersionHeader = %q, want X-Kezio-Agent-Schema-Version", AgentSchemaVersionHeader)
	}
	if AgentSchemaVersion <= 0 {
		t.Fatalf("AgentSchemaVersion = %d, want a positive version", AgentSchemaVersion)
	}
	if ActionWait != "wait" {
		t.Fatalf("ActionWait = %q, want wait", ActionWait)
	}
}

func TestRegisterRequestRoundTrip(t *testing.T) {
	rotational := false
	want := RegisterRequest{
		Hardware: keziov1alpha2.MachineHardwareSpec{
			Disks: []keziov1alpha2.MachineHardwareDisk{
				{DeviceName: "/dev/nvme0n1", SerialNumber: "S123", SizeBytes: 512 << 30, Rotational: &rotational},
			},
			Nics: []keziov1alpha2.MachineHardwareNIC{
				{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:01"},
			},
			MemoryBytes: 16 << 30,
			CPUCount:    8,
		},
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RegisterRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Hardware.Disks) != 1 || got.Hardware.Disks[0].SerialNumber != "S123" {
		t.Fatalf("Hardware.Disks = %+v, want the round-tripped disk", got.Hardware.Disks)
	}
	if got.Hardware.Disks[0].Rotational == nil || *got.Hardware.Disks[0].Rotational != false {
		t.Fatalf("Hardware.Disks[0].Rotational = %v, want a round-tripped false", got.Hardware.Disks[0].Rotational)
	}
	if len(got.Hardware.Nics) != 1 || got.Hardware.Nics[0].MACAddress != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("Hardware.Nics = %+v, want the round-tripped nic", got.Hardware.Nics)
	}
	if got.Hardware.MemoryBytes != want.Hardware.MemoryBytes || got.Hardware.CPUCount != want.Hardware.CPUCount {
		t.Fatalf("MemoryBytes/CPUCount = %d/%d, want %d/%d", got.Hardware.MemoryBytes, got.Hardware.CPUCount, want.Hardware.MemoryBytes, want.Hardware.CPUCount)
	}
}

func TestRegisterResponseRoundTrip(t *testing.T) {
	want := RegisterResponse{MachineName: "node-01", SessionToken: "abc123", SessionTTLSeconds: 21600}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RegisterResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestNextResponseRoundTrip(t *testing.T) {
	want := NextResponse{Action: ActionWait, PollIntervalSeconds: 15}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got NextResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestErrorResponseRoundTrip(t *testing.T) {
	want := ErrorResponse{Error: "unauthorized"}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ErrorResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
