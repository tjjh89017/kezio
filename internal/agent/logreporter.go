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
	"context"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// LogReporter is a deploy.Reporter that only logs: it never posts to the
// controller. Posting deploy progress over POST /agent/progress (and
// reacting to agentapi.ProgressResponse.Action == ProgressActionAbort)
// is not implemented yet; this stands in for it so a deploy's steps are
// still observable in the agent's own log output.
type LogReporter struct {
	// Logf receives one line per report, in fmt.Printf style. Nil
	// discards them, making the zero value a silent no-op.
	Logf func(format string, args ...any)
}

var _ deploy.Reporter = LogReporter{}

// Report implements deploy.Reporter.
func (r LogReporter) Report(_ context.Context, req agentapi.ProgressRequest) error {
	if r.Logf == nil {
		return nil
	}
	if req.Message != "" {
		r.Logf("deploy progress: step=%s state=%s message=%s", req.Step, req.State, req.Message)
	} else {
		r.Logf("deploy progress: step=%s state=%s", req.Step, req.State)
	}
	return nil
}
