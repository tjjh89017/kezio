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

package main

import (
	"context"
	"fmt"
	"os/exec"
)

// execSfdisk implements ingest.Sfdisk by shelling out to sfdisk. sfdisk
// reads a regular file exactly like a block device (the same S_ISREG
// handling partclone has), so this needs no privilege either.
type execSfdisk struct{}

func (execSfdisk) Dump(ctx context.Context, diskPath string) ([]byte, error) {
	//nolint:gosec // diskPath is an ingest-controlled scratch path, not user input
	cmd := exec.CommandContext(ctx, "sfdisk", "--json", diskPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sfdisk --json %s: %w", diskPath, err)
	}
	return out, nil
}
