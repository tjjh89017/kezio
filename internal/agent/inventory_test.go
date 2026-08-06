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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func diskByName(disks []keziov1alpha1.MachineHardwareDisk, name string) *keziov1alpha1.MachineHardwareDisk {
	for i := range disks {
		if disks[i].DeviceName == name {
			return &disks[i]
		}
	}
	return nil
}

func TestCollect(t *testing.T) {
	hw, err := Collect("testdata/host1")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	t.Run("virtual block devices are filtered out", func(t *testing.T) {
		if len(hw.Disks) != 2 {
			t.Fatalf("Disks = %+v, want exactly sda and nvme0n1", hw.Disks)
		}
	})

	t.Run("scsi disk: serial, model, vendor, size, rotational, hctl, pcie path", func(t *testing.T) {
		sda := diskByName(hw.Disks, "/dev/sda")
		if sda == nil {
			t.Fatalf("no /dev/sda in %+v", hw.Disks)
		}
		if sda.SerialNumber != "SERIAL123" {
			t.Errorf("SerialNumber = %q, want SERIAL123", sda.SerialNumber)
		}
		if sda.Model != "SuperDisk" {
			t.Errorf("Model = %q, want SuperDisk", sda.Model)
		}
		if sda.Vendor != "ACME" {
			t.Errorf("Vendor = %q, want ACME", sda.Vendor)
		}
		if sda.WWN != "t10.ATA-WWN-SDA-0001" {
			t.Errorf("WWN = %q, want t10.ATA-WWN-SDA-0001", sda.WWN)
		}
		if sda.SizeBytes != 1000000*512 {
			t.Errorf("SizeBytes = %d, want %d", sda.SizeBytes, 1000000*512)
		}
		if sda.Rotational == nil || !*sda.Rotational {
			t.Errorf("Rotational = %v, want true", sda.Rotational)
		}
		if sda.HCTL != "0:0:0:0" {
			t.Errorf("HCTL = %q, want 0:0:0:0", sda.HCTL)
		}
		if sda.PciePath != "0000:00:1f.2" {
			t.Errorf("PciePath = %q, want 0000:00:1f.2", sda.PciePath)
		}
	})

	t.Run("nvme disk: pcie path, no hctl, solid state", func(t *testing.T) {
		nvme := diskByName(hw.Disks, "/dev/nvme0n1")
		if nvme == nil {
			t.Fatalf("no /dev/nvme0n1 in %+v", hw.Disks)
		}
		if nvme.SizeBytes != 2000000000*512 {
			t.Errorf("SizeBytes = %d, want %d", nvme.SizeBytes, 2000000000*512)
		}
		if nvme.Rotational == nil || *nvme.Rotational {
			t.Errorf("Rotational = %v, want false", nvme.Rotational)
		}
		if nvme.HCTL != "" {
			t.Errorf("HCTL = %q, want empty (nvme has no SCSI HCTL)", nvme.HCTL)
		}
		// The PCIe symlink target for nvme0n1 has two PCI-address-shaped
		// segments (the bridge at 0000:00:1c.0 and the NVMe controller
		// at 0000:01:00.0); the device sits behind the latter.
		if nvme.PciePath != "0000:01:00.0" {
			t.Errorf("PciePath = %q, want 0000:01:00.0", nvme.PciePath)
		}
	})

	t.Run("loopback interface is filtered out, physical nic is reported", func(t *testing.T) {
		if len(hw.Nics) != 1 {
			t.Fatalf("Nics = %+v, want exactly eth0", hw.Nics)
		}
		if hw.Nics[0].Name != "eth0" || hw.Nics[0].MACAddress != "aa:bb:cc:dd:ee:01" {
			t.Errorf("Nics[0] = %+v, want eth0/aa:bb:cc:dd:ee:01", hw.Nics[0])
		}
	})

	t.Run("memory and cpu", func(t *testing.T) {
		if hw.MemoryBytes != 16336000*1024 {
			t.Errorf("MemoryBytes = %d, want %d", hw.MemoryBytes, 16336000*1024)
		}
		if hw.CPUCount != 2 {
			t.Errorf("CPUCount = %d, want 2", hw.CPUCount)
		}
	})
}

func TestCollect_MissingSysBlockIsAnError(t *testing.T) {
	if _, err := Collect("testdata/does-not-exist"); err == nil {
		t.Fatalf("expected an error when /sys/block cannot be enumerated")
	}
}

func TestCollect_MissingOptionalAttributesLeaveZeroValues(t *testing.T) {
	// A disk directory with no device/, no wwid, no queue/ - every
	// per-attribute read must degrade gracefully instead of failing the
	// whole collection.
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "sys", "block", "sdz"))
	mkdirAll(t, filepath.Join(root, "sys", "class", "net"))
	mkdirAll(t, filepath.Join(root, "proc"))
	writeFile(t, filepath.Join(root, "proc", "meminfo"), "MemTotal:       1024 kB\n")
	writeFile(t, filepath.Join(root, "proc", "cpuinfo"), "processor\t: 0\n")

	hw, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(hw.Disks) != 1 {
		t.Fatalf("Disks = %+v, want exactly one bare disk entry", hw.Disks)
	}
	sdz := hw.Disks[0]
	if sdz.DeviceName != "/dev/sdz" {
		t.Errorf("DeviceName = %q, want /dev/sdz", sdz.DeviceName)
	}
	if sdz.SerialNumber != "" || sdz.Model != "" || sdz.WWN != "" || sdz.SizeBytes != 0 || sdz.Rotational != nil {
		t.Errorf("expected every optional field at its zero value, got %+v", sdz)
	}
}

func TestCollect_SerialNumberSources(t *testing.T) {
	// SCSI and NVMe expose the serial at device/serial; virtio-blk has no
	// device/serial and instead exposes it at the block device's top
	// level (/sys/block/vdX/serial). collectOneDisk must read
	// device/serial first and fall back to the top-level file, so a
	// virtio disk's serial still reaches the controller's disk-hint
	// matching.
	cases := []struct {
		name           string
		deviceSerial   string // device/serial content; "" means the file is absent
		topLevelSerial string // <dev>/serial content; "" means the file is absent
		want           string
	}{
		{"virtio: top-level serial only", "", "KEZIOE2EBMCDISK1", "KEZIOE2EBMCDISK1"},
		{"scsi: device/serial only", "SCSISER1", "", "SCSISER1"},
		{"both present: device/serial wins", "DEVSER1", "TOPSER1", "DEVSER1"},
		{"neither present: empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			devDir := filepath.Join(root, "sys", "block", "vda")
			mkdirAll(t, filepath.Join(devDir, "device"))
			if tc.deviceSerial != "" {
				writeFile(t, filepath.Join(devDir, "device", "serial"), tc.deviceSerial+"\n")
			}
			if tc.topLevelSerial != "" {
				writeFile(t, filepath.Join(devDir, "serial"), tc.topLevelSerial+"\n")
			}
			mkdirAll(t, filepath.Join(root, "sys", "class", "net"))
			mkdirAll(t, filepath.Join(root, "proc"))
			writeFile(t, filepath.Join(root, "proc", "meminfo"), "MemTotal:       1024 kB\n")
			writeFile(t, filepath.Join(root, "proc", "cpuinfo"), "processor\t: 0\n")

			hw, err := Collect(root)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			vda := diskByName(hw.Disks, "/dev/vda")
			if vda == nil {
				t.Fatalf("no /dev/vda in %+v", hw.Disks)
			}
			if vda.SerialNumber != tc.want {
				t.Errorf("SerialNumber = %q, want %q", vda.SerialNumber, tc.want)
			}
		})
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
