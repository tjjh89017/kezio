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

// Command agent is kezio-agent: the binary that runs in the live boot
// environment (see hack/live-image). It reads its controller URL and
// single-use boot token off the kernel cmdline (kezio.server=,
// kezio.token=), collects the machine's hardware inventory, registers
// with the controller (internal/agentserver), and polls for its next
// action. See internal/agent for the actual logic; this file is only
// process wiring (cmdline flags, signal handling, exit codes).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjjh89017/kezio/internal/agent"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

func main() {
	// cmdlinePath and inventoryRoot default to the real paths every
	// production boot reads; overriding them is only ever useful to
	// point this binary at a fixture tree instead of a live machine.
	cmdlinePath := flag.String("cmdline-path", agent.ProcCmdlinePath,
		"path to the kernel cmdline to read kezio.server=/kezio.token= from")
	inventoryRoot := flag.String("inventory-root", "/",
		"root directory hardware inventory collection reads /sys and /proc under")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmdline, err := agent.ReadCmdline(*cmdlinePath)
	if err != nil {
		log.Fatalf("kezio-agent: reading kernel cmdline: %v", err)
	}
	log.Printf("kezio-agent: booted; kezio.server=%s kezio.token=%s", cmdline.Server, redactToken(cmdline.Token))

	reporter := &agent.HTTPReporter{Client: agent.NewClient(cmdline.Server), Logf: log.Printf}
	executor := &deploy.Executor{
		Runner:   agent.ExecRunner{},
		Ezio:     agent.ExecEzioLauncher{Logf: log.Printf},
		Fetcher:  agent.HTTPTorrentFetcher{},
		Progress: reporter,
		Logf:     log.Printf,
	}

	err = agent.Run(ctx, agent.Config{
		Cmdline:       cmdline,
		InventoryRoot: *inventoryRoot,
		Logf:          log.Printf,
		OnRegistered: func(result agent.RegisterResult) {
			reporter.SessionToken = result.SessionToken
		},
		Deploy: func(ctx context.Context, plan *agentapi.DeployPlan) error {
			// A fresh, cancellable context per deploy: reporter.Cancel
			// stops this specific Execute call on a controller-requested
			// abort without also tearing down the agent's own poll loop.
			deployCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			reporter.Cancel = cancel
			return executor.Execute(deployCtx, plan)
		},
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("kezio-agent: %v", err)
	}
}

// redactToken returns a short, non-reversible summary of a boot token
// for log lines: the token is a live, single-use credential and must
// never appear in full in a log that a bystander or unrelated tooling
// might read (e.g. a serial console capture). An empty token is
// reported as such rather than folded into the same "<redacted>" a
// present-but-short token gets: the booted-line log is the only signal
// an operator reading a serial console capture has for telling "this
// boot has no token to register with" apart from "it has one, just not
// shown" - collapsing that distinction was what made the crash-loop
// this only symptom look like kezio-agent itself was broken.
func redactToken(token string) string {
	if token == "" {
		return "<empty>"
	}
	if len(token) <= 8 {
		return "<redacted>"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
