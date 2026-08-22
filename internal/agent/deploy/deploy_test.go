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

package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

const fixtureSfdisk = `{"partitiontable":{"label":"gpt","id":"11111111-1111-1111-1111-111111111111","sectorsize":512,"partitions":[
  {"node":"/dev/loop0p1","start":2048,"size":2048,"type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
  {"node":"/dev/loop0p2","start":4096,"size":2048,"type":"0657FD6D-A4AB-43C4-84E5-0933C84B4F4F"},
  {"node":"/dev/loop0p3","start":6144,"size":2048,"type":"0FC63DAF-8483-4772-8E79-3D69D8477DE4"}
]}}`

// basicPlan returns a valid DeployPlan targeting /dev/sda with three
// slots: #1 mkfs vfat (an ESP-shaped blank slot), #2 swap, #3 torrent
// (the OS root, restored over BitTorrent).
func basicPlan() *agentapi.DeployPlan {
	return &agentapi.DeployPlan{
		SchemaVersion: agentapi.AgentSchemaVersion,
		RunName:       "node-01-abcde",
		RunUID:        "uid-1",
		MachineName:   "node-01",
		TargetDisk:    "/dev/sda",
		SfdiskScript:  fixtureSfdisk,
		Slots: []agentapi.DeploySlot{
			{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "vfat"}},
			{Number: 2, Device: "/dev/sda2", Swap: &agentapi.DeploySwap{UUID: "swap-uuid"}},
			{Number: 3, Device: "/dev/sda3", Torrent: &agentapi.DeployTorrent{URL: "http://seeder/root.torrent", InfoHash: "deadbeef"}},
		},
		AfterDeploy: keziov1alpha2.AfterDeployReboot,
	}
}

// newTestExecutor wires an Executor against fakes, returning them for
// assertions.
func newTestExecutor(ezioClient EzioClient) (*Executor, *fakeRunner, *fakeEzioLauncher, *RecordingReporter) {
	runner := newFakeRunner()
	launcher := &fakeEzioLauncher{client: ezioClient}
	fetcher := &fakeTorrentFetcher{bytes: map[string][]byte{"http://seeder/root.torrent": []byte("torrent-bytes")}}
	reporter := &RecordingReporter{}
	e := &Executor{
		Runner:       runner,
		Ezio:         launcher,
		Fetcher:      fetcher,
		Progress:     reporter,
		PollInterval: time.Millisecond,
	}
	return e, runner, launcher, reporter
}

func finishedTorrent() map[string]seeder.Torrent {
	return map[string]seeder.Torrent{
		"deadbeef": {Hash: "deadbeef", IsFinished: true, IsPaused: true, Total: 100, TotalDone: 100},
	}
}

func TestExecute_HappyPath(t *testing.T) {
	client := newFakeEzioClient([]map[string]seeder.Torrent{finishedTorrent()})
	e, runner, launcher, reporter := newTestExecutor(client)

	plan := basicPlan()
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := runner.commandNames()
	wantInOrder := []string{
		"sfdisk /dev/sda",
		"blockdev --rereadpt /dev/sda",
		"mkfs.vfat /dev/sda1",
		"mkswap --uuid swap-uuid /dev/sda2",
		"systemctl reboot",
	}
	assertSubsequence(t, calls, wantInOrder)

	if _, ok := client.added["torrent-bytes"]; !ok {
		t.Errorf("AddTorrent was not called with the fetched torrent bytes")
	}
	if !client.shutdownCalled {
		t.Errorf("ezio Shutdown was not called")
	}
	// EzioHandle.Stop is always deferred, even after a successful
	// graceful Shutdown - see EzioHandle's own doc comment on why that
	// is safe.
	if !launcher.stopCalled {
		t.Errorf("EzioHandle.Stop was never called")
	}
	if !launcher.launched {
		t.Errorf("Ezio.Launch was never called despite a torrent slot")
	}

	last := reporter.Reports[len(reporter.Reports)-1]
	if last.Step != keziov1alpha2.DeployRunPhaseSucceeded || last.State != agentapi.ProgressStateSucceeded {
		t.Errorf("final report = %+v, want the terminal Succeeded phase", last)
	}
	if last.RunName != plan.RunName || last.RunUID != plan.RunUID {
		t.Errorf("final report run identity = %q/%q, want %q/%q", last.RunName, last.RunUID, plan.RunName, plan.RunUID)
	}
}

func TestExecute_NoTorrentSlotsNeverLaunchesEzio(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, _, launcher, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{
		{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}},
	}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if launcher.launched {
		t.Errorf("Ezio.Launch was called for a plan with no torrent slots")
	}
}

func TestExecute_InvalidPlanFailsBeforeAnyCommand(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, reporter := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = nil // Validate rejects an empty slot list

	err := e.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("Execute: want an error for an invalid plan")
	}
	if len(runner.calls) != 0 {
		t.Errorf("Runner was invoked %d times for a plan that should have failed validation first", len(runner.calls))
	}
	if len(reporter.Reports) != 0 {
		t.Errorf("progress was reported for a plan invalid before Execute could establish run identity")
	}
}

func TestExecute_DeviceMismatchRefusesToDeploy(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots[0].Device = "/dev/sda9" // does not match DevicePartitionPath(disk, 1)

	err := e.Execute(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "does not match the expected path") {
		t.Fatalf("Execute error = %v, want a device-mismatch refusal", err)
	}
	for _, c := range runner.calls {
		if c.name == "sfdisk" {
			t.Errorf("sfdisk ran despite a device mismatch that should have refused the whole deploy")
		}
	}
}

func TestExecute_DiskTooSmallRefusesToDeploy(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)
	runner.blockdevSizes["/dev/sda"] = 1024 // far smaller than the fixture layout needs

	err := e.Execute(context.Background(), basicPlan())
	if err == nil || !strings.Contains(err.Error(), "smaller than") {
		t.Fatalf("Execute error = %v, want a disk-too-small refusal", err)
	}
}

func TestExecute_TwoDisksTargetingSameDeviceIsRejected(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, _, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.DataImages = []agentapi.DeployDataImagePlan{{
		ImageRef:     keziov1alpha2.NameRef{Name: "data-image"},
		TargetDisk:   plan.TargetDisk,
		SfdiskScript: fixtureSfdisk,
		Slots:        []agentapi.DeploySlot{{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}},
	}}

	err := e.Execute(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "targeted by more than one image") {
		t.Fatalf("Execute error = %v, want a duplicate-disk refusal", err)
	}
}

func TestExecute_DataImageDiskIsWritten(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}}
	plan.DataImages = []agentapi.DeployDataImagePlan{{
		ImageRef:     keziov1alpha2.NameRef{Name: "data-image"},
		TargetDisk:   "/dev/sdb",
		SfdiskScript: fixtureSfdisk,
		Slots:        []agentapi.DeploySlot{{Number: 1, Device: "/dev/sdb1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}},
	}}

	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := runner.commandNames()
	if !slices.Contains(calls, "sfdisk /dev/sdb") {
		t.Errorf("data image disk /dev/sdb was never partitioned; calls: %v", calls)
	}
	if !slices.Contains(calls, "mkfs.ext4 /dev/sdb1") {
		t.Errorf("data image slot /dev/sdb1 was never made; calls: %v", calls)
	}
}

// TestExecute_DataImagesOnlyPlanNoOSImage covers the plan shape
// agentapi.DeployPlan.Validate documents but basicPlan-derived tests
// never exercise: TargetDisk/SfdiskScript/Slots all empty, only
// DataImages set. diskPlans must not synthesize an "OS image" disk
// plan out of that empty state - an earlier version did, and its
// disk "" failed validate's "/dev/" prefix check on every such
// deploy.
func TestExecute_DataImagesOnlyPlanNoOSImage(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, launcher, reporter := newTestExecutor(client)

	plan := &agentapi.DeployPlan{
		SchemaVersion: agentapi.AgentSchemaVersion,
		RunName:       "node-01-abcde",
		RunUID:        "uid-1",
		MachineName:   "node-01",
		DataImages: []agentapi.DeployDataImagePlan{{
			ImageRef:     keziov1alpha2.NameRef{Name: "data-image"},
			TargetDisk:   "/dev/sdb",
			SfdiskScript: fixtureSfdisk,
			Slots:        []agentapi.DeploySlot{{Number: 1, Device: "/dev/sdb1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}},
		}},
		AfterDeploy: keziov1alpha2.AfterDeployPowerOff,
	}

	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := runner.commandNames()
	if !slices.Contains(calls, "sfdisk /dev/sdb") {
		t.Errorf("data image disk /dev/sdb was never partitioned; calls: %v", calls)
	}
	if !slices.Contains(calls, "mkfs.ext4 /dev/sdb1") {
		t.Errorf("data image slot /dev/sdb1 was never made; calls: %v", calls)
	}
	if launcher.launched {
		t.Error("ezio was launched for a plan with no torrent slots")
	}
	last := reporter.Reports[len(reporter.Reports)-1]
	if last.State != agentapi.ProgressStateSucceeded {
		t.Fatalf("last report state = %q, want succeeded", last.State)
	}
}

func TestExecute_ContextCancelledDuringSeedingReportsFailed(t *testing.T) {
	// GetTorrentStatus always answers "still seeding": the poll loop
	// never sees allStopped and only exits once ctx is cancelled.
	client := newFakeEzioClient([]map[string]seeder.Torrent{{
		"deadbeef": {Hash: "deadbeef", IsFinished: false, Total: 100, TotalDone: 10},
	}})
	e, _, _, reporter := newTestExecutor(client)
	e.PollInterval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := e.Execute(ctx, basicPlan())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want context.DeadlineExceeded", err)
	}

	last := reporter.Reports[len(reporter.Reports)-1]
	if last.State != agentapi.ProgressStateFailed {
		t.Errorf("final report state = %q, want %q", last.State, agentapi.ProgressStateFailed)
	}
	if last.Message == "" {
		t.Errorf("final failed report carries no message")
	}
}

func TestExecute_AfterDeployPowerOff(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4"}}}
	plan.AfterDeploy = keziov1alpha2.AfterDeployPowerOff

	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := runner.commandNames()
	if !slices.Contains(calls, "systemctl poweroff") {
		t.Errorf("systemctl poweroff was not invoked; calls: %v", calls)
	}
	if slices.Contains(calls, "systemctl reboot") {
		t.Errorf("systemctl reboot was invoked despite AfterDeployPowerOff; calls: %v", calls)
	}
}

func TestExecute_MkfsPassesLabelAndUUID(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{
		{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "ext4", Label: "root", UUID: "fs-uuid"}},
	}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !slices.Contains(runner.commandNames(), "mkfs.ext4 -L root -U fs-uuid /dev/sda1") {
		t.Errorf("mkfs did not carry label/uuid flags; calls: %v", runner.commandNames())
	}
}

func TestExecute_MkfsXFSUUIDUsesDashM(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{
		{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "xfs", UUID: "fs-uuid"}},
	}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !slices.Contains(runner.commandNames(), "mkfs.xfs -m uuid=fs-uuid /dev/sda1") {
		t.Errorf("mkfs.xfs did not carry -m uuid=; calls: %v", runner.commandNames())
	}
}

func TestExecute_MkfsVfatUsesDashN(t *testing.T) {
	client := newFakeEzioClient(nil)
	e, runner, _, _ := newTestExecutor(client)

	plan := basicPlan()
	plan.Slots = []agentapi.DeploySlot{
		{Number: 1, Device: "/dev/sda1", Mkfs: &agentapi.DeployMkfs{Filesystem: "vfat", Label: "ESP"}},
	}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !slices.Contains(runner.commandNames(), "mkfs.vfat -n ESP /dev/sda1") {
		t.Errorf("mkfs.vfat did not use -n for its label; calls: %v", runner.commandNames())
	}
}

func TestWriteConsole(t *testing.T) {
	var buf strings.Builder
	e := &Executor{Console: &buf}
	e.writeConsole("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("writeConsole did not write to Console; got %q", buf.String())
	}
}

func TestWriteConsole_NilIsNoop(t *testing.T) {
	e := &Executor{}
	e.writeConsole("hello") // must not panic
}

// assertSubsequence fails the test unless want appears, in order (not
// necessarily contiguously), within got.
func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("commands %v do not contain the subsequence %v (matched %d/%d)", got, want, i, len(want))
	}
}
