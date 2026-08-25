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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
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
// machine name and session token the poll loop needs.
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
func (c *Client) Register(ctx context.Context, token string, hardware keziov1alpha3.MachineHardwareSpec) (RegisterResult, error) {
	body, err := json.Marshal(agentapi.RegisterRequest{Hardware: hardware})
	if err != nil {
		return RegisterResult{}, fmt.Errorf("encoding registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(agentapi.RegisterPath), bytes.NewReader(body))
	if err != nil {
		return RegisterResult{}, fmt.Errorf("building registration request: %w", err)
	}
	c.setCommonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

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
// authenticating with the session token Register returned.
func (c *Client) Next(ctx context.Context, sessionToken string) (agentapi.NextResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(agentapi.NextPath), nil)
	if err != nil {
		return agentapi.NextResponse{}, fmt.Errorf("building poll request: %w", err)
	}
	c.setCommonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+sessionToken)

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

// Progress posts one deploy-step report to the controller, authenticating
// with the session token Register returned.
func (c *Client) Progress(ctx context.Context, sessionToken string, req agentapi.ProgressRequest) (agentapi.ProgressResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return agentapi.ProgressResponse{}, fmt.Errorf("encoding progress request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(agentapi.ProgressPath), bytes.NewReader(body))
	if err != nil {
		return agentapi.ProgressResponse{}, fmt.Errorf("building progress request: %w", err)
	}
	c.setCommonHeaders(httpReq)
	httpReq.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return agentapi.ProgressResponse{}, fmt.Errorf("progress request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return agentapi.ProgressResponse{}, fmt.Errorf("progress report rejected: %s", statusSummary(resp))
	}

	var out agentapi.ProgressResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentapi.ProgressResponse{}, fmt.Errorf("decoding progress response: %w", err)
	}
	return out, nil
}

// setCommonHeaders sets the headers every request this client sends
// carries, regardless of endpoint: AgentSchemaVersionHeader, so the
// controller can tell which wire schema this agent build speaks, and
// Content-Type for the requests that have a body (harmless on the GET
// request Next sends).
func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set(agentapi.AgentSchemaVersionHeader, strconv.Itoa(agentapi.AgentSchemaVersion))
	req.Header.Set("Content-Type", "application/json")
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
