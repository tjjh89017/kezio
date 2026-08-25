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

package kezioctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// statusPollInterval is how often `kezioctl status --watch` re-checks the
// Machine and its DeployRun.
const statusPollInterval = 2 * time.Second

// StatusOptions configures Status.
type StatusOptions struct {
	MachineName string
	Namespace   string
	// Watch keeps polling and printing until the DeployRun reaches a
	// terminal phase (Succeeded/Failed), rather than printing once and
	// returning.
	Watch bool
	// PollInterval overrides how often Watch re-checks. The zero value
	// uses statusPollInterval; tests set a shorter interval.
	PollInterval time.Duration
	// Out receives one line per observed change. Required.
	Out io.Writer
}

// terminalDeployRunPhases are DeployRunStatus.Phase values Watch stops on.
var terminalDeployRunPhases = map[string]bool{
	keziov1alpha3.DeployRunPhaseSucceeded: true,
	keziov1alpha3.DeployRunPhaseFailed:    true,
}

// Status implements `kezioctl status`: it reports the named Machine's
// deploy progress by reading its current DeployRun - Machine.status
// carries no phase/progress of its own, only a reference to the DeployRun
// that does (see DeployRunStatus). The reference is chosen newest first:
// CurrentRunRef, so a run in progress is what gets reported; then
// LastAttemptedRunRef, which names the last run that ended whether it
// succeeded or failed; then LastSuccessfulRunRef. A Machine with no
// deploy ever requested has none of the three and reports that.
//
// Without Watch, this prints exactly one line and returns. With Watch, it
// polls (see statusPollInterval) - this codebase's established
// wait-for-a-change idiom (see ImageDelete's waitForImageGone) rather than
// a server-side watch, which nothing else in kezioctl uses - printing a
// new line only when the reported state actually changes, until the
// DeployRun reaches a terminal phase, ctx is canceled, or the Machine or
// DeployRun disappears.
func Status(ctx context.Context, c client.Client, opts StatusOptions) error {
	if opts.Out == nil {
		return errStatusRequiresOut
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = statusPollInterval
	}

	var lastPrinted string
	printIfChanged := func() (done bool, err error) {
		line, run, err := statusLine(ctx, c, opts)
		if err != nil {
			return false, err
		}
		if line != lastPrinted {
			_, _ = fmt.Fprintln(opts.Out, line)
			lastPrinted = line
		}
		return run != nil && terminalDeployRunPhases[run.Status.Phase], nil
	}

	done, err := printIfChanged()
	if err != nil {
		return err
	}
	if !opts.Watch || done {
		return nil
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("canceled while watching Machine %s/%s: %w", opts.Namespace, opts.MachineName, ctx.Err())
		case <-ticker.C:
			done, err := printIfChanged()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// statusLine fetches the Machine and (if it has one) its current or last
// DeployRun, and renders one report line. run is nil when the Machine has
// never had a deploy requested.
func statusLine(ctx context.Context, c client.Client, opts StatusOptions) (string, *keziov1alpha3.DeployRun, error) {
	machine := &keziov1alpha3.Machine{}
	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.MachineName}
	if err := c.Get(ctx, key, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil, fmt.Errorf("machine %s/%s not found", opts.Namespace, opts.MachineName)
		}
		return "", nil, fmt.Errorf("get Machine %s/%s: %w", opts.Namespace, opts.MachineName, err)
	}

	runRef := machine.Status.CurrentRunRef
	if runRef == nil {
		runRef = machine.Status.LastAttemptedRunRef
	}
	if runRef == nil {
		// Reached only for a Machine whose status an older controller
		// wrote: every write of lastSuccessfulRunRef also writes
		// lastAttemptedRunRef.
		runRef = machine.Status.LastSuccessfulRunRef
	}
	if runRef == nil {
		return fmt.Sprintf("machine %s/%s: state=%s (no deploy recorded)",
			machine.Namespace, machine.Name, orDash(machine.Status.State)), nil, nil
	}

	runNamespace := runRef.Namespace
	if runNamespace == "" {
		runNamespace = machine.Namespace
	}
	run := &keziov1alpha3.DeployRun{}
	runKey := client.ObjectKey{Namespace: runNamespace, Name: runRef.Name}
	if err := c.Get(ctx, runKey, run); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil, fmt.Errorf("deployrun %s/%s (referenced by Machine %s/%s) not found",
				runNamespace, runRef.Name, machine.Namespace, machine.Name)
		}
		return "", nil, fmt.Errorf("get DeployRun %s/%s: %w", runNamespace, runRef.Name, err)
	}

	succeeded := meta.FindStatusCondition(run.Status.Conditions, keziov1alpha3.DeployRunConditionSucceeded)
	succeededStr := "unknown"
	if succeeded != nil {
		succeededStr = string(succeeded.Status)
	}

	return fmt.Sprintf("machine %s/%s: state=%s deployrun=%s phase=%s succeeded=%s",
		machine.Namespace, machine.Name, orDash(machine.Status.State),
		run.Name, orDash(run.Status.Phase), succeededStr), run, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// errStatusRequiresOut guards Status against a caller that forgot to set
// Out, which would otherwise panic deep inside fmt.Fprintln.
var errStatusRequiresOut = errors.New("StatusOptions.Out must not be nil")
