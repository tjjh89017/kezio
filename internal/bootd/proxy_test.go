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

package bootd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordedRequest captures everything an upstream test server observed
// about one proxied request, so assertions can check the exact bytes
// bootd's proxy forwarded rather than merely that a request arrived.
type recordedRequest struct {
	method string
	path   string
	query  string
	body   string
	header http.Header
}

// newRecordingUpstream returns an httptest.Server that records every
// request it receives into *got (overwriting on each call - every test
// below sends exactly one request per upstream) and answers with a fixed
// body and status.
func newRecordingUpstream(t *testing.T, status int, respBody string, got *recordedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading upstream request body: %v", err)
		}
		*got = recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   string(body),
			header: r.Header.Clone(),
		}
		w.Header().Set("X-Upstream-Header", "present")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

// TestNewProxyServer_RequiresAtLeastOneUpstream pins ProxyConfig's
// opt-in contract: a caller must not be able to build a ProxyServer with
// nothing configured to proxy.
func TestNewProxyServer_RequiresAtLeastOneUpstream(t *testing.T) {
	if _, err := NewProxyServer(ProxyConfig{}); err == nil {
		t.Fatal("expected an error building a ProxyServer with no upstream URL set")
	}
}

// TestProxyConfig_Enabled pins the rule cmd/bootd relies on: enabled iff
// at least one upstream URL is set.
func TestProxyConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProxyConfig
		want bool
	}{
		{"zero value", ProxyConfig{}, false},
		{"agent only", ProxyConfig{AgentUpstreamURL: "http://agent.example:8091"}, true},
		{"boot only", ProxyConfig{BootUpstreamURL: "http://boot.example:8090"}, true},
		{"both", ProxyConfig{AgentUpstreamURL: "http://agent.example:8091", BootUpstreamURL: "http://boot.example:8090"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewProxyServer_RejectsInvalidUpstreamURL: a malformed or non-HTTP(S)
// upstream URL is rejected at construction, not discovered later as every
// request silently 502ing.
func TestNewProxyServer_RejectsInvalidUpstreamURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProxyConfig
	}{
		{"malformed agent URL", ProxyConfig{AgentUpstreamURL: "://not-a-url"}},
		{"malformed boot URL", ProxyConfig{BootUpstreamURL: "://not-a-url"}},
		{"non-http scheme", ProxyConfig{AgentUpstreamURL: "ftp://agent.example:21"}},
		{"no host", ProxyConfig{BootUpstreamURL: "http://"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewProxyServer(tc.cfg); err == nil {
				t.Errorf("expected an error building a ProxyServer with %+v", tc.cfg)
			}
		})
	}
}

// TestProxyServer_ForwardsAgentRoutes proves every part of an /agent/...
// request - method, path, query, body, and a custom header - reaches the
// agent upstream unchanged, and that the upstream's response (status,
// body, and its own header) comes back unchanged too.
func TestProxyServer_ForwardsAgentRoutes(t *testing.T) {
	var got recordedRequest
	agentUpstream := newRecordingUpstream(t, http.StatusCreated, `{"ok":true}`, &got)
	defer agentUpstream.Close()

	srv, err := NewProxyServer(ProxyConfig{AgentUpstreamURL: agentUpstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/agent/machines/node-01/progress?x=1",
		strings.NewReader(`{"step":"WritingContent"}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-session-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("upstream saw method %q, want POST", got.method)
	}
	if got.path != "/agent/machines/node-01/progress" {
		t.Errorf("upstream saw path %q, want /agent/machines/node-01/progress", got.path)
	}
	if got.query != "x=1" {
		t.Errorf("upstream saw query %q, want x=1", got.query)
	}
	if got.body != `{"step":"WritingContent"}` {
		t.Errorf("upstream saw body %q", got.body)
	}
	if got.header.Get("Authorization") != "Bearer test-session-token" {
		t.Errorf("upstream saw Authorization %q", got.header.Get("Authorization"))
	}
	if got.header.Get("Content-Type") != "application/json" {
		t.Errorf("upstream saw Content-Type %q", got.header.Get("Content-Type"))
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("response status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if string(respBody) != `{"ok":true}` {
		t.Errorf("response body = %q", respBody)
	}
	if resp.Header.Get("X-Upstream-Header") != "present" {
		t.Errorf("response missing upstream header, got headers %v", resp.Header)
	}
}

// TestProxyServer_ForwardsBootRoutes proves the boot-server prefix
// forwards a plain GET (GRUB's own shape) the same way
// TestProxyServer_ForwardsAgentRoutes proves it for the agent prefix.
func TestProxyServer_ForwardsBootRoutes(t *testing.T) {
	var got recordedRequest
	bootUpstream := newRecordingUpstream(t, http.StatusOK, "configfile x\n", &got)
	defer bootUpstream.Close()

	srv, err := NewProxyServer(ProxyConfig{BootUpstreamURL: bootUpstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/boot/grub.cfg-aa:bb:cc:dd:ee:01")
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	if got.method != http.MethodGet {
		t.Errorf("upstream saw method %q, want GET", got.method)
	}
	if got.path != "/boot/grub.cfg-aa:bb:cc:dd:ee:01" {
		t.Errorf("upstream saw path %q", got.path)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("response status = %d, want 200", resp.StatusCode)
	}
	if string(respBody) != "configfile x\n" {
		t.Errorf("response body = %q", respBody)
	}
}

// TestProxyServer_StreamsResponseBody proves a large/streamed body (like a
// kernel/initrd/squashfs download) passes through intact.
func TestProxyServer_StreamsResponseBody(t *testing.T) {
	// Bigger than any default buffer stage, and a repeating
	// position-dependent pattern so byte-level corruption is caught.
	want := bytes.Repeat([]byte("kezio-live-artifact-stream-"), 8192)

	bootUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	}))
	defer bootUpstream.Close()

	srv, err := NewProxyServer(ProxyConfig{BootUpstreamURL: bootUpstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/boot/artifacts/filesystem.squashfs")
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("streamed body length = %d, want %d (or content differs)", len(got), len(want))
	}
}

// TestProxyServer_MapsUpstreamErrorToBadGateway proves an unreachable
// upstream (connection refused - the upstream server below is created
// and immediately closed, so its address is guaranteed to refuse) is
// mapped to a 502, not a hung request or a 200 with an empty body.
func TestProxyServer_MapsUpstreamErrorToBadGateway(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	srv, err := NewProxyServer(ProxyConfig{AgentUpstreamURL: deadURL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/agent/register")
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("response status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// TestProxyServer_OnlyProxiesConfiguredPrefix proves the two upstreams
// are independent: only the agent upstream configured leaves /boot/...
// a plain 404, not a fallthrough to the agent upstream.
func TestProxyServer_OnlyProxiesConfiguredPrefix(t *testing.T) {
	var got recordedRequest
	agentUpstream := newRecordingUpstream(t, http.StatusOK, "ok", &got)
	defer agentUpstream.Close()

	srv, err := NewProxyServer(ProxyConfig{AgentUpstreamURL: agentUpstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/boot/grub.cfg-aa:bb:cc:dd:ee:01")
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("response status = %d, want 404 (no boot upstream configured)", resp.StatusCode)
	}
}

// TestProxyServer_Addr pins ProxyConfig.Addr's default (DefaultProxyAddr
// when unset) vs an explicit value.
func TestProxyServer_Addr(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()

	srv, err := NewProxyServer(ProxyConfig{AgentUpstreamURL: upstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if srv.addr != DefaultProxyAddr {
		t.Errorf("addr = %q, want default %q", srv.addr, DefaultProxyAddr)
	}

	const explicit = "192.0.2.2:8080"
	srv, err = NewProxyServer(ProxyConfig{AgentUpstreamURL: upstream.URL, Addr: explicit})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if srv.addr != explicit {
		t.Errorf("addr = %q, want %q", srv.addr, explicit)
	}
}

// TestProxyServer_StartServesAndShutsDown exercises the manager.Runnable
// contract: Start binds Addr, serves, and returns cleanly on cancel.
func TestProxyServer_StartServesAndShutsDown(t *testing.T) {
	var got recordedRequest
	agentUpstream := newRecordingUpstream(t, http.StatusOK, "ok", &got)
	defer agentUpstream.Close()

	srv, err := NewProxyServer(ProxyConfig{Addr: "127.0.0.1:0", AgentUpstreamURL: agentUpstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}

	// Addr uses an ephemeral port Start doesn't report back, so exercise
	// Handler() directly for the request/response assertion and leave
	// Start's bind/serve/shutdown contract to the ctx-cancel check below.
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	resp, err := http.Get(front.URL + "/agent/register")
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("response status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	// Give Start a moment to reach ListenAndServe before cancelling -
	// this only needs Start to have entered its select, not the listener
	// to be dial-able, since nothing here dials it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after context cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s of context cancellation")
	}
}

// TestProxyServer_ReadyReflectsListenState: Ready is false before Start
// runs, true once the listener is bound, false again once serving stops.
func TestProxyServer_ReadyReflectsListenState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()

	srv, err := NewProxyServer(ProxyConfig{Addr: "127.0.0.1:0", AgentUpstreamURL: upstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}

	if srv.Ready() {
		t.Fatal("Ready() = true before Start was ever called")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for !srv.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("Ready() never became true within 2s of Start")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after context cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s of context cancellation")
	}

	if srv.Ready() {
		t.Error("Ready() = true after Start returned, want false")
	}
}

// TestProxyServer_StartReturnsErrorOnBindFailure proves a bind failure
// (address already in use) is reported as an error, not silently ignored,
// and Ready stays false.
func TestProxyServer_StartReturnsErrorOnBindFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port to collide with: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	srv, err := NewProxyServer(ProxyConfig{Addr: occupied.Addr().String(), AgentUpstreamURL: upstream.URL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}

	err = srv.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil binding to an address already in use, want an error")
	}
	if srv.Ready() {
		t.Error("Ready() = true after a failed bind, want false")
	}
}
