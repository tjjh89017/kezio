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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/utils/ptr"

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
	// outputs, keyed by "name args..." (fakeCall.String()), answers a
	// call with canned stdout instead of the empty-success default -
	// used to script efibootmgr's listing output for finalize tests.
	outputs map[string][]byte
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		blockdevSizes:  map[string]int64{},
		errs:           map[string]error{},
		blockdevErrors: map[string]error{},
		outputs:        map[string][]byte{},
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
	if out, ok := f.outputs[key]; ok {
		return out, nil
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
	// A plain "mount <device> <target>" (never "--bind", which always
	// carries 3 args) stands in for a real mount of whatever file system
	// ensureRemovableFallback or runChrootScriptStep just mounted. This
	// fake never touches a real block device, so it seeds target with a
	// stub EFI/BOOT/BOOTX64.EFI - the layout ensureRemovableFallback
	// finds already satisfied and leaves alone - unless a test overrides
	// this by calling target-specific setup before Execute runs.
	if name == "mount" && len(args) == 2 {
		target := args[1]
		dir := filepath.Join(target, "EFI", "BOOT")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "BOOTX64.EFI"), []byte("stub"), 0o644)
		}
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
	addedTuning    map[string][2]int32
	statusSequence []map[string]seeder.Torrent
	statusCalls    int
	paused         []string
	shutdownCalled bool
}

func newFakeEzioClient(statusSequence []map[string]seeder.Torrent) *fakeEzioClient {
	return &fakeEzioClient{added: map[string]string{}, addedTuning: map[string][2]int32{}, statusSequence: statusSequence}
}

func (f *fakeEzioClient) AddTorrent(_ context.Context, torrent []byte, savePath string, _ bool, maxUploads, maxConnections int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[string(torrent)] = savePath
	f.addedTuning[string(torrent)] = [2]int32{maxUploads, maxConnections}
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
	events []agentapi.ProgressRequest
}

func (f *fakeProgressReporter) ReportProgress(_ context.Context, req agentapi.ProgressRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := req
	cp.Partitions = append([]agentapi.PartitionProgress(nil), req.Partitions...)
	f.events = append(f.events, cp)
	return nil
}

func (f *fakeProgressReporter) last() []agentapi.PartitionProgress {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return nil
	}
	return f.events[len(f.events)-1].Partitions
}

// steps returns the Step value of every report, in order, so a test can
// assert the whole-plan step machine progressed through the expected
// sequence and ended on the terminal success step.
func (f *fakeProgressReporter) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	steps := make([]string, len(f.events))
	for i, e := range f.events {
		steps[i] = e.Step
	}
	return steps
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
		"blockdev --rereadpt " + disk,
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
	if got, want := ezio.addedTuning["torrent-bytes"], [2]int32{seeder.DefaultMaxUploads, seeder.DefaultMaxConnections}; got != want {
		t.Errorf("AddTorrent (max_uploads, max_connections) = %v, want %v (plan.Ezio was nil, so the built-in defaults apply)", got, want)
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

// TestExecute_PlanEzioTuningReachesAddTorrent verifies that
// plan.Ezio's MaxUploads/MaxConnections (already resolved by
// internal/agentserver's buildDeployPlan - cluster default merged with
// any Machine.spec.ezio override) are the exact values applyPartition
// hands to AddTorrent, not the package's own built-in defaults.
func TestExecute_PlanEzioTuningReachesAddTorrent(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	ezio := newFakeEzioClient([]map[string]seeder.Torrent{
		{"deadbeef": {IsFinished: true, IsPaused: true, TotalDone: 100, Total: 100}},
	})
	launcher := &fakeLauncher{client: ezio}

	plan := samplePlan(disk)
	plan.Ezio = &keziov1alpha1.MachineEzioTuning{
		MaxUploads:     ptr.To(int32(7)),
		MaxConnections: ptr.To(int32(9)),
	}

	e := &Executor{Runner: runner, Ezio: launcher, PollInterval: time.Millisecond}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got, want := ezio.addedTuning["torrent-bytes"], [2]int32{7, 9}; got != want {
		t.Errorf("AddTorrent (max_uploads, max_connections) = %v, want %v (plan.Ezio's own values)", got, want)
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

// espOnlyPlanDisk is the fixed target disk every espOnlyPlan uses -
// finalize tests only ever need one disk, so it is a constant rather
// than a parameter every call site would just repeat.
const espOnlyPlanDisk = "/dev/nvme0n1"

// espOnlyPlan builds a one-disk, ESP-only plan (no content, no swap) so
// finalize tests exercise efibootmgr and the terminal reboot/poweroff
// action without ezio in the picture at all.
func espOnlyPlan(afterDeploy string, growLastPartition bool) *agentapi.DeployPlan {
	disk := espOnlyPlanDisk
	return &agentapi.DeployPlan{
		MachineName: "node-01",
		AfterDeploy: afterDeploy,
		OS: &agentapi.ImageDeployPlan{
			ImageRef:          keziov1alpha1.NameRef{Name: "os-image"},
			Disk:              disk,
			SfdiskJSON:        fixtureSfdiskJSON,
			GrowLastPartition: growLastPartition,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: agentapi.DevicePartitionPath(disk, 1), Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}
}

func TestExecute_FinalizeCreatesUEFIBootEntryAndRemovesPriorOne(t *testing.T) {
	runner := newFakeRunner()
	// A prior kezio-created entry for this same machine already exists
	// (Boot0003), alongside an unrelated entry (Boot0001) that must be
	// left alone.
	runner.outputs["efibootmgr "] = []byte(
		"BootCurrent: 0001\n" +
			"Boot0001* Windows Boot Manager\n" +
			"Boot0003* kezio:node-01\n",
	)

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := runner.commandNames()
	wantInOrder := []string{
		"efibootmgr ",
		"efibootmgr --bootnum 0003 --delete-bootnum",
		"efibootmgr --create --disk /dev/nvme0n1 --part 1 --loader \\EFI\\BOOT\\BOOTX64.EFI --label kezio:node-01",
		"systemctl reboot",
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
	for _, c := range calls {
		if c == "efibootmgr --bootnum 0001 --delete-bootnum" {
			t.Fatalf("deleted the unrelated Boot0001 entry, want only the kezio:node-01 entry removed: %v", calls)
		}
	}
}

func TestExecute_GrowLastPartitionFlagOffIssuesNoGrowCommands(t *testing.T) {
	runner := newFakeRunner()

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, c := range runner.commandNames() {
		if strings.HasPrefix(c, "growpart") || strings.Contains(c, "resize") {
			t.Fatalf("GrowLastPartition was false but a grow/resize command ran: %q (all calls: %v)", c, runner.commandNames())
		}
	}
}

func TestExecute_GrowLastPartitionFlagOnGrowsPartitionAndFileSystem(t *testing.T) {
	runner := newFakeRunner()

	plan := espOnlyPlan(keziov1alpha1.AfterDeployReboot, true)
	// vfat has no known online-resize tool in fsResizeCommand, so switch
	// the ESP's FSType to ext4 to also exercise the file system resize
	// half of growLastPartition.
	plan.OS.Partitions[0].FSType = "ext4"

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	if err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := runner.commandNames()
	wantInOrder := []string{
		"growpart /dev/nvme0n1 1",
		"resize2fs " + agentapi.DevicePartitionPath(espOnlyPlanDisk, 1),
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
}

func TestExecute_AfterDeploySelectsRebootOrPowerOff(t *testing.T) {
	cases := []struct {
		afterDeploy string
		wantCommand string
	}{
		{keziov1alpha1.AfterDeployReboot, "systemctl reboot"},
		{keziov1alpha1.AfterDeployPowerOff, "systemctl poweroff"},
		{"", "systemctl reboot"}, // empty AfterDeploy defaults to reboot
	}

	for _, tc := range cases {
		t.Run(tc.afterDeploy, func(t *testing.T) {
			runner := newFakeRunner()
			e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
			if err := e.Execute(context.Background(), espOnlyPlan(tc.afterDeploy, false)); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			calls := runner.commandNames()
			if calls[len(calls)-1] != tc.wantCommand {
				t.Fatalf("last command = %q, want %q (all calls: %v)", calls[len(calls)-1], tc.wantCommand, calls)
			}
			for _, other := range []string{"systemctl reboot", "systemctl poweroff"} {
				if other == tc.wantCommand {
					continue
				}
				for _, c := range calls {
					if c == other {
						t.Fatalf("issued %q in addition to %q", other, tc.wantCommand)
					}
				}
			}
		})
	}
}

func TestExecute_NeverChrootsOrRunsUpdateInitramfs(t *testing.T) {
	runner := newFakeRunner()

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, c := range runner.calls {
		if c.name == "chroot" || c.name == "update-initramfs" {
			t.Fatalf("finalize ran %q - it must never chroot or regenerate the initramfs (see the package doc comment)", c.name)
		}
	}
}

func TestExecute_ReportsFinalizeAndTerminalSteps(t *testing.T) {
	runner := newFakeRunner()
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	steps := progress.steps()
	if len(steps) == 0 {
		t.Fatal("no progress reports were sent")
	}
	if last := steps[len(steps)-1]; last != agentapi.DeployStepRebootingToDisk {
		t.Fatalf("last reported step = %q, want the terminal %q", last, agentapi.DeployStepRebootingToDisk)
	}

	sawFinalizing := false
	for _, s := range steps {
		if s == agentapi.DeployStepFinalizing {
			sawFinalizing = true
		}
	}
	if !sawFinalizing {
		t.Fatalf("never reported %q among steps %v", agentapi.DeployStepFinalizing, steps)
	}
}

// TestExecute_TerminalReportCarriesFinalizeBootSummary exercises the gap
// attempt 38's post-mortem found: the agent's own log output never
// reaches the deployed machine's serial console, and the machine is gone
// the moment it reboots into the deployed disk, so the terminal progress
// report is the only place finalize's boot-entry and removable-fallback
// outcome (and the resulting efibootmgr listing) survives to be read
// after the fact.
func TestExecute_TerminalReportCarriesFinalizeBootSummary(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["efibootmgr "] = []byte(
		"BootCurrent: 0001\n" +
			"BootOrder: 0002,0001,0000\n" +
			"Boot0001* kezio:node-01\n",
	)
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	last := progress.events[len(progress.events)-1]
	if last.Step != agentapi.DeployStepRebootingToDisk {
		t.Fatalf("last step = %q, want %q", last.Step, agentapi.DeployStepRebootingToDisk)
	}
	if !strings.Contains(last.StepMessage, "removable fallback already present") {
		t.Fatalf("StepMessage = %q, want it to mention the removable fallback outcome", last.StepMessage)
	}
	if !strings.Contains(last.StepMessage, "BootOrder: 0002,0001,0000") {
		t.Fatalf("StepMessage = %q, want it to carry the post-finalize efibootmgr listing", last.StepMessage)
	}
}

// TestExecute_TerminalReportAlsoEchoesBootSummaryToConsole exercises
// Executor.Console: the same finalize boot summary the terminal
// progress report's StepMessage carries must also reach a non-nil
// Console, since that is the channel meant to survive the report itself
// getting lost to a network or controller-side race (see Console's
// field doc comment).
func TestExecute_TerminalReportAlsoEchoesBootSummaryToConsole(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["efibootmgr "] = []byte(
		"BootCurrent: 0001\n" +
			"BootOrder: 0002,0001,0000\n" +
			"Boot0001* kezio:node-01\n",
	)
	progress := &fakeProgressReporter{}
	var console bytes.Buffer

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress, Console: &console}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(console.String(), "removable fallback already present") {
		t.Fatalf("console output = %q, want it to mention the removable fallback outcome", console.String())
	}
	if !strings.Contains(console.String(), "BootOrder: 0002,0001,0000") {
		t.Fatalf("console output = %q, want it to carry the post-finalize efibootmgr listing", console.String())
	}
}

// TestExecute_NilConsoleIsSafe exercises the default (Console left nil,
// as every other test in this file does): Execute must run to
// completion exactly as it did before Console existed, never
// dereferencing it.
func TestExecute_NilConsoleIsSafe(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["efibootmgr "] = []byte("BootCurrent: 0001\n")
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress}
	if err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestExecute_ReportsFailedStepOnError exercises Execute failing partway
// through (a Runner command returning an error), the case that matters to
// the controller: without a terminal step report, the controller's
// agent-driven Provision would poll MachineConditionProvisioningProgress
// forever, never reaching an intermediate step it also does not
// recognize as failure. Execute must report DeployStepFailed with the
// error detail as its last progress event before returning the error.
func TestExecute_ReportsFailedStepOnError(t *testing.T) {
	runner := newFakeRunner()
	runner.errs["sfdisk /dev/nvme0n1"] = fmt.Errorf("injected sfdisk failure")
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress}
	err := e.Execute(context.Background(), espOnlyPlan(keziov1alpha1.AfterDeployReboot, false))
	if err == nil {
		t.Fatal("Execute: got nil error, want the injected sfdisk failure")
	}

	steps := progress.steps()
	if len(steps) == 0 {
		t.Fatal("no progress reports were sent")
	}
	if last := steps[len(steps)-1]; last != agentapi.DeployStepFailed {
		t.Fatalf("last reported step = %q, want the terminal failure step %q", last, agentapi.DeployStepFailed)
	}
	lastMsg := progress.events[len(progress.events)-1].StepMessage
	if !strings.Contains(lastMsg, "injected sfdisk failure") {
		t.Fatalf("StepMessage = %q, want it to mention the underlying failure", lastMsg)
	}
}

// TestExecute_ReportsFailedStepOnValidationRefusal exercises the case
// where Execute never gets past validate: even then, Execute must report
// DeployStepFailed so the controller does not poll forever waiting for a
// deploy that was refused before writing a single byte.
func TestExecute_ReportsFailedStepOnValidationRefusal(t *testing.T) {
	disk := "/dev/nvme0n1"
	runner := newFakeRunner()
	plan := &agentapi.DeployPlan{
		OS: &agentapi.ImageDeployPlan{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			Disk:       disk,
			SfdiskJSON: fixtureSfdiskJSON,
			Partitions: []agentapi.PlanPartition{
				{Number: 1, Device: "/dev/sdX99", Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat"},
			},
		},
	}
	progress := &fakeProgressReporter{}

	e := &Executor{Runner: runner, Ezio: &fakeLauncher{}, Progress: progress}
	if err := e.Execute(context.Background(), plan); err == nil {
		t.Fatal("Execute: got nil error, want a validation refusal")
	}

	steps := progress.steps()
	if len(steps) == 0 || steps[len(steps)-1] != agentapi.DeployStepFailed {
		t.Fatalf("steps = %v, want the last one to be %q", steps, agentapi.DeployStepFailed)
	}
}
