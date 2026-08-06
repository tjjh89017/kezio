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
	"regexp"
	"sort"
	"strconv"
	"strings"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// Collect gathers the full hardware inventory this machine's agent
// reports at registration (the disk hint sources the controller's
// disk-matching logic consumes, plus NICs, memory, and CPU count),
// reading everything under root instead of the
// real "/" - production calls Collect("/"); tests point root at a
// testdata fixture tree so no real hardware is needed to exercise the
// parsing logic.
//
// Every per-item read (a disk's serial, a NIC's MAC, ...) is
// best-effort: a missing or unreadable attribute file is common and
// expected (not every disk exposes a serial through sysfs; a NIC that
// is down may not expose speed), so Collect leaves that one field at its
// zero value instead of failing the whole inventory over it. Collect
// only returns an error when it cannot even enumerate the top-level
// /sys/block or /sys/class/net directories, or read /proc/meminfo or
// /proc/cpuinfo - a live environment where those are missing has bigger
// problems than an incomplete inventory.
func Collect(root string) (*keziov1alpha1.MachineHardwareStatus, error) {
	disks, err := collectDisks(root)
	if err != nil {
		return nil, err
	}
	nics, err := collectNICs(root)
	if err != nil {
		return nil, err
	}
	memBytes, err := collectMemoryBytes(root)
	if err != nil {
		return nil, err
	}
	cpuCount, err := collectCPUCount(root)
	if err != nil {
		return nil, err
	}

	return &keziov1alpha1.MachineHardwareStatus{
		Disks:       disks,
		Nics:        nics,
		MemoryBytes: memBytes,
		CPUCount:    cpuCount,
	}, nil
}

// virtualBlockDevicePrefixes names /sys/block entries that are never
// physical disks worth reporting as a deployment target: loopback
// devices and RAM disks. Everything else - SCSI/SATA (sdX), NVMe
// (nvmeXnY), virtio (vdX), and so on - is reported; the controller's
// disk-hint matching logic is what decides which one is the
// deployment target, not this collector.
var virtualBlockDevicePrefixes = []string{"loop", "ram"}

func isVirtualBlockDevice(name string) bool {
	for _, p := range virtualBlockDevicePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func collectDisks(root string) ([]keziov1alpha1.MachineHardwareDisk, error) {
	blockDir := filepath.Join(root, "sys", "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, err
	}

	var disks []keziov1alpha1.MachineHardwareDisk
	for _, e := range entries {
		name := e.Name()
		if isVirtualBlockDevice(name) {
			continue
		}
		disks = append(disks, collectOneDisk(blockDir, name))
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].DeviceName < disks[j].DeviceName })
	return disks, nil
}

func collectOneDisk(blockDir, name string) keziov1alpha1.MachineHardwareDisk {
	devDir := filepath.Join(blockDir, name)

	disk := keziov1alpha1.MachineHardwareDisk{
		DeviceName:   "/dev/" + name,
		SerialNumber: readTrimmed(filepath.Join(devDir, "device", "serial")),
		WWN:          readTrimmed(filepath.Join(devDir, "wwid")),
		Model:        readTrimmed(filepath.Join(devDir, "device", "model")),
		Vendor:       readTrimmed(filepath.Join(devDir, "device", "vendor")),
	}

	if disk.SerialNumber == "" {
		// virtio-blk has no device/serial; it exposes the serial at the
		// block device's top level (/sys/block/vdX/serial). SCSI and
		// NVMe keep their device/serial precedence above.
		disk.SerialNumber = readTrimmed(filepath.Join(devDir, "serial"))
	}

	if sectors, ok := readUint(filepath.Join(devDir, "size")); ok {
		// /sys/block/<dev>/size is always in 512-byte sectors,
		// regardless of the device's actual logical block size - a
		// sysfs convention, not this device's real sector size.
		disk.SizeBytes = int64(sectors) * 512
	}

	if v := readTrimmed(filepath.Join(devDir, "queue", "rotational")); v != "" {
		rotational := v == "1"
		disk.Rotational = &rotational
	}

	if target, err := os.Readlink(devDir); err == nil {
		if pcie, ok := lastMatch(pciePathPattern, target); ok {
			disk.PciePath = pcie
		}
		if hctl, ok := lastMatch(hctlPattern, target); ok {
			disk.HCTL = hctl
		}
	}

	return disk
}

// pciePathPattern matches a PCI/PCIe address path segment, for example
// "0000:01:00.0", as it appears in the /sys/block/<dev> symlink target.
var pciePathPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// hctlPattern matches a SCSI host:channel:target:lun address path
// segment, for example "0:0:0:0".
var hctlPattern = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+:[0-9]+$`)

// lastMatch returns the last path segment of target that matches re: the
// symlink target walks from the PCI root down to the block device (for
// example ".../0000:00:1c.0/0000:01:00.0/nvme/nvme0/nvme0n1"), and the
// segment closest to the device - the last match - is the one this
// device actually sits behind.
func lastMatch(re *regexp.Regexp, target string) (string, bool) {
	found := ""
	for _, seg := range strings.Split(target, "/") {
		if re.MatchString(seg) {
			found = seg
		}
	}
	return found, found != ""
}

func collectNICs(root string) ([]keziov1alpha1.MachineHardwareNIC, error) {
	netDir := filepath.Join(root, "sys", "class", "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, err
	}

	var nics []keziov1alpha1.MachineHardwareNIC
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		nics = append(nics, keziov1alpha1.MachineHardwareNIC{
			Name:       name,
			MACAddress: readTrimmed(filepath.Join(netDir, name, "address")),
		})
	}
	sort.Slice(nics, func(i, j int) bool { return nics[i].Name < nics[j].Name })
	return nics, nil
}

// meminfoTotalPattern matches /proc/meminfo's MemTotal line, for example
// "MemTotal:       16384000 kB".
var meminfoTotalPattern = regexp.MustCompile(`(?m)^MemTotal:\s+(\d+)\s*kB`)

func collectMemoryBytes(root string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "meminfo"))
	if err != nil {
		return 0, err
	}
	m := meminfoTotalPattern.FindSubmatch(data)
	if m == nil {
		return 0, nil
	}
	kb, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, nil
	}
	return kb * 1024, nil
}

func collectCPUCount(root string) (int32, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "cpuinfo"))
	if err != nil {
		return 0, err
	}
	var count int32
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	return count, nil
}

// readTrimmed reads path and returns its trimmed content, or "" if the
// file does not exist or cannot be read - a common, expected case for
// many optional sysfs attribute files.
func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readUint reads path as a base-10 unsigned integer, returning ok=false
// if the file is missing, empty, or not a valid number.
func readUint(path string) (uint64, bool) {
	v := readTrimmed(path)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
