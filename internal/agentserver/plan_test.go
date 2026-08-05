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

package agentserver

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/store"
)

const testTrackerURL = "http://tracker.example/announce"

// newPlanTestClient builds a fake client seeded with objs, using a
// scheme that knows about both kezio's own types (Machine, Image) and
// core/v1 (ConfigMap).
func newPlanTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (client-go): %v", err)
	}
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (kezio): %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha1.Machine{}, MachineNameIndexField, IndexMachineName).
		WithObjects(objs...).
		Build()
}

// writeFixtureContent writes a minimal, valid content directory (a
// torrent.info plus its one extent file, nested under content/ as
// store.LoadContentDirTorrentInfo requires) under a fresh temp store
// root, and returns the root and the content's info hash - the same
// fixture shape internal/controller's seeder tests use.
func writeFixtureContent(t *testing.T) (root string, hash store.InfoHash) {
	t.Helper()
	root = t.TempDir()

	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 100,
		Extents:     []store.Extent{{Offset: 0, Length: store.PieceSize}},
		PieceHashes: []store.PieceHash{{1, 2, 3, 4, 5}},
	}
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	dir := store.ContentDir(root, hash)
	if err := os.MkdirAll(store.ContentDataDir(dir), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	infoFile, err := os.Create(store.ContentTorrentInfoPath(root, hash)) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("create torrent.info: %v", err)
	}
	if err := store.WriteTorrentInfo(infoFile, info); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}
	if err := infoFile.Close(); err != nil {
		t.Fatalf("close torrent.info: %v", err)
	}
	extentPath := store.ContentExtentPath(root, hash, 0)
	if err := os.WriteFile(extentPath, make([]byte, store.PieceSize), 0o644); err != nil { //nolint:gosec // test fixture path
		t.Fatalf("write extent file: %v", err)
	}
	return root, hash
}

// readyImage builds a Ready Image object named name, with a layout
// ConfigMap named name+"-cm" holding sfdiskJSON, and the given
// partitions. It returns the Image and the ConfigMap, both ready to seed
// a fake client with.
func readyImage(name, sfdiskJSON string, partitions []keziov1alpha1.ImagePartitionStatus) (*keziov1alpha1.Image, *corev1.ConfigMap) {
	cmName := name + "-cm"
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha1.ImageSpec{
			Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatRaw},
		},
		Status: keziov1alpha1.ImageStatus{
			State: keziov1alpha1.ImageStateReady,
			Disk: &keziov1alpha1.ImageDiskStatus{
				PartitionTable: keziov1alpha1.PartitionTableGPT,
				LayoutRef:      &keziov1alpha1.NameRef{Name: cmName},
			},
			Partitions: partitions,
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: "default"},
		Data:       map[string]string{sfdiskJSONKey: sfdiskJSON},
	}
	return image, cm
}

func TestBuildDeployPlan_OSImageWithContentSwapAndBlankPartitions(t *testing.T) {
	storeRoot, hash := writeFixtureContent(t)

	partitions := []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat", InfoHash: hash.String()},
		{Number: 2, Role: keziov1alpha1.PartitionRoleSwap, UUID: "11111111-1111-1111-1111-111111111111"},
		{Number: 3, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"}, // blank: no infoHash, no uuid
	}
	image, cm := readyImage("os-image", `{"partitiontable":{"label":"gpt"}}`, partitions)

	ezio := &keziov1alpha1.MachineEzioTuning{CacheSizeMB: int32Ptr(256)}
	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			ImageRef:    &keziov1alpha1.NameRef{Name: "os-image"},
			Ezio:        ezio,
			AfterDeploy: keziov1alpha1.AfterDeployPowerOff,
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("buildDeployPlan returned a nil plan; want a ready plan")
	}

	if plan.AfterDeploy != keziov1alpha1.AfterDeployPowerOff {
		t.Errorf("AfterDeploy = %q, want %q", plan.AfterDeploy, keziov1alpha1.AfterDeployPowerOff)
	}
	if plan.MachineName != "node-01" {
		t.Errorf("MachineName = %q, want %q (the agent's finalize step needs it for a stable UEFI boot entry label)", plan.MachineName, "node-01")
	}
	if plan.Ezio == nil || plan.Ezio.CacheSizeMB == nil || *plan.Ezio.CacheSizeMB != 256 {
		t.Errorf("Ezio = %+v, want CacheSizeMB=256", plan.Ezio)
	}
	if plan.OS == nil {
		t.Fatal("plan.OS is nil")
	}
	if plan.OS.Disk != "/dev/nvme0n1" {
		t.Errorf("OS.Disk = %q, want /dev/nvme0n1", plan.OS.Disk)
	}
	if plan.OS.SfdiskJSON != `{"partitiontable":{"label":"gpt"}}` {
		t.Errorf("OS.SfdiskJSON = %q", plan.OS.SfdiskJSON)
	}
	if len(plan.OS.Partitions) != 3 {
		t.Fatalf("len(OS.Partitions) = %d, want 3", len(plan.OS.Partitions))
	}

	esp := plan.OS.Partitions[0]
	if esp.Device != "/dev/nvme0n1p1" {
		t.Errorf("esp.Device = %q, want /dev/nvme0n1p1", esp.Device)
	}
	if esp.InfoHash != hash.String() {
		t.Errorf("esp.InfoHash = %q, want %q", esp.InfoHash, hash.String())
	}
	if len(esp.Torrent) == 0 {
		t.Error("esp.Torrent is empty; want built .torrent bytes")
	}
	if esp.SwapUUID != "" {
		t.Errorf("esp.SwapUUID = %q, want empty for a content partition", esp.SwapUUID)
	}

	swap := plan.OS.Partitions[1]
	if swap.SwapUUID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("swap.SwapUUID = %q", swap.SwapUUID)
	}
	if swap.InfoHash != "" || len(swap.Torrent) != 0 {
		t.Errorf("swap partition carries content fields: infoHash=%q torrent=%d bytes", swap.InfoHash, len(swap.Torrent))
	}
	if swap.Device != "/dev/nvme0n1p2" {
		t.Errorf("swap.Device = %q, want /dev/nvme0n1p2", swap.Device)
	}

	blank := plan.OS.Partitions[2]
	if blank.InfoHash != "" || blank.SwapUUID != "" {
		t.Errorf("blank partition should have neither InfoHash nor SwapUUID set: %+v", blank)
	}
	if blank.FSType != "ext4" {
		t.Errorf("blank.FSType = %q, want ext4 (mkfs target)", blank.FSType)
	}
	if blank.Device != "/dev/nvme0n1p3" {
		t.Errorf("blank.Device = %q, want /dev/nvme0n1p3", blank.Device)
	}
}

func TestBuildDeployPlan_MultiDataImageUsesNonNVMeDeviceNaming(t *testing.T) {
	storeRoot := t.TempDir()

	image1, cm1 := readyImage("data-1", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
	})
	image2, cm2 := readyImage("data-2", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "xfs"},
	})

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-02", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			DataImages: []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: "data-1"}},
				{ImageRef: keziov1alpha1.NameRef{Name: "data-2"}},
			},
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				DataImages: []keziov1alpha1.MachineProvisionedImage{
					{ImageRef: keziov1alpha1.NameRef{Name: "data-1"}, TargetDisk: "/dev/sda"},
					{ImageRef: keziov1alpha1.NameRef{Name: "data-2"}, TargetDisk: "/dev/sdb"},
				},
			},
		},
	}

	c := newPlanTestClient(t, image1, cm1, image2, cm2, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("buildDeployPlan returned a nil plan; want a ready plan")
	}
	if plan.OS != nil {
		t.Errorf("plan.OS = %+v, want nil (no spec.imageRef)", plan.OS)
	}
	if len(plan.DataImages) != 2 {
		t.Fatalf("len(DataImages) = %d, want 2", len(plan.DataImages))
	}
	if plan.DataImages[0].Disk != "/dev/sda" || plan.DataImages[0].Partitions[0].Device != "/dev/sda1" {
		t.Errorf("DataImages[0] = %+v", plan.DataImages[0])
	}
	if plan.DataImages[1].Disk != "/dev/sdb" || plan.DataImages[1].Partitions[0].Device != "/dev/sdb1" {
		t.Errorf("DataImages[1] = %+v", plan.DataImages[1])
	}
	if plan.AfterDeploy != keziov1alpha1.AfterDeployReboot {
		t.Errorf("AfterDeploy = %q, want the Reboot default", plan.AfterDeploy)
	}
}

func TestBuildDeployPlan_NotReadyCases(t *testing.T) {
	storeRoot := t.TempDir()
	image, cm := readyImage("os-image", "{}", []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleData, FSType: "ext4"},
	})

	baseMachine := func() *keziov1alpha1.Machine {
		return &keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef: &keziov1alpha1.NameRef{Name: "os-image"},
			},
			Status: keziov1alpha1.MachineStatus{
				State: keziov1alpha1.MachineStateProvisioning,
				Provisioning: &keziov1alpha1.MachineProvisioningStatus{
					Image: &keziov1alpha1.MachineProvisionedImage{
						ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
						TargetDisk: "/dev/nvme0n1",
					},
				},
			},
		}
	}

	cases := []struct {
		name   string
		modify func(m *keziov1alpha1.Machine)
	}{
		{"not provisioning", func(m *keziov1alpha1.Machine) { m.Status.State = keziov1alpha1.MachineStateAvailable }},
		{"no provisioning status", func(m *keziov1alpha1.Machine) { m.Status.Provisioning = nil }},
		{"target disk unresolved", func(m *keziov1alpha1.Machine) { m.Status.Provisioning.Image.TargetDisk = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := baseMachine()
			tc.modify(machine)
			c := newPlanTestClient(t, image, cm, machine)
			cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

			plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
			if err != nil {
				t.Fatalf("buildDeployPlan: %v", err)
			}
			if plan != nil {
				t.Fatalf("plan = %+v, want nil (not ready)", plan)
			}
		})
	}
}

func TestBuildDeployPlan_ImageNotReadyIsNotAnError(t *testing.T) {
	storeRoot := t.TempDir()
	image, cm := readyImage("os-image", "{}", nil)
	image.Status.State = keziov1alpha1.ImageStateIngesting

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil (Image not Ready)", plan)
	}
}

// TestBuildDeployPlan_ImageFailedIsNotThePlanBuildersJob characterizes
// buildImagePlan's handling of an Image in ImageStateFailed (the state a
// checksum mismatch or a failed ingest job drives an Image to): it is
// treated exactly like ImageStateIngesting - "not ready yet", answered
// with a nil plan and a nil error, never a plan-building error. Detecting
// that an Image has permanently failed and moving the Machine to
// MachineStateError is the controller's job (see
// MachineReconciler.checkReferencedImagesFailed in
// internal/controller/machine_controller.go), which runs before a Machine
// referencing a failed Image is ever polled through this path again.
func TestBuildDeployPlan_ImageFailedIsNotThePlanBuildersJob(t *testing.T) {
	storeRoot := t.TempDir()
	image, cm := readyImage("os-image", "{}", nil)
	image.Status.State = keziov1alpha1.ImageStateFailed

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil (Image Failed)", plan)
	}
}

func TestBuildDeployPlan_MissingLayoutConfigMapIsAnError(t *testing.T) {
	storeRoot := t.TempDir()
	image, _ := readyImage("os-image", "{}", nil) // ConfigMap deliberately not seeded

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err == nil {
		t.Fatal("buildDeployPlan: want an error for a missing layout ConfigMap")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil alongside the error", plan)
	}
}

func TestBuildDeployPlan_ImageRefResolvesToRefNamespaceNotMachineNamespace(t *testing.T) {
	storeRoot, hash := writeFixtureContent(t)

	partitions := []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat", InfoHash: hash.String()},
	}
	image, cm := readyImage("os-image", `{"partitiontable":{"label":"gpt"}}`, partitions)
	image.Namespace = "images-ns"
	cm.Namespace = "images-ns"

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			ImageRef: &keziov1alpha1.NameRef{Name: "os-image", Namespace: "images-ns"},
		},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image", Namespace: "images-ns"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("buildDeployPlan returned a nil plan; want a ready plan built from the ref's namespace (images-ns), not machine.Namespace (default)")
	}
	if plan.OS == nil {
		t.Fatal("plan.OS is nil")
	}
	if plan.OS.Disk != "/dev/nvme0n1" {
		t.Errorf("OS.Disk = %q, want /dev/nvme0n1", plan.OS.Disk)
	}
	if len(plan.OS.Partitions) != 1 {
		t.Fatalf("len(OS.Partitions) = %d, want 1", len(plan.OS.Partitions))
	}
}

func TestBuildDeployPlan_MissingStoreContentForRecordedInfoHashIsAnError(t *testing.T) {
	// storeRoot is a fresh, empty temp dir: the InfoHash below is
	// well-formed but its content directory was never written, so
	// buildPartitionTorrent's LoadContentDirTorrentInfo call must fail.
	storeRoot := t.TempDir()
	_, hash := writeFixtureContent(t) // only used to mint a well-formed InfoHash

	partitions := []keziov1alpha1.ImagePartitionStatus{
		{Number: 1, Role: keziov1alpha1.PartitionRoleESP, FSType: "vfat", InfoHash: hash.String()},
	}
	image, cm := readyImage("os-image", `{"partitiontable":{"label":"gpt"}}`, partitions)

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}},
		Status: keziov1alpha1.MachineStatus{
			State: keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
					TargetDisk: "/dev/nvme0n1",
				},
			},
		},
	}

	c := newPlanTestClient(t, image, cm, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err == nil {
		t.Fatal("buildDeployPlan: want an error for a recorded InfoHash whose content was never written to the store")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil alongside the error", plan)
	}
}

func TestBuildDeployPlan_NoOSImageOrDataImagesReturnsNilPlan(t *testing.T) {
	storeRoot := t.TempDir()

	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Status: keziov1alpha1.MachineStatus{
			State:        keziov1alpha1.MachineStateProvisioning,
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{},
		},
	}

	c := newPlanTestClient(t, machine)
	cfg := Config{StoreRoot: storeRoot, TrackerURL: testTrackerURL}

	plan, err := buildDeployPlan(context.Background(), c, cfg, machine)
	if err != nil {
		t.Fatalf("buildDeployPlan: %v", err)
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil (no OS image, no dataImages)", plan)
	}
}

func int32Ptr(v int32) *int32 { return &v }
