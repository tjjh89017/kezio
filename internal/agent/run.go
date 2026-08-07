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
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// DefaultRegisterRetryInterval is how long Run waits between failed
// registration attempts, when Config.RegisterRetryInterval is zero.
// Registration can fail for reasons that clear up on their own shortly
// after boot (the NIC has not finished DHCP yet, the controller is
// momentarily unreachable), so Run keeps retrying rather than giving up
// - there is nothing else useful for the live environment to do instead.
const DefaultRegisterRetryInterval = 5 * time.Second

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
	// RegisterRetryInterval is how long registerWithRetry waits between
	// failed registration attempts. Zero means
	// DefaultRegisterRetryInterval.
	RegisterRetryInterval time.Duration
	// Logf receives progress and error messages in fmt.Printf style.
	// Nil discards them.
	Logf func(format string, args ...any)
	// Executor executes a DeployPlan received from a "deploy" poll
	// response. Nil (the default in every test that only exercises the
	// poll loop's logging) means a received plan is logged and never
	// executed - the behavior this package had before the deploy
	// executor existed. Production wiring (cmd/agent) sets it to a
	// *deploy.Executor with real Runner/Ezio implementations; Run
	// attaches this machine's own progress reporter to a copy of it
	// once registration resolves the machine name and session token
	// Executor.Progress needs.
	Executor *deploy.Executor
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
// cancelled. See DefaultRegisterRetryInterval's doc comment for why
// retrying indefinitely is the right behavior here, rather than giving up
// after a fixed number of attempts.
func registerWithRetry(ctx context.Context, client *Client, cfg Config, hardware *keziov1alpha1.MachineHardwareStatus) (RegisterResult, error) {
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

// pollLoop calls Client.Next on pollInterval until ctx is cancelled,
// presenting reg.SessionToken on every call. A poll error is logged and
// retried on the next tick, the same way a heartbeat that misses one
// beat should not bring the whole agent down. A "wait" response is
// logged and otherwise ignored. A "deploy" response's plan is always
// logged; when cfg.Executor is set, it is also executed - once. A
// successful execution latches deployed so a plan that stays available
// on later polls (the controller keeps answering ActionDeploy until the
// Machine leaves Provisioning, which is a later work item's job) is
// never re-applied; a failed execution does not latch, so a transient
// failure (a disk not yet settled, a momentary ezio hiccup) gets retried
// on the next poll.
//
// Before executing, a received plan's SchemaVersion is checked against
// agentapi.PlanSchemaVersion: internal/agentserver's own compatibility
// gate (this agent already advertised its version in the Next call
// above) should make a mismatch here unreachable in practice, but a
// received plan is untrusted input regardless of which side is supposed
// to have already refused it, so this is the second, independent check -
// see agentapi's package doc comment. A mismatch is refused the same way
// a validation failure inside Executor.Execute is: logged, reported as a
// deploy-step failure, and never handed to Execute, so no disk is ever
// touched.
func pollLoop(ctx context.Context, client *Client, cfg Config, reg RegisterResult, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	executor := attachProgressReporter(cfg.Executor, client, reg)
	deployed := false

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

			if executor == nil || deployed || resp.Action != agentapi.ActionDeploy || resp.Plan == nil {
				continue
			}
			if resp.Plan.SchemaVersion != agentapi.PlanSchemaVersion {
				cfg.log("refusing deploy plan: schema version %d unsupported (this agent supports %d); no disk touched",
					resp.Plan.SchemaVersion, agentapi.PlanSchemaVersion)
				reportSchemaMismatch(ctx, cfg, client, reg, resp.Plan.SchemaVersion)
				continue
			}
			if err := executor.Execute(ctx, resp.Plan); err != nil {
				cfg.log("executing deploy plan failed: %v", err)
				continue
			}
			cfg.log("deploy plan executed: every partition written or made, local ezio daemon shut down")
			deployed = true
		}
	}
}

// reportSchemaMismatch tells the controller this agent refused a
// DeployPlan whose SchemaVersion it does not support, using the same
// DeployStepFailed shape internal/agent/deploy.Executor.Execute reports
// on any other refusal - so the controller's real, agent-driven Deployer
// recognizes the deploy failed instead of polling forever, and an
// operator reading MachineConditionProvisioningProgress sees exactly
// why. Best-effort, like every other progress report: a delivery
// failure is logged, not escalated.
func reportSchemaMismatch(ctx context.Context, cfg Config, client *Client, reg RegisterResult, gotVersion int) {
	req := agentapi.ProgressRequest{
		Step: agentapi.DeployStepFailed,
		StepMessage: fmt.Sprintf("refusing deploy plan: server sent schema version %d, this agent only supports %d",
			gotVersion, agentapi.PlanSchemaVersion),
	}
	if err := client.ReportProgress(ctx, reg.MachineName, reg.SessionToken, req); err != nil {
		cfg.log("reporting schema-version refusal failed: %v", err)
	}
}

// attachProgressReporter returns a copy of template with Progress set to
// a reporter bound to reg's machine name and session token, or nil when
// template itself is nil. It copies rather than mutates *template so a
// Config value (and its Executor template) stays reusable across
// multiple Run calls without one call's session leaking into another's.
func attachProgressReporter(template *deploy.Executor, client *Client, reg RegisterResult) *deploy.Executor {
	if template == nil {
		return nil
	}
	executor := *template
	executor.Progress = &clientProgressReporter{client: client, machineName: reg.MachineName, sessionToken: reg.SessionToken}
	return &executor
}

// clientProgressReporter adapts Client.ReportProgress (which needs the
// machine name and session token on every call) to
// deploy.ProgressReporter's single-argument shape.
type clientProgressReporter struct {
	client       *Client
	machineName  string
	sessionToken string
}

func (r *clientProgressReporter) ReportProgress(ctx context.Context, req agentapi.ProgressRequest) error {
	return r.client.ReportProgress(ctx, r.machineName, r.sessionToken, req)
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
