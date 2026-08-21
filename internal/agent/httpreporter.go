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
	"sync"
	"time"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// httpReporterMaxAttempts bounds Report's retry loop against a transport
// error (the controller unreachable, a connection reset): a report that
// never lands must not hang the deploy step it describes forever, but a
// single failed attempt is worth a couple of quick retries before giving
// up on it.
const httpReporterMaxAttempts = 3

// httpReporterRetryDelay is the fixed delay between Report's retry
// attempts.
const httpReporterRetryDelay = 2 * time.Second

// HTTPReporter is a deploy.Reporter that posts every report to the
// controller's POST /agent/progress and cancels the running deploy when
// the controller answers ProgressActionAbort. It never returns a
// sentinel error for the Executor to interpret: cancelling Cancel is
// itself the whole abort mechanism, so nothing in internal/agent/deploy
// needs to know abort exists.
type HTTPReporter struct {
	// Client posts the report. Required.
	Client *Client
	// SessionToken authenticates every report. Set once, right after
	// registration (see Config.OnRegistered) and before any deploy can
	// start - safe to read here without a lock since Report is only ever
	// called after that point.
	SessionToken string
	// Cancel stops the deploy in progress once the controller answers
	// abort. Set by the caller immediately before each Execute call (see
	// cmd/agent); nil is a safe no-op, for a test that does not need
	// cancellation.
	Cancel context.CancelFunc
	// Logf receives progress and diagnostic messages in fmt.Printf
	// style. Nil discards them.
	Logf func(format string, args ...any)
	// RetryDelay overrides httpReporterRetryDelay between retry attempts.
	// Zero means httpReporterRetryDelay; tests set this short so a
	// transport-error test does not have to wait out the real delay.
	RetryDelay time.Duration

	// aborted guards Cancel so a controller that keeps answering abort on
	// every subsequent report (including the deploy's own final failed
	// report) never calls it more than once.
	aborted sync.Once
}

var _ deploy.Reporter = (*HTTPReporter)(nil)

// Report implements deploy.Reporter. It always posts against a fresh,
// non-cancelled context: ctx is the deploy's own context, which Cancel
// may have already cancelled by the time this runs (in particular for
// the deploy's final failed report, sent from the very cancellation this
// reporter triggered) - binding the outgoing request to ctx would then
// make that report fail before it ever reaches the wire, exactly when it
// matters most.
func (r *HTTPReporter) Report(ctx context.Context, req agentapi.ProgressRequest) error {
	var lastErr error
	for attempt := range httpReporterMaxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(r.retryDelay()):
			}
		}

		resp, err := r.Client.Progress(context.Background(), r.SessionToken, req)
		if err != nil {
			lastErr = err
			r.log("posting progress report failed (attempt %d/%d): %v", attempt+1, httpReporterMaxAttempts, err)
			continue
		}

		if resp.Action == agentapi.ProgressActionAbort {
			r.aborted.Do(func() {
				r.log("controller requested abort; cancelling the running deploy")
				if r.Cancel != nil {
					r.Cancel()
				}
			})
		}
		return nil
	}
	return lastErr
}

func (r *HTTPReporter) retryDelay() time.Duration {
	if r.RetryDelay > 0 {
		return r.RetryDelay
	}
	return httpReporterRetryDelay
}

func (r *HTTPReporter) log(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}
