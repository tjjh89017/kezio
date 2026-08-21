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

package planbuild

import (
	"fmt"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/diskmatch"
)

// resolveDisk matches hints against hardware's inventory, wrapping
// diskmatch's sentinel errors into a DiskSelectionError labeled for the
// image/dataImages entry that failed to resolve.
func resolveDisk(disks []keziov1alpha2.MachineHardwareDisk, hints *keziov1alpha2.TargetDiskHints, label string) (*keziov1alpha2.MachineHardwareDisk, error) {
	disk, err := diskmatch.Match(disks, hints)
	if err != nil {
		return nil, &DiskSelectionError{Reason: fmt.Sprintf("%s: %v", label, err)}
	}
	return disk, nil
}

// checkDistinctDisks wraps diskmatch.CheckDistinct's ErrAmbiguous as a
// DiskSelectionError, matching resolveDisk's error shape.
func checkDistinctDisks(selections []diskmatch.Selection) error {
	if err := diskmatch.CheckDistinct(selections); err != nil {
		return &DiskSelectionError{Reason: err.Error()}
	}
	return nil
}

// partitionDevicePath renders the kernel device path for partition number
// on disk, following the standard partition-suffix convention: a "pN"
// separator for a disk name ending in a digit (nvme0n1, mmcblk0, loop0),
// a bare number otherwise (sda, vda).
func partitionDevicePath(disk string, number int32) string {
	if len(disk) > 0 {
		last := disk[len(disk)-1]
		if last >= '0' && last <= '9' {
			return fmt.Sprintf("%sp%d", disk, number)
		}
	}
	return fmt.Sprintf("%s%d", disk, number)
}
