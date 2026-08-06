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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/tjjh89017/kezio/internal/agentapi"
)

// init verifies agentRoutePrefix actually covers every route
// internal/agentapi defines, so the two can never silently drift apart -
// a new agentapi route added outside "/agent/" would otherwise proxy as
// a plain 404 with nothing to explain why.
func init() {
	if !strings.HasPrefix(agentapi.RegisterPath, agentRoutePrefix) {
		panic(fmt.Sprintf("bootd: agentRoutePrefix %q does not cover agentapi.RegisterPath %q", agentRoutePrefix, agentapi.RegisterPath))
	}
	if !strings.HasPrefix(agentapi.NextPathPrefix, agentRoutePrefix) {
		panic(fmt.Sprintf("bootd: agentRoutePrefix %q does not cover agentapi.NextPathPrefix %q", agentRoutePrefix, agentapi.NextPathPrefix))
	}
}

// DefaultProxyAddr is ProxyServer.Addr's default: port 80 on whatever
// address the caller binds it to. Firmware and a live-booted agent both
// already carry an explicit "http://<host>:<port>" URL in their kernel
// cmdline (BOOTD_BOOT_CONFIG_URL / kezio.server=), so there is nothing
// implicit about the port they use - the default only saves a site the
// trouble of inventing one.
const DefaultProxyAddr = ":80"

// bootRoutePrefix and agentRoutePrefix are the two path prefixes
// ProxyServer's handler dispatches on. bootRoutePrefix mirrors
// internal/bootserver's route shapes (GET /boot/grub.cfg-<mac>, GET
// /boot/artifacts/..., GET /boot/http/<name> - see that package's doc
// comment) without importing internal/bootserver: every one of its
// routes already lives under "/boot/", so forwarding the whole prefix
// needs no per-route knowledge here, and it would otherwise pull this
// package's binary into depending on bootserver's controller-runtime
// client plumbing for a plain string prefix.
const bootRoutePrefix = "/boot/"

// agentRoutePrefix mirrors agentapi.NextPathPrefix's parent path
// ("/agent/machines/...") plus agentapi.RegisterPath ("/agent/register")
// - every agentserver route lives under "/agent/", so, like
// bootRoutePrefix, forwarding the whole prefix is exact without
// depending on agentserver itself.
const agentRoutePrefix = "/agent/"

// ProxyConfig configures ProxyServer. Both upstream URLs are optional and
// independent: a deployment can front only the agent API, only the boot
// server, or both. Leaving both empty means the proxy is disabled
// entirely - see NewProxyServer.
type ProxyConfig struct {
	// Addr is the address ProxyServer listens on, for example
	// "192.0.2.2:80" (the provisioning interface's own address, not
	// every interface the pod happens to have - see cmd/bootd's
	// bootdConfigFromEnv for why the default binds to BOOTD_SERVER_IP
	// specifically). Empty means DefaultProxyAddr.
	Addr string
	// AgentUpstreamURL is internal/agentserver's in-cluster base URL,
	// for example "http://kezio-agent-server.kezio-system.svc:8091".
	// Every request under agentRoutePrefix is forwarded here unchanged.
	// Empty disables agent proxying.
	AgentUpstreamURL string
	// BootUpstreamURL is internal/bootserver's in-cluster base URL, for
	// example "http://kezio-boot-server.kezio-system.svc:8090". Every
	// request under bootRoutePrefix is forwarded here unchanged. Empty
	// disables boot-server proxying.
	BootUpstreamURL string
	// VRFDevice, when set, binds the listening socket to this device
	// (SO_BINDTODEVICE, see bindToDeviceControl) before it binds to
	// Addr, so the listener's routing follows that device's VRF table
	// instead of the pod's default one. Set this to
	// ProvisioningVRFName exactly when Config.ProvisioningGateway is
	// configured (see cmd/bootd). Empty (the default) binds the
	// socket exactly as before this field existed.
	VRFDevice string
}

// Enabled reports whether ProxyServer has anything to serve: at least one
// of AgentUpstreamURL or BootUpstreamURL is set. cmd/bootd only adds a
// ProxyServer to the manager when this is true, so a bootd deployment
// that sets neither variable behaves exactly as it did before this type
// existed.
func (c ProxyConfig) Enabled() bool {
	return c.AgentUpstreamURL != "" || c.BootUpstreamURL != ""
}

func (c ProxyConfig) addr() string {
	if c.Addr != "" {
		return c.Addr
	}
	return DefaultProxyAddr
}

// ProxyServer is bootd's HTTP reverse-proxy front for the agent-facing
// and boot-config-serving HTTP APIs: it listens on the provisioning
// network and forwards agentRoutePrefix to internal/agentserver and
// bootRoutePrefix to internal/bootserver, so a net-booting machine (and
// the agent it boots into) reaches both through the one bootd address
// already advertised on the segment (BOOTD_SERVER_IP / BOOTD_NEXT_SERVER_IP)
// instead of needing a second, separately-reachable in-cluster address
// per provisioning segment.
//
// Neither upstream is proxied unless its URL is configured
// (ProxyConfig.Enabled): a bootd deployment that predates this type, or
// that deliberately fronts the agent/boot servers some other way, is
// unaffected by adding it.
type ProxyServer struct {
	handler   http.Handler
	addr      string
	vrfDevice string
}

var _ manager.Runnable = (*ProxyServer)(nil)

// NewProxyServer builds a ProxyServer from cfg. It returns an error for a
// configured upstream URL that does not parse, or that names neither
// http nor https - firmware and the agent both speak plain HTTP only
// (see internal/bootserver's Server.Start doc comment on why boot-time
// serving is unauthenticated HTTP), so an https upstream is accepted
// only because a deployment may still terminate TLS between bootd and
// its own upstream even though the segment-facing side never does.
// Calling this with cfg.Enabled() false returns an error: cmd/bootd only
// calls it once it has already decided the proxy is wanted, and a
// ProxyServer with no route to serve would silently 404 every request
// bootRoutePrefix/agentRoutePrefix ever match.
func NewProxyServer(cfg ProxyConfig) (*ProxyServer, error) {
	if !cfg.Enabled() {
		return nil, errors.New("bootd: NewProxyServer requires at least one of AgentUpstreamURL or BootUpstreamURL")
	}

	mux := http.NewServeMux()
	if cfg.AgentUpstreamURL != "" {
		proxy, err := newReverseProxy(cfg.AgentUpstreamURL)
		if err != nil {
			return nil, fmt.Errorf("agent upstream URL %q: %w", cfg.AgentUpstreamURL, err)
		}
		mux.Handle(agentRoutePrefix, proxy)
	}
	if cfg.BootUpstreamURL != "" {
		proxy, err := newReverseProxy(cfg.BootUpstreamURL)
		if err != nil {
			return nil, fmt.Errorf("boot upstream URL %q: %w", cfg.BootUpstreamURL, err)
		}
		mux.Handle(bootRoutePrefix, proxy)
	}

	return &ProxyServer{handler: mux, addr: cfg.addr(), vrfDevice: cfg.VRFDevice}, nil
}

// newReverseProxy builds a *httputil.ReverseProxy forwarding to rawURL
// unchanged (path, method, headers, and body all pass through - the
// default Director httputil.NewSingleHostReverseProxy installs rewrites
// only the scheme/host/path-prefix, and rawURL carries no path prefix in
// every deployment this package ships, so the request path served to the
// upstream is byte-identical to the one bootd received).
func newReverseProxy(rawURL string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q is not http or https", target.Scheme)
	}
	if target.Host == "" {
		return nil, errors.New("missing host")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Map every upstream failure (connection refused, timeout, upstream
	// closing early mid-response) onto a plain 502 - the same outcome
	// net/http/httputil's own default ErrorHandler produces, made
	// explicit here so it is logged through this package's own logger
	// instead of the standard library's global log.Printf, matching
	// internal/bootserver's and internal/agentserver's handlers.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logf.FromContext(r.Context()).WithName("bootd-proxy").
			Error(err, "proxying request to upstream failed", "upstream", rawURL, "path", r.URL.Path)
		w.WriteHeader(http.StatusBadGateway)
	}
	return proxy, nil
}

// Start implements manager.Runnable: it serves Handler on Addr until ctx
// is cancelled, then shuts the server down gracefully - the same
// listen/shutdown shape internal/bootserver.Server.Start and
// internal/agentserver.Server.Start use.
//
// The listener is built explicitly (rather than via
// http.Server.ListenAndServe) so vrfDevice can be applied to it: empty,
// net.ListenConfig.Control is nil and the resulting listener is
// identical to what ListenAndServe would have produced.
func (p *ProxyServer) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	if p.vrfDevice != "" {
		lc.Control = bindToDeviceControl(p.vrfDevice)
	}
	ln, err := lc.Listen(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", p.addr, err)
	}

	httpServer := &http.Server{
		Handler:           p.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler returns the routed http.Handler, exported for tests.
func (p *ProxyServer) Handler() http.Handler {
	return p.handler
}
