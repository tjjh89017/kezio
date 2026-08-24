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

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
)

// ExecRunner is the production deploy.Runner: it shells out to the named
// command through os/exec. The zero value is ready to use.
type ExecRunner struct{}

var _ deploy.Runner = ExecRunner{}

// Run implements deploy.Runner.
func (r ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	return r.RunEnv(ctx, nil, stdin, name, args...)
}

// RunEnv implements deploy.Runner. env is appended to this process's own
// environment, so a command still sees PATH and everything else the live
// environment set up; a name env repeats wins, which is how exec applies
// duplicate entries.
func (ExecRunner) RunEnv(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
