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

package bootd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// DefaultProxyAddr is ProxyServer.Addr's default: DefaultProxyPort
// (identity.go) on whatever address the caller binds it to. Firmware and
// a live-booted agent both already carry an explicit
// "http://<host>:<port>" URL (BOOTD_BOOT_CONFIG_URL / kezio.server=), so
// the default only saves inventing a port.
var DefaultProxyAddr = fmt.Sprintf(":%d", DefaultProxyPort)

// bootRoutePrefix mirrors internal/bootserver's routes (all under
// "/boot/") without importing that package, avoiding a dependency on its
// controller-runtime client plumbing just for a string prefix.
const bootRoutePrefix = "/boot/"

// agentRoutePrefix mirrors the in-cluster agent server's routes (all
// under "/agent/"), same rationale as bootRoutePrefix.
const agentRoutePrefix = "/agent/"

// grubNetSearchRoutePrefix mirrors internal/bootserver's GET /grub/<name>
// route, same rationale as bootRoutePrefix.
const grubNetSearchRoutePrefix = "/grub/"

// httpBootRoutePrefix mirrors internal/bootserver's GET /boot/http/<name>
// route, the one this proxy hands a UEFI HTTP Boot client (see
// HTTPBootURL). It is a subtree of bootRoutePrefix, so nothing extra is
// routed for it.
const httpBootRoutePrefix = bootRoutePrefix + "http/"

// HTTPBootURL builds the Config.HTTPBootURL value for bootFilename served
// through this proxy: proxyAddr is where the proxy listens
// (ProxyConfig.Addr), fallbackHost the address to publish when that
// listen address names no host - ":80" binds every interface but tells a
// client nothing, and a URL is only useful if a machine on the
// provisioning segment can dial it. Port 80 is left out of the URL rather
// than spelled, since some firmware compares the host against the DHCP
// server's own address.
//
// Returns an error only for an unparseable proxyAddr; a caller that has
// no boot upstream to proxy should not call this at all, since the URL
// would 404 for every client that followed it.
func HTTPBootURL(proxyAddr, fallbackHost, bootFilename string) (string, error) {
	host, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return "", fmt.Errorf("proxy address %q: %w", proxyAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = fallbackHost
	}
	authority := host
	if port != "" && port != fmt.Sprint(DefaultProxyPort) {
		authority = net.JoinHostPort(host, port)
	}
	return "http://" + authority + httpBootRoutePrefix + bootFilename, nil
}

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
	// AgentUpstreamURL is the in-cluster agent server's base URL, for
	// example "http://kezio-agent-server.kezio-system.svc:8091". Every
	// request under agentRoutePrefix is forwarded here unchanged.
	// Empty disables agent proxying.
	AgentUpstreamURL string
	// BootUpstreamURL is internal/bootserver's in-cluster base URL, for
	// example "http://kezio-boot-server.kezio-system.svc:8090". Every
	// request under bootRoutePrefix is forwarded here unchanged. Empty
	// disables boot-server proxying.
	BootUpstreamURL string
}

// Enabled reports whether at least one of AgentUpstreamURL or
// BootUpstreamURL is set. cmd/bootd only adds a ProxyServer when true.
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
// and boot-config-serving APIs: it listens on the provisioning network
// and forwards agentRoutePrefix to the in-cluster agent server and
// bootRoutePrefix to internal/bootserver, so a net-booting machine
// reaches both through the one bootd address already advertised on the
// segment (BOOTD_SERVER_IP / BOOTD_NEXT_SERVER_IP), instead of needing a
// second in-cluster address per provisioning segment.
//
// Neither upstream is proxied unless its URL is configured
// (ProxyConfig.Enabled).
type ProxyServer struct {
	handler http.Handler
	addr    string

	// ready flips true once Start's net.Listen succeeds, false once
	// serving stops - for cmd/bootd's readyz check to know this front
	// can actually answer a request, not just that the process is up.
	ready atomic.Bool
}

var _ manager.Runnable = (*ProxyServer)(nil)

// NewProxyServer builds a ProxyServer from cfg. Errors on an upstream URL
// that fails to parse or names neither http nor https - the
// segment-facing side is always plain HTTP (firmware speaks nothing
// else), but an https upstream is accepted since a deployment may
// terminate TLS between bootd and its own upstream. Errors if
// cfg.Enabled() is false, since a ProxyServer with no route would
// silently 404 everything.
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
		// The netboot GRUB handed to UEFI HTTP Boot clients does not
		// search for its config under the directory it was fetched from:
		// Debian bakes in the prefix "/grub", applied against the server
		// root (a live machine at the resulting prompt reports
		// $prefix=(http,192.0.2.2)/grub). Its whole config search lands
		// outside bootRoutePrefix, and without this route every probe died
		// here as an unlogged 404 while both ends' logs stayed empty.
		mux.Handle(grubNetSearchRoutePrefix, proxy)
	}

	return &ProxyServer{handler: mux, addr: cfg.addr()}, nil
}

// newReverseProxy builds a *httputil.ReverseProxy forwarding to rawURL
// unchanged: the default Director only rewrites scheme/host/path-prefix,
// and rawURL carries no path prefix, so the upstream sees the same
// request path bootd received.
func newReverseProxy(rawURL string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	if target.Scheme != httpScheme && target.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q is not http or https", target.Scheme)
	}
	if target.Host == "" {
		return nil, errors.New("missing host")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Explicit 502 handler so upstream failures log through this
	// package's logger instead of the stdlib's global log.Printf.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logf.FromContext(r.Context()).WithName("bootd-proxy").
			Error(err, "proxying request to upstream failed", "upstream", rawURL, "path", r.URL.Path)
		w.WriteHeader(http.StatusBadGateway)
	}
	return proxy, nil
}

// Start implements manager.Runnable: serves Handler on Addr until ctx is
// cancelled, then shuts down gracefully - same shape as
// internal/bootserver.Server.Start. The listener is bound explicitly with
// a log line on both sides, so a bind failure is visible in bootd's own
// log rather than surfacing only as a client timeout minutes later.
func (p *ProxyServer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-proxy")

	log.Info("starting server", "name", "boot config proxy", "addr", p.addr)
	listener, err := net.Listen("tcp", p.addr)
	if err != nil {
		log.Error(err, "boot config proxy failed to bind; every /agent/... and /boot/... "+
			"request on this address will go unanswered until this is fixed", "addr", p.addr)
		return fmt.Errorf("binding boot config proxy to %s: %w", p.addr, err)
	}
	p.ready.Store(true)
	defer p.ready.Store(false)

	httpServer := &http.Server{
		Handler:           p.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		log.Error(err, "boot config proxy stopped serving unexpectedly", "addr", p.addr)
		return err
	}
}

// Ready reports whether Start has bound its listener and is actively
// serving - false before bind succeeds, after it fails, or once serving
// stops, so a readiness check never reports healthy while this proxy
// can't answer a request.
func (p *ProxyServer) Ready() bool {
	return p.ready.Load()
}

// Handler returns the routed http.Handler, exported for tests.
func (p *ProxyServer) Handler() http.Handler {
	return p.handler
}
