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

package kezioctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// layoutFile is the on-disk shape `kezioctl image upload --layout` reads.
// It exists because ImageDiskLayout.SfdiskJSON is a JSON-encoded *string*
// (the sfdisk --dump --json dump, kept opaque so the Image controller
// never has to re-derive it), which is awkward to author by hand as an
// escaped string literal. layoutFile instead accepts the sfdisk dump as
// nested JSON (typically produced by piping `sfdisk --dump --json
// <device>` straight into this field) and LoadLayout re-serializes it
// into ImageDiskLayout.SfdiskJSON's string form.
type layoutFile struct {
	// PartitionTable is copied verbatim into ImageDiskLayout.PartitionTable.
	PartitionTable string `json:"partitionTable"`
	// Sfdisk is the sfdisk --dump --json dump, kept as raw JSON so
	// LoadLayout can re-serialize it losslessly into
	// ImageDiskLayout.SfdiskJSON without round-tripping through a Go
	// struct that would need to track every field sfdisk emits.
	Sfdisk json.RawMessage `json:"sfdisk"`
	// Slots is copied verbatim into ImageDiskLayout.Slots.
	Slots []keziov1alpha2.ImageSlot `json:"slots"`
}

// LoadLayout reads and parses a layout file at path into an
// ImageDiskLayout for ImageSpec.Layout. See layoutFile's doc comment for
// the file's shape. kezioctl never derives a layout itself (that would
// need sfdisk/qemu-img/blkid against the source image, tools the
// server-side ingest pipeline already runs - see this package's doc
// comment): the operator supplies one, typically captured once with
// `sfdisk --dump --json` against a reference disk or copied from a
// previously ingested Image's spec.layout.
func LoadLayout(path string) (keziov1alpha2.ImageDiskLayout, error) {
	f, err := os.Open(path) //nolint:gosec // path is a CLI-supplied path, the whole point of this flag
	if err != nil {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("open layout file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	layout, err := ParseLayout(f)
	if err != nil {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("parse layout file %s: %w", path, err)
	}
	return layout, nil
}

// ParseLayout parses layoutFile JSON read from r into an ImageDiskLayout.
// Split out from LoadLayout so it can be unit tested against an in-memory
// reader instead of a real file.
func ParseLayout(r io.Reader) (keziov1alpha2.ImageDiskLayout, error) {
	var lf layoutFile
	if err := json.NewDecoder(r).Decode(&lf); err != nil {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("decode layout JSON: %w", err)
	}

	if lf.PartitionTable == "" {
		return keziov1alpha2.ImageDiskLayout{}, errors.New("layout is missing \"partitionTable\"")
	}
	if len(lf.Sfdisk) == 0 {
		return keziov1alpha2.ImageDiskLayout{}, errors.New("layout is missing \"sfdisk\"")
	}
	if len(lf.Slots) == 0 {
		return keziov1alpha2.ImageDiskLayout{}, errors.New("layout has no \"slots\"")
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, lf.Sfdisk); err != nil {
		return keziov1alpha2.ImageDiskLayout{}, fmt.Errorf("\"sfdisk\" is not valid JSON: %w", err)
	}

	return keziov1alpha2.ImageDiskLayout{
		PartitionTable: lf.PartitionTable,
		SfdiskJSON:     compact.String(),
		Slots:          lf.Slots,
	}, nil
}
