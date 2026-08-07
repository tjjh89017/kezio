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
// environment (see hack/live-image). It reads its controller URL and
// single-use boot token off the kernel cmdline (kezio.server=,
// kezio.token=), collects the machine's hardware inventory, registers
// with the controller (internal/agentserver), and polls for its next
// action. See internal/agent for the actual logic; this file is only
// process wiring (signal handling, logging, exit codes).
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjjh89017/kezio/internal/agent"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
)

// consolePath is the live boot environment's serial console device,
// where deploy.Executor.Console echoes the boot summary and which every
// e2e diagnostics dump's console capture reads from.
const consolePath = "/dev/console"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmdline, err := agent.ReadCmdline(agent.ProcCmdlinePath)
	if err != nil {
		log.Fatalf("kezio-agent: reading kernel cmdline: %v", err)
	}
	log.Printf("kezio-agent: booted; kezio.server=%s kezio.token=%s", cmdline.Server, redactToken(cmdline.Token))

	// console stays a nil io.Writer (not a typed-nil *os.File) on open
	// failure, so deploy.Executor.Console's nil check sees a real nil
	// rather than an interface holding a non-functional *os.File. A
	// missing console device is logged and otherwise non-fatal; the
	// terminal progress report remains the primary channel.
	var console io.Writer
	if f, err := os.OpenFile(consolePath, os.O_WRONLY, 0); err != nil {
		log.Printf("kezio-agent: opening %s for the finalize boot summary echo failed "+
			"(continuing without it): %v", consolePath, err)
	} else {
		console = f
	}

	err = agent.Run(ctx, agent.Config{
		Cmdline:       cmdline,
		InventoryRoot: "/",
		Logf:          log.Printf,
		Executor: &deploy.Executor{
			Runner:  execRunner{},
			Ezio:    ezioLauncher{Logf: log.Printf},
			Fetcher: httpTorrentFetcher{},
			Logf:    log.Printf,
			Console: console,
		},
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("kezio-agent: %v", err)
	}
}

// redactToken returns a short, non-reversible summary of a boot token
// for log lines: the token is a live, single-use credential and must
// never appear in full in a log that a bystander or unrelated tooling
// might read (e.g. a serial console capture).
func redactToken(token string) string {
	if len(token) <= 8 {
		return "<redacted>"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
