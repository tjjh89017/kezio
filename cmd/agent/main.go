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

// Command agent is kezio-agent: the binary that runs in the live boot
// environment (see hack/live-image), replacing the kezio-agent-stub
// placeholder script. It reads its controller URL and single-use boot
// token off the kernel cmdline (kezio.server=, kezio.token=; the same
// values the placeholder stub logged and nothing more), collects the
// machine's hardware inventory, registers with the controller
// (internal/agentserver on the other end), and then polls for its next
// action. See internal/agent for the actual logic; this file is only
// process wiring (signal handling, logging, exit codes).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjjh89017/kezio/internal/agent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmdline, err := agent.ReadCmdline(agent.ProcCmdlinePath)
	if err != nil {
		log.Fatalf("kezio-agent: reading kernel cmdline: %v", err)
	}
	log.Printf("kezio-agent: booted; kezio.server=%s kezio.token=%s", cmdline.Server, redactToken(cmdline.Token))

	err = agent.Run(ctx, agent.Config{
		Cmdline:       cmdline,
		InventoryRoot: "/",
		Logf:          log.Printf,
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("kezio-agent: %v", err)
	}
}

// redactToken returns a short, non-reversible summary of a boot token
// for log lines: the token is a live, single-use credential, so it must
// never appear in full in a log a bystander could read over someone's
// shoulder or that ends up in a serial console capture picked up by
// unrelated tooling. The stub script this binary replaces logged the
// token in full; that was acceptable only because it was never anything
// but a placeholder proving the cmdline plumbing worked, not a real
// credential flow.
func redactToken(token string) string {
	if len(token) <= 8 {
		return "<redacted>"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
