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
	"fmt"
	"time"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// DefaultRegisterRetryInterval is how long Run waits between failed
// registration attempts, when Config.RegisterRetryInterval is zero.
// Registration can fail for reasons that clear up on their own shortly
// after boot (the NIC has not finished DHCP yet, the controller is
// momentarily unreachable), so Run keeps retrying rather than giving up
// - there is nothing else useful for the live environment to do instead.
const DefaultRegisterRetryInterval = 5 * time.Second

// DefaultPollInterval is how often Run polls GET /agent/next until the
// first successful response tells it the server's own interval, and
// whenever a poll fails (PollIntervalSeconds from the last successful
// response is not available to fall back on).
const DefaultPollInterval = 10 * time.Second

// Config configures Run.
type Config struct {
	// Cmdline carries the controller URL and boot token, normally read
	// from ReadCmdline(ProcCmdlinePath).
	Cmdline Cmdline
	// InventoryRoot is the root Collect reads hardware inventory from.
	// Production passes "/"; tests pass a fixture directory.
	InventoryRoot string
	// RegisterRetryInterval is how long registerWithRetry waits between
	// failed registration attempts. Zero means
	// DefaultRegisterRetryInterval.
	RegisterRetryInterval time.Duration
	// Logf receives progress and error messages in fmt.Printf style.
	// Nil discards them.
	Logf func(format string, args ...any)
	// Deploy executes a plan once ActionDeploy is received. Nil means
	// pollLoop logs that no deploy handler is configured and keeps
	// polling - a production build (cmd/agent) always sets this to a
	// real deploy.Executor's Execute method; tests exercising pollLoop's
	// other behavior leave it nil so a stray ActionDeploy can never
	// invoke a real Executor against the test machine.
	Deploy func(ctx context.Context, plan *agentapi.DeployPlan) error
}

func (c Config) log(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run drives the agent's whole lifecycle: collect hardware inventory,
// register with the controller (retrying until it succeeds or ctx is
// cancelled), then poll /agent/next until ctx is cancelled, honouring
// the poll interval each response carries. It returns only when ctx is
// cancelled (nil) or a step fails in a way that retrying cannot fix (a
// malformed cmdline: there is nothing to register with).
func Run(ctx context.Context, cfg Config) error {
	if cfg.Cmdline.Server == "" || cfg.Cmdline.Token == "" {
		return fmt.Errorf("kernel cmdline is missing kezio.server= or kezio.token=; nothing to register with")
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

	return pollLoop(ctx, client, cfg, result)
}

// registerWithRetry calls Client.Register until it succeeds or ctx is
// cancelled. See DefaultRegisterRetryInterval's doc comment for why
// retrying indefinitely is the right behavior here, rather than giving up
// after a fixed number of attempts.
func registerWithRetry(ctx context.Context, client *Client, cfg Config, hardware keziov1alpha2.MachineHardwareSpec) (RegisterResult, error) {
	retryInterval := cfg.RegisterRetryInterval
	if retryInterval <= 0 {
		retryInterval = DefaultRegisterRetryInterval
	}

	for {
		result, err := client.Register(ctx, cfg.Cmdline.Token, hardware)
		if err == nil {
			return result, nil
		}
		cfg.log("registration failed, retrying in %s: %v", retryInterval, err)

		select {
		case <-ctx.Done():
			return RegisterResult{}, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// pollLoop calls Client.Next immediately after registration, then again
// after waiting the interval the previous response carried
// (DefaultPollInterval on the very first call, and again after any
// failed poll, since there is no fresher interval to trust), until ctx
// is cancelled. ActionWait is the only action this build's server ever
// sends; a poll error, and any other action value, is logged and
// otherwise ignored - a heartbeat that misses one beat, or that hears
// something it does not understand yet, should not bring the agent down.
func pollLoop(ctx context.Context, client *Client, cfg Config, reg RegisterResult) error {
	// Always assigned before use in the loop body below (both switch
	// branches set it), so no initial value is needed here.
	var interval time.Duration

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		resp, err := client.Next(ctx, reg.SessionToken)
		switch {
		case err != nil:
			cfg.log("poll failed: %v", err)
			interval = DefaultPollInterval
		default:
			if resp.Action == agentapi.ActionDeploy && resp.Plan != nil {
				dispatchDeploy(ctx, cfg, resp.Plan)
			} else {
				logNextResponse(cfg, resp)
			}
			if resp.PollIntervalSeconds > 0 {
				interval = time.Duration(resp.PollIntervalSeconds) * time.Second
			} else {
				interval = DefaultPollInterval
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// logNextResponse reports what a poll returned. ActionWait logs a plain
// "poll: wait"; any other action value is logged as unrecognized - this
// build's server never sends one, but a received action is untrusted
// input from an agent's perspective regardless of what the server is
// supposed to send, so an unknown value is surfaced rather than silently
// treated as a no-op.
func logNextResponse(cfg Config, resp agentapi.NextResponse) {
	if resp.Action == agentapi.ActionWait {
		cfg.log("poll: wait")
		return
	}
	cfg.log("poll: unrecognized action %q, ignoring", resp.Action)
}

// dispatchDeploy hands plan off to cfg.Deploy, logging its outcome. A nil
// cfg.Deploy (every test that does not set one, and any build that has
// not wired a production deploy.Executor yet) logs and otherwise ignores
// the plan rather than panicking on a nil call - see Config.Deploy's doc
// comment.
func dispatchDeploy(ctx context.Context, cfg Config, plan *agentapi.DeployPlan) {
	if cfg.Deploy == nil {
		cfg.log("poll: deploy plan received (run %s) but no deploy handler is configured; ignoring", plan.RunName)
		return
	}
	cfg.log("poll: deploy plan received (run %s); executing", plan.RunName)
	if err := cfg.Deploy(ctx, plan); err != nil {
		cfg.log("deploy failed: %v", err)
	}
}
