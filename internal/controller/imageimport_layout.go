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

package controller

import (
	"fmt"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/store"
)

// importLayout builds the complete ImageDiskLayout for the Image an
// import creates, from the disk its ingest Job captured. Every fact here
// comes from that one run: the partition table dump, each partition's
// role and identity, and the content name each non-swap partition's
// bytes were stored under.
//
// A swap partition gets a blank slot carrying only its file-system UUID -
// the agent runs mkswap rather than restoring bytes - so no
// PartitionContent is named for it. Every other partition gets a
// contentRef and no fsType: restored content already carries its own file
// system, and a slot may not declare both.
func importLayout(imp *keziov1alpha2.ImageImport, disk *ingest.ResultDisk) (keziov1alpha2.ImageDiskLayout, error) {
	if disk.SfdiskJSON == "" {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("ingest result carries no partition table dump")
	}
	if len(disk.Partitions) == 0 {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("ingest result carries no partitions")
	}

	slots := make([]keziov1alpha2.ImageSlot, 0, len(disk.Partitions))
	for _, part := range disk.Partitions {
		slot := keziov1alpha2.ImageSlot{
			Number:    part.Number,
			Role:      part.Role,
			TypeGUID:  part.TypeGUID,
			PartUUID:  part.PartUUID,
			SizeBytes: part.SizeBytes,
		}
		if part.Role == keziov1alpha2.PartitionRoleSwap {
			slot.UUID = part.UUID
		} else {
			slot.ContentRef = &keziov1alpha2.NameRef{
				Name: store.ContentName(imp.Spec.ContentPrefix, part.Number),
			}
		}
		slots = append(slots, slot)
	}

	return keziov1alpha2.ImageDiskLayout{
		PartitionTable: disk.PartitionTable,
		SfdiskJSON:     disk.SfdiskJSON,
		Slots:          slots,
	}, nil
}
