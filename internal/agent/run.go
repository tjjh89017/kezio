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

	machineName, err := registerWithRetry(ctx, client, cfg, hardware)
	if err != nil {
		return err
	}
	cfg.log("registered as machine %q", machineName)

	return pollLoop(ctx, client, cfg, machineName, pollInterval)
}

// registerWithRetry calls Client.Register until it succeeds or ctx is
// cancelled. See registerRetryInterval's doc comment for why retrying
// indefinitely is the right behavior here, rather than giving up after a
// fixed number of attempts.
func registerWithRetry(ctx context.Context, client *Client, cfg Config, hardware *keziov1alpha1.MachineHardwareStatus) (string, error) {
	for {
		name, err := client.Register(ctx, cfg.Cmdline.Token, hardware)
		if err == nil {
			return name, nil
		}
		cfg.log("registration failed, retrying in %s: %v", registerRetryInterval, err)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(registerRetryInterval):
		}
	}
}

// pollLoop calls Client.Next on pollInterval until ctx is cancelled. A
// poll error is logged and retried on the next tick, the same way a
// heartbeat that misses one beat should not bring the whole agent down;
// there is no action to take on the response yet (see agentapi.NextResponse's
// doc comment), so this loop's only job today is to prove the
// registered machine keeps reaching the controller.
func pollLoop(ctx context.Context, client *Client, cfg Config, machineName string, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			action, err := client.Next(ctx, machineName)
			if err != nil {
				cfg.log("poll failed: %v", err)
				continue
			}
			cfg.log("poll: %s", action)
		}
	}
}
