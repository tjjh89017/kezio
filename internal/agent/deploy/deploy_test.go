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

package deploy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// fakeCall records one Runner.Run invocation.
type fakeCall struct {
	stdin string
	name  string
	args  []string
}

func (c fakeCall) String() string {
	return fmt.Sprintf("%s %s", c.name, strings.Join(c.args, " "))
}

// fakeRunner is a Runner recording every call in order. blockdevSizes
// answers "blockdev --getsize64 <disk>"; every other command succeeds
// with empty output unless errs names it.
type fakeRunner struct {
	mu             sync.Mutex
	calls          []fakeCall
	blockdevSizes  map[string]int64
	errs           map[string]error // keyed by "name args..."
	blockdevErrors map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		blockdevSizes:  map[string]int64{},
		errs:           map[string]error{},
		blockdevErrors: map[string]error{},
	}
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := fakeCall{stdin: string(stdin), name: name, args: args}
	f.calls = append(f.calls, call)

	key := call.String()
	if err, ok := f.errs[key]; ok {
		return nil, err
	}

	if name == "blockdev" && len(args) == 2 && args[0] == "--getsize64" {
		disk := args[1]
		if err, ok := f.blockdevErrors[disk]; ok {
			return nil, err
		}
		size, ok := f.blockdevSizes[disk]
		if !ok {
			size = 100 << 30 // 100 GiB, comfortably large by default
		}
		return []byte(fmt.Sprintf("%d\n", size)), nil
	}
	return nil, nil
}

func (f *fakeRunner) commandNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.String()
	}
	return names
}

// fakeEzioClient is an in-memory EzioClient recording AddTorrent calls
// and serving scripted GetTorrentStatus responses, one slice entry
// consumed per poll (the last entry repeats once exhausted).
type fakeEzioClient struct {
	mu             sync.Mutex
	added          map[string]string // hash -> save_path
	statusSequence []map[string]seeder.Torrent
	statusCalls    int
	paused         []string
	shutdownCalled bool
}

func newFakeEzioClient(statusSequence []map[string]seeder.Torrent) *fakeEzioClient {
	return &fakeEzioClient{added: map[string]string{}, statusSequence: statusSequence}
}

func (f *fakeEzioClient) AddTorrent(_ context.Context, torrent []byte, savePath string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[string(torrent)] = savePath
	return nil
}

func (f *fakeEzioClient) GetTorrentStatus(_ context.Context, _ []string) (map[string]seeder.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.statusCalls
	if idx >= len(f.statusSequence) {
		idx = len(f.statusSequence) - 1
	}
	f.statusCalls++
	return f.statusSequence[idx], nil
}

func (f *fakeEzioClient) PauseTorrent(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, hash)
	return nil
}

func (f *fakeEzioClient) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalled = true
	return nil
}

type fakeLauncher struct {
	client    EzioClient
	stopped   bool
	launchErr error
}

func (f *fakeLauncher) Launch(context.Context, *keziov1alpha1.MachineEzioTuning) (EzioHandle, error) {
	if f.launchErr != nil {
		return EzioHandle{}, f.launchErr
	}
	return EzioHandle{
		Client: f.client,
		Stop:   func() error { f.stopped = true; return nil },
	}, nil
}

type fakeProgressReporter struct {
	mu     sync.Mutex
	events [][]agentapi.PartitionProgress
}

func (f *fakeProgressReporter) ReportProgress(_ context.Context, partitions []agentapi.PartitionProgress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]agentapi.PartitionProgress(nil), partitions...)
	f.events = append(f.events, cp)
	return nil
}

func (f *fakeProgressReporter) last() []agentapi.PartitionProgress {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return nil
	}
	return f.events[len(f.events)-1]
}

// samplePlan builds a one-disk plan with one ESP (blank/vfat), one swap
// partition, and one content partition - exercising all three
// PlanPartition classifications in one image plan.
func samplePlan(disk string) *agentapi.DeployPlan {
	return &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath(disk, 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
				{Number: 2, Device: agentapi.DevicePartitionPath(disk, 2), Role: keziov1alpha1.PartitionRoleData, FSType: "ext4", InfoHash: "deadbeef", Torrent: []byte("torrent-bytes")},
				{Number: 3, Device: agentapi.DevicePartitionPath(disk, 3), Role: keziov1alpha1.PartitionRoleSwap, SwapUUID: "11111111-1111-1111-1111-111111111111"},
			},
		},
		AfterDeploy: keziov1alpha1.AfterDeployReboot,
	}
}

func TestExecute_CommandSequenceForMixedPartitionTypes(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	ezio := newFakeEzioClient([]map[string]seeder.Torrent{
		{"deadbeef": {IsFinished: true, IsPaused: true, TotalDone: 100, Total: 100}},
	})
	launcher := &fakeLauncher{client: ezio}
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: launcher, Progress: progress, PollInterval: time.Millisecond}
	if err := e.Execute(context.Background(), samplePlan(disk)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := runner.commandNames()
	wantInOrder := []string{
		"blockdev --getsize64 " + disk, // safety check, before any write
		"sfdisk " + disk,
		"partprobe " + disk,
		"mkfs.vfat " + agentapi.DevicePartitionPath(disk, 1),
		"mkswap --uuid 11111111-1111-1111-1111-111111111111 " + agentapi.DevicePartitionPath(disk, 3),
	}
	idx := 0
	for _, want := range wantInOrder {
		for idx < len(calls) && calls[idx] != want {
			idx++
		}
		if idx == len(calls) {
			t.Fatalf("command %q not found in order in %v", want, calls)
		}
		idx++
	}

	if launcher.client == nil {
		t.Fatal("ezio was never launched despite a content partition in the plan")
	}
	if got, want := ezio.added["torrent-bytes"], agentapi.DevicePartitionPath(disk, 2); got != want {
		t.Errorf("AddTorrent save_path = %q, want %q (the raw partition device, not a -F content directory)", got, want)
	}
	if !ezio.shutdownCalled {
		t.Error("Shutdown was never called once every torrent finished and paused")
	}
	if !launcher.stopped {
		t.Error("EzioHandle.Stop was never called as the deferred backstop")
	}

	final := progress.last()
	if len(final) != 3 {
		t.Fatalf("final progress has %d partitions, want 3", len(final))
	}
	for _, p := range final {
		if p.Phase != agentapi.PartitionPhaseDone || p.PercentDone != 100 {
			t.Errorf("final progress for partition %d = %+v, want done/100", p.Number, p)
		}
	}
}

func TestExecute_NoContentPartitionsNeverLaunchesEzio(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	launcher := &fakeLauncher{launchErr: fmt.Errorf("Launch must not be called")}

	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "data-only"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath(disk, 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}

	e := &Executor{Runner: runner, Ezio: launcher}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestExecute_RefusesOnDeviceMismatch(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				// Wrong device path for partition 1 - should never
				// happen from a real controller, but a corrupted or
				// buggy plan must be refused, not trusted.
				{Number: 1, Device: "/dev/sdX99", Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	err := e.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("Execute: got nil error for a plan whose Device does not match its own Disk/Number, want a refusal")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Runner was invoked %d times despite the plan being refused; want zero commands issued", len(runner.calls))
	}
}

func TestExecute_RefusesWhenDiskTooSmall(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	// fixtureSfdiskJSON's furthest partition ends at (6144+2048)*512
	// bytes; report a disk far smaller than that.
	runner.blockdevSizes[disk] = 1024

	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath(disk, 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	err := e.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("Execute: got nil error for a disk smaller than the layout requires, want a refusal")
	}
	for _, c := range runner.calls {
		if c.name == "sfdisk" {
			t.Fatalf("sfdisk was invoked despite the disk being too small: %v", c)
		}
	}
}

func TestExecute_RefusesWhenTwoImagesTargetSameDisk(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	ip := agentapi.ImageDeployPlan{
		ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
		Disk:       disk,
		SfdiskJSON: fixtureSfdiskJSON,
		Partitions: []agentapi.PlanPartition{
			{Number: 1, Device: agentapi.DevicePartitionPath(disk, 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
		},
	}
	plan := &agentapi.DeployPlan{
		OS:         &ip,
		DataImages: []agentapi.ImageDeployPlan{ip},
	}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	err := e.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("Execute: got nil error for two image plans sharing a disk, want a refusal")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Runner was invoked %d times despite the plan being refused", len(runner.calls))
	}
}

func TestExecute_RefusesNonDevPath(t *testing.T) {
	runner := newFakeRunner()
	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       "not-a-device-path",
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath("not-a-device-path", 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	if err := e.Execute(context.Background(), plan); err == nil {
		t.Fatal("Execute: got nil error for a disk path outside /dev/, want a refusal")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Runner was invoked %d times despite the plan being refused", len(runner.calls))
	}
}

func TestExecute_StopPolicyPausesBeforeShutdown(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	// First poll: finished but still within the idle window and under
	// the upload ratio - must not pause yet. Second poll: idle long
	// enough - must pause, and Shutdown follows once paused+finished is
	// observed.
	ezio := newFakeEzioClient([]map[string]seeder.Torrent{
		{"deadbeef": {IsFinished: true, IsPaused: false, TotalDone: 100, Total: 100, TotalPayloadUpload: 10, FinishedTime: 1, LastUpload: 1}},
		{"deadbeef": {IsFinished: true, IsPaused: false, TotalDone: 100, Total: 100, TotalPayloadUpload: 10, FinishedTime: 20, LastUpload: 20}},
		{"deadbeef": {IsFinished: true, IsPaused: true, TotalDone: 100, Total: 100, TotalPayloadUpload: 10, FinishedTime: 25, LastUpload: 25}},
	})
	launcher := &fakeLauncher{client: ezio}

	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 2, Device: agentapi.DevicePartitionPath(disk, 2), Role: keziov1alpha1.PartitionRoleData, InfoHash: "deadbeef", Torrent: []byte("t")},
			},
		},
	}

	e := &Executor{Runner: runner, Ezio: launcher, PollInterval: time.Millisecond}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(ezio.paused) == 0 {
		t.Fatal("PauseTorrent was never called")
	}
	if !ezio.shutdownCalled {
		t.Error("Shutdown was never called")
	}
}
