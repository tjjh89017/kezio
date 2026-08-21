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
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestRouteAndHeaderConstants(t *testing.T) {
	if RegisterPath != "/agent/register" {
		t.Fatalf("RegisterPath = %q, want /agent/register", RegisterPath)
	}
	if NextPath != "/agent/next" {
		t.Fatalf("NextPath = %q, want /agent/next", NextPath)
	}
	if ProgressPath != "/agent/progress" {
		t.Fatalf("ProgressPath = %q, want /agent/progress", ProgressPath)
	}
	if AgentSchemaVersionHeader != "X-Kezio-Agent-Schema-Version" {
		t.Fatalf("AgentSchemaVersionHeader = %q, want X-Kezio-Agent-Schema-Version", AgentSchemaVersionHeader)
	}
	if AgentSchemaVersion != 3 {
		t.Fatalf("AgentSchemaVersion = %d, want 3 (strictly greater than every schema version an earlier agent build ever sent)", AgentSchemaVersion)
	}
	if ActionWait != "wait" {
		t.Fatalf("ActionWait = %q, want wait", ActionWait)
	}
	if ActionDeploy != "deploy" {
		t.Fatalf("ActionDeploy = %q, want deploy", ActionDeploy)
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

func TestNextResponseWithPlanRoundTrip(t *testing.T) {
	want := NextResponse{
		Action:              ActionDeploy,
		PollIntervalSeconds: 15,
		Plan:                &DeployPlan{SchemaVersion: AgentSchemaVersion, RunName: "run-1", TargetDisk: "/dev/nvme0n1"},
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got NextResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Action != want.Action || got.Plan == nil || got.Plan.RunName != want.Plan.RunName {
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

func validDeployPlan() DeployPlan {
	return DeployPlan{
		SchemaVersion: AgentSchemaVersion,
		RunName:       "run-1",
		RunUID:        "uid-1",
		TargetDisk:    "/dev/nvme0n1",
		SfdiskScript:  `{"partitiontable":{}}`,
		Slots: []DeploySlot{
			{Number: 1, Device: "/dev/nvme0n1p1", Mkfs: &DeployMkfs{Filesystem: "vfat"}},
			{Number: 2, Device: "/dev/nvme0n1p2", Torrent: &DeployTorrent{URL: "http://seeder/1.torrent", InfoHash: "abc123"}},
			{Number: 3, Device: "/dev/nvme0n1p3", Swap: &DeploySwap{UUID: "swap-uuid"}},
		},
		DataImages: []DeployDataImagePlan{
			{
				ImageRef:     keziov1alpha2.NameRef{Name: "data-image"},
				TargetDisk:   "/dev/sdb",
				SfdiskScript: `{"partitiontable":{}}`,
				Slots:        []DeploySlot{{Number: 1, Device: "/dev/sdb1", Mkfs: &DeployMkfs{Filesystem: "ext4"}}},
			},
		},
		Hooks: []ResolvedHook{
			{
				Name: "default",
				Steps: []ResolvedHookStep{
					{Type: HookStepTypeBuiltin, Builtin: "efibootmgr", TimeoutSeconds: 30},
					{Type: HookStepTypeScript, Content: "echo hi", TimeoutSeconds: 30},
					{Type: HookStepTypeChrootScript, Content: "echo chroot", OSFamily: keziov1alpha2.OSFamilyLinux, TimeoutSeconds: 30},
				},
			},
		},
		AfterDeploy: keziov1alpha2.AfterDeployReboot,
	}
}

func TestDeployPlanRoundTripPreservesFieldNames(t *testing.T) {
	want := validDeployPlan()

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	for _, key := range []string{"schemaVersion", "runName", "runUID", "targetDisk", "sfdiskScript", "slots", "dataImages", "hooks", "afterDeploy"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("marshaled DeployPlan is missing key %q: %s", key, raw)
		}
	}

	var got DeployPlan
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RunName != want.RunName || got.TargetDisk != want.TargetDisk || len(got.Slots) != len(want.Slots) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if got.Slots[1].Torrent == nil || got.Slots[1].Torrent.InfoHash != "abc123" {
		t.Fatalf("Slots[1].Torrent = %+v, want the round-tripped torrent", got.Slots[1].Torrent)
	}
	if len(got.DataImages) != 1 || got.DataImages[0].ImageRef.Name != "data-image" {
		t.Fatalf("DataImages = %+v, want the round-tripped data image plan", got.DataImages)
	}
	if len(got.Hooks) != 1 || len(got.Hooks[0].Steps) != 3 {
		t.Fatalf("Hooks = %+v, want the round-tripped hook", got.Hooks)
	}
}

func TestDeployPlanValidate_Valid(t *testing.T) {
	if err := validDeployPlan().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a well-formed plan", err)
	}
}

func TestDeployPlanValidate_SchemaVersionMismatch(t *testing.T) {
	p := validDeployPlan()
	p.SchemaVersion = AgentSchemaVersion - 1
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for a mismatched schema version")
	}
}

func TestDeployPlanValidate_EmptyTargetDisk(t *testing.T) {
	p := validDeployPlan()
	p.TargetDisk = ""
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for an empty targetDisk")
	}
}

func TestDeployPlanValidate_DataImagesOnlyNoTargetDiskIsValid(t *testing.T) {
	p := DeployPlan{
		SchemaVersion: AgentSchemaVersion,
		RunName:       "run-1",
		RunUID:        "uid-1",
		DataImages: []DeployDataImagePlan{
			{
				ImageRef:     keziov1alpha2.NameRef{Name: "data-image"},
				TargetDisk:   "/dev/sdb",
				SfdiskScript: `{"partitiontable":{}}`,
				Slots:        []DeploySlot{{Number: 1, Device: "/dev/sdb1", Mkfs: &DeployMkfs{Filesystem: "ext4"}}},
			},
		},
		AfterDeploy: keziov1alpha2.AfterDeployPowerOff,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a dataImages-only plan with no OS image (no targetDisk, no slots)", err)
	}
}

func TestDeployPlanValidate_EmptySlots(t *testing.T) {
	p := validDeployPlan()
	p.Slots = nil
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for no slots")
	}
}

func TestDeployPlanValidate_DuplicatePartitionNumber(t *testing.T) {
	p := validDeployPlan()
	p.Slots[1].Number = p.Slots[0].Number
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for a duplicate partition number")
	}
}

func TestDeployPlanValidate_SlotContentKindInvariant(t *testing.T) {
	cases := []struct {
		name string
		slot DeploySlot
	}{
		{name: "no content kind set", slot: DeploySlot{Number: 1, Device: "/dev/x1"}},
		{
			name: "two content kinds set",
			slot: DeploySlot{
				Number: 1, Device: "/dev/x1",
				Mkfs:    &DeployMkfs{Filesystem: "ext4"},
				Torrent: &DeployTorrent{URL: "http://seeder/1.torrent", InfoHash: "abc"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validDeployPlan()
			p.Slots = []DeploySlot{tc.slot}
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate() = nil for %s, want an error", tc.name)
			}
		})
	}
}

func TestDeployPlanValidate_DataImageInvariants(t *testing.T) {
	p := validDeployPlan()
	p.DataImages[0].TargetDisk = ""
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for a dataImages entry with no targetDisk")
	}

	p = validDeployPlan()
	p.DataImages[0].Slots = nil
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for a dataImages entry with no slots")
	}
}

func TestDeployPlanValidate_HookStepInvariants(t *testing.T) {
	cases := []struct {
		name string
		step ResolvedHookStep
	}{
		{name: "builtin with no name", step: ResolvedHookStep{Type: HookStepTypeBuiltin, TimeoutSeconds: 30}},
		{name: "script with no content", step: ResolvedHookStep{Type: HookStepTypeScript, TimeoutSeconds: 30}},
		{name: "chrootScript with no content", step: ResolvedHookStep{Type: HookStepTypeChrootScript, TimeoutSeconds: 30}},
		{name: "unknown type", step: ResolvedHookStep{Type: "bogus", TimeoutSeconds: 30}},
		{name: "non-positive timeout", step: ResolvedHookStep{Type: HookStepTypeBuiltin, Builtin: "mkswap", TimeoutSeconds: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validDeployPlan()
			p.Hooks = []ResolvedHook{{Name: "h", Steps: []ResolvedHookStep{tc.step}}}
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate() = nil for %s, want an error", tc.name)
			}
		})
	}
}

func TestProgressRequestRoundTrip(t *testing.T) {
	percent := int32(42)
	bytesDone := int64(1024)
	want := ProgressRequest{
		RunName:     "run-1",
		RunUID:      "uid-1",
		Step:        "WritingContent",
		State:       ProgressStateRunning,
		Message:     "seeding",
		PercentDone: &percent,
		BytesDone:   &bytesDone,
		Timestamp:   time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ProgressRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RunName != want.RunName || got.State != want.State || *got.PercentDone != *want.PercentDone || *got.BytesDone != *want.BytesDone {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Fatalf("Timestamp = %v, want %v", got.Timestamp, want.Timestamp)
	}
}

func TestProgressResponseRoundTrip(t *testing.T) {
	want := ProgressResponse{Action: ProgressActionAbort}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ProgressResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if ProgressActionContinue != "continue" {
		t.Fatalf("ProgressActionContinue = %q, want continue", ProgressActionContinue)
	}
}
