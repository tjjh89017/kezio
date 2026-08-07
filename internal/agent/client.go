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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// Client talks to the controller's agent registration endpoint
// (internal/agentserver on the other end).
type Client struct {
	// HTTPClient performs the requests. nil means http.DefaultClient
	// wrapped with a sane per-request timeout - see NewClient.
	HTTPClient *http.Client
	// BaseURL is the controller's externally reachable base URL, the
	// kezio.server= cmdline value (Cmdline.Server).
	BaseURL string
}

// NewClient builds a Client with a bounded per-request timeout: the live
// environment has no other work competing for the connection, so a
// generous but finite timeout (long enough for a slow link, short enough
// that a wedged TCP connection does not hang the agent forever) is
// enough defense against a network that accepts the connection but never
// answers.
func NewClient(baseURL string) *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		BaseURL:    baseURL,
	}
}

// RegisterResult is what a successful Register call returns: the
// machine name subsequent Next calls target, and the session credential
// they authenticate with.
type RegisterResult struct {
	// MachineName is the name the presented boot token resolved to.
	MachineName string
	// SessionToken is the short-lived credential returned exactly once
	// by this call; Next presents it as a Bearer token on every poll.
	SessionToken string
}

// Register posts the collected hardware inventory to the controller,
// authenticating with the single-use boot token. It returns the machine
// name the controller resolved the token to and the session token that
// authenticates subsequent Next calls.
func (c *Client) Register(ctx context.Context, token string, hardware *keziov1alpha1.MachineHardwareStatus) (RegisterResult, error) {
	body, err := json.Marshal(agentapi.RegisterRequest{Hardware: *hardware})
	if err != nil {
		return RegisterResult{}, fmt.Errorf("encoding registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(agentapi.RegisterPath), bytes.NewReader(body))
	if err != nil {
		return RegisterResult{}, fmt.Errorf("building registration request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("registration request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return RegisterResult{}, fmt.Errorf("registration rejected: %s", statusSummary(resp))
	}

	var out agentapi.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return RegisterResult{}, fmt.Errorf("decoding registration response: %w", err)
	}
	if out.MachineName == "" {
		return RegisterResult{}, fmt.Errorf("registration response has no machine name")
	}
	if out.SessionToken == "" {
		return RegisterResult{}, fmt.Errorf("registration response has no session token")
	}
	return RegisterResult{MachineName: out.MachineName, SessionToken: out.SessionToken}, nil
}

// Next polls the controller for this machine's next action,
// authenticating with the session token Register returned. It returns
// the decoded NextResponse: Action is agentapi.ActionWait or
// agentapi.ActionDeploy, with Plan populated in the latter case. Every
// call advertises this agent's supported plan schema version
// (agentapi.AgentSchemaVersionHeader, agentapi.PlanSchemaVersion) so the
// controller can withhold a plan this build would misread - see
// agentapi's package doc comment for the full versioning rule.
func (c *Client) Next(ctx context.Context, machineName, sessionToken string) (agentapi.NextResponse, error) {
	path := agentapi.NextPathPrefix + machineName + agentapi.NextPathSuffix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return agentapi.NextResponse{}, fmt.Errorf("building poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.PlanSchemaVersion))

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return agentapi.NextResponse{}, fmt.Errorf("poll request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return agentapi.NextResponse{}, fmt.Errorf("poll rejected: %s", statusSummary(resp))
	}

	var out agentapi.NextResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentapi.NextResponse{}, fmt.Errorf("decoding poll response: %w", err)
	}
	return out, nil
}

// ReportProgress posts a snapshot of a running deploy's progress - per-
// partition detail plus the whole-plan step - to the controller,
// authenticating with the session token the same way Next does. It is
// best-effort from the caller's perspective - internal/agent/deploy.
// Executor logs (and otherwise ignores) an error returned here rather
// than failing an in-progress deployment over a missed progress update.
func (c *Client) ReportProgress(ctx context.Context, machineName, sessionToken string, req agentapi.ProgressRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding progress request: %w", err)
	}

	path := agentapi.NextPathPrefix + machineName + agentapi.ProgressPathSuffix
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building progress request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+sessionToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("progress request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("progress report rejected: %s", statusSummary(resp))
	}
	return nil
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// statusSummary formats a non-2xx response for an error message,
// including a bounded prefix of the body (an ErrorResponse.Error value,
// ordinarily short) without risking an unbounded read of a hostile or
// misbehaving server's response.
func statusSummary(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
}
