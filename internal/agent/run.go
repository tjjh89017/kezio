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

package agent

import (
	"context"
	"fmt"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// registerRetryInterval is how long Run waits between failed
// registration attempts. Registration can fail for reasons that clear up
// on their own shortly after boot (the NIC has not finished DHCP yet,
// the controller is momentarily unreachable), so Run keeps retrying
// rather than giving up - there is nothing else useful for the live
// environment to do instead.
const registerRetryInterval = 5 * time.Second

// DefaultPollInterval is how often Run polls GET .../next once
// registered, when Config.PollInterval is zero.
const DefaultPollInterval = 10 * time.Second

// Config configures Run.
type Config struct {
	// Cmdline carries the controller URL and boot token, normally read
	// from ReadCmdline(ProcCmdlinePath).
	Cmdline Cmdline
	// InventoryRoot is the root Collect reads hardware inventory from.
	// Production passes "/"; tests pass a fixture directory.
	InventoryRoot string
	// PollInterval is how often Run polls GET .../next after
	// registering. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// Logf receives progress and error messages in fmt.Printf style.
	// Nil discards them.
	Logf func(format string, args ...any)
}

func (c Config) log(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run drives the agent's whole lifecycle: collect hardware inventory,
// register with the controller (retrying until it succeeds or ctx is
// cancelled), then poll for the next action until ctx is cancelled. It
// returns only when ctx is cancelled (nil) or a step fails in a way that
// retrying cannot fix (a malformed cmdline: there is nothing to register
// with).
func Run(ctx context.Context, cfg Config) error {
	if cfg.Cmdline.Server == "" || cfg.Cmdline.Token == "" {
		return fmt.Errorf("kernel cmdline is missing kezio.server= or kezio.token=; nothing to register with")
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}

	hardware, err := Collect(cfg.InventoryRoot)
	if err != nil {
		return fmt.Errorf("collecting hardware inventory: %w", err)
	}
	cfg.log("collected inventory: %d disk(s), %d nic(s), %d MiB memory, %d cpu(s)",
		len(hardware.Disks), len(hardware.Nics), hardware.MemoryBytes/(1<<20), hardware.CPUCount)

	client := NewClient(cfg.Cmdline.Server)

	result, err := registerWithRetry(ctx, client, cfg, hardware)
	if err != nil {
		return err
	}
	cfg.log("registered as machine %q", result.MachineName)

	return pollLoop(ctx, client, cfg, result, pollInterval)
}

// registerWithRetry calls Client.Register until it succeeds or ctx is
// cancelled. See registerRetryInterval's doc comment for why retrying
// indefinitely is the right behavior here, rather than giving up after a
// fixed number of attempts.
func registerWithRetry(ctx context.Context, client *Client, cfg Config, hardware *keziov1alpha1.MachineHardwareStatus) (RegisterResult, error) {
	for {
		result, err := client.Register(ctx, cfg.Cmdline.Token, hardware)
		if err == nil {
			return result, nil
		}
		cfg.log("registration failed, retrying in %s: %v", registerRetryInterval, err)

		select {
		case <-ctx.Done():
			return RegisterResult{}, ctx.Err()
		case <-time.After(registerRetryInterval):
		}
	}
}

// pollLoop calls Client.Next on pollInterval until ctx is cancelled,
// presenting reg.SessionToken on every call. A poll error is logged and
// retried on the next tick, the same way a heartbeat that misses one
// beat should not bring the whole agent down. A "wait" response is
// logged and otherwise ignored; a "deploy" response's plan is parsed and
// logged - executing it is a later work item, so this loop's job today
// is proving the registered machine keeps reaching the controller and
// that a served plan decodes as expected.
func pollLoop(ctx context.Context, client *Client, cfg Config, reg RegisterResult, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resp, err := client.Next(ctx, reg.MachineName, reg.SessionToken)
			if err != nil {
				cfg.log("poll failed: %v", err)
				continue
			}
			logNextResponse(cfg, resp)
		}
	}
}

// logNextResponse reports what a poll returned: a plain "poll: wait" for
// ActionWait, or a one-line summary of the received DeployPlan for
// ActionDeploy - disk, partition count, per data image - for
// ActionDeploy. It does not execute the plan; see pollLoop's doc
// comment.
func logNextResponse(cfg Config, resp agentapi.NextResponse) {
	if resp.Action != agentapi.ActionDeploy || resp.Plan == nil {
		cfg.log("poll: %s", resp.Action)
		return
	}

	plan := resp.Plan
	if plan.OS != nil {
		cfg.log("received plan for disk %s with %d partitions (OS image %s)",
			plan.OS.Disk, len(plan.OS.Partitions), plan.OS.ImageRef.Name)
	}
	for _, dataImage := range plan.DataImages {
		cfg.log("received plan for disk %s with %d partitions (data image %s)",
			dataImage.Disk, len(dataImage.Partitions), dataImage.ImageRef.Name)
	}
}
