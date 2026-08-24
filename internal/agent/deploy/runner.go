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

package deploy

import "context"

// Runner executes one external command to completion and returns its
// combined stdout+stderr output. Executor shells out to sfdisk, blockdev,
// mkfs.*, mkswap, efibootmgr, mount/umount, and /bin/sh through this
// single narrow interface instead of calling os/exec directly, so the
// orchestration logic in this package - which partitions get which
// command, in what order, on what condition - is unit-testable against a
// fake Runner that records every invocation without touching a real
// device or needing root.
type Runner interface {
	// Run executes name with args, feeding stdin to the process (nil
	// means no input), and returns its combined output. A non-nil error
	// means the process failed to start or exited non-zero; the returned
	// output (if any) is still useful for the caller's error message.
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

	// RunEnv is Run with env - entries in "NAME=value" form - added to
	// the environment the process inherits. Only a post hook script step
	// needs it: every other command this package runs takes all of its
	// input through arguments or stdin.
	RunEnv(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error)
}
