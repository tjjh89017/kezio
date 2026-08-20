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
	"strings"
	"testing"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

const testLayoutJSON = `{
  "partitionTable": "gpt",
  "sfdisk": {
    "partitiontable": {
      "label": "gpt",
      "id": "disk-guid",
      "partitions": [
        {"node": "/dev/loop0p1", "start": 2048, "size": 204800, "type": "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"}
      ]
    }
  },
  "slots": [
    {"number": 1, "role": "esp"}
  ]
}`

func TestParseLayout(t *testing.T) {
	layout, err := ParseLayout(strings.NewReader(testLayoutJSON))
	if err != nil {
		t.Fatalf("ParseLayout() error = %v", err)
	}
	if layout.PartitionTable != keziov1alpha2.PartitionTableGPT {
		t.Errorf("PartitionTable = %q, want %q", layout.PartitionTable, keziov1alpha2.PartitionTableGPT)
	}
	if !strings.Contains(layout.SfdiskJSON, "disk-guid") {
		t.Errorf("SfdiskJSON = %q, want it to carry the nested sfdisk dump", layout.SfdiskJSON)
	}
	if strings.Contains(layout.SfdiskJSON, "\n") {
		t.Errorf("SfdiskJSON = %q, want compacted JSON with no newlines", layout.SfdiskJSON)
	}
	if len(layout.Slots) != 1 || layout.Slots[0].Role != keziov1alpha2.PartitionRoleESP {
		t.Fatalf("Slots = %+v, want one esp slot", layout.Slots)
	}
}

func TestParseLayout_MissingFields(t *testing.T) {
	cases := map[string]string{
		"missing partitionTable": `{"sfdisk":{"a":1},"slots":[{"number":1,"role":"esp"}]}`,
		"missing sfdisk":         `{"partitionTable":"gpt","slots":[{"number":1,"role":"esp"}]}`,
		"missing slots":          `{"partitionTable":"gpt","sfdisk":{"a":1}}`,
		"empty slots":            `{"partitionTable":"gpt","sfdisk":{"a":1},"slots":[]}`,
		"invalid json":           `{not json`,
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLayout(strings.NewReader(input)); err == nil {
				t.Fatalf("ParseLayout(%q) expected an error", input)
			}
		})
	}
}

func TestLoadLayout_MissingFile(t *testing.T) {
	if _, err := LoadLayout("/nonexistent/layout.json"); err == nil {
		t.Fatal("LoadLayout() expected an error for a missing file")
	}
}
