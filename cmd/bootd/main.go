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

// Command bootd runs kezio-bootd: the dnsmasq-based proxyDHCP service
// and the TFTP server a UEFI firmware talks to at the start of a
// network boot (see internal/bootd's package doc comment for the full
// design). Unlike the controller-manager binary (cmd/main.go), bootd
// does not reconcile anything - it runs a minimal controller-runtime
// manager purely to get a live-updated cache of Machine boot MAC
// addresses (internal/bootd's MACCache), with no leader election, no
// webhook server, and metrics disabled by default, since none of those
// apply to a per-site, non-reconciling process that is meant to run one
// replica per boot segment, not compete for leadership across sites.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/bootd"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(keziov1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg, err := bootdConfigFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid bootd configuration")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// No leader election: bootd is meant to run one replica per
		// boot segment (see config/bootd's README), not race other
		// sites' bootd instances for a single active/passive lease.
		LeaderElection: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	dnsmasq := &bootd.Dnsmasq{
		Config:     cfg.Server,
		RunDir:     cfg.RunDir,
		BinaryPath: cfg.DnsmasqPath,
	}

	macCache, err := bootd.NewMACCache(ctx, mgr.GetCache(), dnsmasq)
	if err != nil {
		setupLog.Error(err, "unable to set up Machine MAC cache")
		os.Exit(1)
	}
	if err := mgr.Add(macCache); err != nil {
		setupLog.Error(err, "unable to add Machine MAC cache")
		os.Exit(1)
	}

	if err := mgr.Add(dnsmasq); err != nil {
		setupLog.Error(err, "unable to add dnsmasq supervisor")
		os.Exit(1)
	}

	if cfg.GrubConfig == "" {
		// Not fatal: DHCP/TFTP for shim+grub still work, but the GRUB
		// image has no config to source, so every net boot stalls at
		// the grub> prompt - worth one loud line at startup.
		setupLog.Info("BOOTD_BOOT_CONFIG_URL is not set; " +
			"grub/grub.cfg will not be served and netbooting machines will stop at the GRUB prompt")
	}
	tftpServer := &bootd.TFTPServer{
		Dir:        cfg.TFTPDir,
		Addr:       cfg.TFTPAddr,
		GrubConfig: cfg.GrubConfig,
	}
	if err := mgr.Add(tftpServer); err != nil {
		setupLog.Error(err, "unable to add TFTP server")
		os.Exit(1)
	}

	if cfg.Proxy.Enabled() {
		proxy, err := bootd.NewProxyServer(cfg.Proxy)
		if err != nil {
			setupLog.Error(err, "invalid bootd proxy configuration")
			os.Exit(1)
		}
		if err := mgr.Add(proxy); err != nil {
			setupLog.Error(err, "unable to add proxy server")
			os.Exit(1)
		}
	}

	setupLog.Info("starting bootd",
		"answerAll", cfg.Server.AnswerAll,
		"tftpDir", cfg.TFTPDir,
		"provisioningNet", cfg.Server.ProvisioningNet.String(),
		"relay", cfg.Server.RelayServerIP,
		"leaseMode", cfg.Server.LeaseMode)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running bootd")
		os.Exit(1)
	}
}

// bootdConfig is bootd's environment-derived configuration.
type bootdConfig struct {
	Server  bootd.Config
	TFTPDir string
	// TFTPAddr optionally overrides bootd.DefaultTFTPAddr.
	TFTPAddr string
	// RunDir optionally overrides bootd.DefaultRunDir.
	RunDir string
	// DnsmasqPath optionally overrides bootd.DefaultDnsmasqPath.
	DnsmasqPath string
	// GrubConfig is the rendered GRUB bootstrap config the TFTP server
	// hands the netboot GRUB image (see bootd.GrubConfigPath); empty
	// when BOOTD_BOOT_CONFIG_URL is unset.
	GrubConfig string
	// Proxy configures bootd's optional reverse-proxy front for the
	// in-cluster agent and boot config servers (see bootd.ProxyServer).
	// Proxy.Enabled() is false - the zero value - unless
	// BOOTD_AGENT_UPSTREAM_URL or BOOTD_BOOT_UPSTREAM_URL is set.
	Proxy bootd.ProxyConfig
}

// bootdConfigFromEnv builds bootdConfig from the process environment.
// Every variable below is required unless noted, since bootd - unlike
// the controller-manager's optional bootserver/seeder subsystems - has
// no useful inert default: a bootd process whose dnsmasq cannot answer
// the provisioning segment is not doing anything at all.
//
//   - BOOTD_SERVER_IP: bootd's own IPv4 address on the provisioning
//     network, advertised as the PXE boot server (unless
//     BOOTD_NEXT_SERVER_IP overrides it) and used as the relay source
//     address when relaying is enabled. Required.
//   - BOOTD_PROVISIONING_CIDR: the provisioning network's IPv4 subnet
//     in CIDR form, for example "192.0.2.0/24" - rendered as dnsmasq's
//     proxyDHCP dhcp-range. Required.
//   - BOOTD_DHCP_INTERFACE: optional interface name dnsmasq binds its
//     DHCP sockets to exclusively (the pod's provisioning attachment,
//     typically "net1"). Unset means dnsmasq listens on every
//     interface in the pod's network namespace.
//   - BOOTD_DHCP_RELAY_SERVER: optional IPv4 address of the site's
//     existing DHCP server. Set, bootd's dnsmasq additionally relays
//     every DHCP request on the segment to it (dhcp-relay), so a
//     provisioning segment without its own DHCP server still gets
//     leases. Unset (the default): proxyDHCP only, bootd never touches
//     lease traffic. Mutually exclusive with BOOTD_LEASE_MODE.
//   - BOOTD_LEASE_MODE: set to "true" to make bootd's dnsmasq the
//     segment's own DHCP lease server instead of a proxyDHCP front,
//     for a provisioning segment with no DHCP server of its own. The
//     MAC gate is unchanged: only enrolled MACs receive a lease, the
//     same as they receive PXE info in proxy mode - see
//     internal/bootd.Config.LeaseMode. Defaults to "false". Mutually
//     exclusive with BOOTD_DHCP_RELAY_SERVER.
//   - BOOTD_LEASE_RANGE_START, BOOTD_LEASE_RANGE_END: optional IPv4
//     bounds for the BOOTD_LEASE_MODE dhcp-range. Both unset (the
//     default) auto-derives the range from BOOTD_PROVISIONING_CIDR's
//     first and last host addresses. Setting only one is an error.
//     Ignored unless BOOTD_LEASE_MODE is set.
//   - BOOTD_NEXT_SERVER_IP: overrides the PXE boot-server address
//     handed to clients, when the TFTP service is reachable at a
//     different address than BOOTD_SERVER_IP. Optional, defaults to
//     BOOTD_SERVER_IP.
//   - BOOTD_TFTP_DIR: local directory containing shimx64.efi and
//     grubx64.efi (see internal/bootd.ShimFilename/GrubFilename).
//     Required.
//   - BOOTD_TFTP_ADDR: optional override for the TFTP listen address
//     (default ":69").
//   - BOOTD_BOOT_CONFIG_URL: the boot config server's
//     (internal/bootserver) externally reachable base URL as booting
//     machines see it, for example "http://192.0.2.1:8090" - the same
//     value as that server's BOOT_SERVER_URL. Set, bootd serves the
//     rendered GRUB bootstrap config at grub/grub.cfg over TFTP (see
//     internal/bootd.GrubConfigPath), which is what hands the netboot
//     GRUB image off to that server's per-MAC config. Unset, the path
//     is not served and a netbooting machine stops at the GRUB prompt
//   - bootd logs the gap at startup rather than failing, so a
//     deployment that only exercises the DHCP/TFTP front (or predates
//     this variable) keeps its previous behavior. Must be an http URL
//     (GRUB's netboot image has no TLS stack); anything else is a
//     startup error.
//   - BOOTD_BOOT_FILENAME: optional override for the PXE boot filename
//     handed out (default internal/bootd.DefaultBootFilename,
//     "shimx64.efi").
//   - BOOTD_RUN_DIR: optional writable directory for the rendered
//     dnsmasq config, dhcp-hostsfile, and leasefile (default
//     internal/bootd.DefaultRunDir, "/run/bootd").
//   - BOOTD_DNSMASQ_PATH: optional path to the dnsmasq binary (default
//     internal/bootd.DefaultDnsmasqPath, "/usr/sbin/dnsmasq").
//   - BOOTD_ANSWER_ALL: set to "true" to disable the MAC gate (see
//     internal/bootd's package doc comment and MACCache) and answer
//     every PXE client regardless of Machine enrollment. Defaults to
//     "false" - the fail-secure, known-MACs-only mode.
//   - BOOTD_AGENT_UPSTREAM_URL: internal/agentserver's in-cluster base
//     URL, for example
//     "http://kezio-agent-server.kezio-system.svc.cluster.local:8091".
//     Set, bootd reverse-proxies every /agent/... request it receives on
//     BOOTD_PROXY_ADDR to this upstream (see bootd.ProxyServer), so a
//     live-booted kezio-agent can reach the in-cluster agent server
//     through bootd's own provisioning-network address instead of
//     needing that server separately reachable there. Unset (the
//     default): bootd proxies nothing under /agent/, unchanged from
//     before this variable existed.
//   - BOOTD_BOOT_UPSTREAM_URL: internal/bootserver's in-cluster base
//     URL, for example
//     "http://kezio-boot-server.kezio-system.svc.cluster.local:8090".
//     Set, bootd reverse-proxies every /boot/... request (GRUB's
//     per-MAC config fetch and the live kernel/initrd/squashfs
//     download) to this upstream, the same way
//     BOOTD_AGENT_UPSTREAM_URL does for /agent/.... Unset (the
//     default): bootd proxies nothing under /boot/.
//   - BOOTD_PROXY_ADDR: optional, the address the reverse proxy above
//     listens on when at least one of the two upstream URLs is set.
//     Defaults to BOOTD_SERVER_IP with port 80 - the provisioning
//     interface's own address, not every interface the pod happens to
//     have (see bootd.DefaultProxyAddr). Ignored, along with both
//     upstream URLs, when neither upstream URL is set.
//
// BOOTD_HTTP_BOOT_URL, BOOTD_DHCP_ADDR, and BOOTD_PXE_ADDR are no
// longer supported (dnsmasq's proxyDHCP engine does not answer UEFI
// HTTP Boot clients, and its DHCP ports are the well-known 67/4011
// only); setting any of them is an error rather than a silent no-op,
// so a deployment relying on the removed behavior fails loudly.
func bootdConfigFromEnv() (bootdConfig, error) {
	for _, removed := range []string{"BOOTD_HTTP_BOOT_URL", "BOOTD_DHCP_ADDR", "BOOTD_PXE_ADDR"} {
		if os.Getenv(removed) != "" {
			return bootdConfig{}, fmt.Errorf(
				"%s is no longer supported (see cmd/bootd's bootdConfigFromEnv doc comment); unset it", removed)
		}
	}

	serverIPStr := os.Getenv("BOOTD_SERVER_IP")
	if serverIPStr == "" {
		return bootdConfig{}, fmt.Errorf("BOOTD_SERVER_IP is required")
	}
	serverIP := net.ParseIP(serverIPStr)
	if serverIP == nil || serverIP.To4() == nil {
		return bootdConfig{}, fmt.Errorf("BOOTD_SERVER_IP %q is not a valid IPv4 address", serverIPStr)
	}

	cidrStr := os.Getenv("BOOTD_PROVISIONING_CIDR")
	if cidrStr == "" {
		return bootdConfig{}, fmt.Errorf("BOOTD_PROVISIONING_CIDR is required")
	}
	_, provisioningNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return bootdConfig{}, fmt.Errorf("BOOTD_PROVISIONING_CIDR %q is not a valid CIDR: %w", cidrStr, err)
	}

	tftpDir := os.Getenv("BOOTD_TFTP_DIR")
	if tftpDir == "" {
		return bootdConfig{}, fmt.Errorf("BOOTD_TFTP_DIR is required")
	}

	var nextServerIP net.IP
	if s := os.Getenv("BOOTD_NEXT_SERVER_IP"); s != "" {
		nextServerIP = net.ParseIP(s)
		if nextServerIP == nil || nextServerIP.To4() == nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_NEXT_SERVER_IP %q is not a valid IPv4 address", s)
		}
	}

	var grubConfig string
	if s := os.Getenv("BOOTD_BOOT_CONFIG_URL"); s != "" {
		var err error
		grubConfig, err = bootd.RenderGrubConfig(s)
		if err != nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_BOOT_CONFIG_URL %q: %w", s, err)
		}
	}

	var relayServerIP net.IP
	if s := os.Getenv("BOOTD_DHCP_RELAY_SERVER"); s != "" {
		relayServerIP = net.ParseIP(s)
		if relayServerIP == nil || relayServerIP.To4() == nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_DHCP_RELAY_SERVER %q is not a valid IPv4 address", s)
		}
	}

	leaseMode := os.Getenv("BOOTD_LEASE_MODE") == "true"
	if leaseMode && relayServerIP != nil {
		return bootdConfig{}, fmt.Errorf("BOOTD_LEASE_MODE and BOOTD_DHCP_RELAY_SERVER are mutually exclusive")
	}
	var leaseRangeStart, leaseRangeEnd net.IP
	startStr, endStr := os.Getenv("BOOTD_LEASE_RANGE_START"), os.Getenv("BOOTD_LEASE_RANGE_END")
	if (startStr == "") != (endStr == "") {
		return bootdConfig{}, fmt.Errorf(
			"BOOTD_LEASE_RANGE_START and BOOTD_LEASE_RANGE_END must both be set, or both left unset")
	}
	if startStr != "" {
		leaseRangeStart = net.ParseIP(startStr)
		if leaseRangeStart == nil || leaseRangeStart.To4() == nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_LEASE_RANGE_START %q is not a valid IPv4 address", startStr)
		}
		leaseRangeEnd = net.ParseIP(endStr)
		if leaseRangeEnd == nil || leaseRangeEnd.To4() == nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_LEASE_RANGE_END %q is not a valid IPv4 address", endStr)
		}
	}

	proxyCfg := bootd.ProxyConfig{
		Addr:             os.Getenv("BOOTD_PROXY_ADDR"),
		AgentUpstreamURL: os.Getenv("BOOTD_AGENT_UPSTREAM_URL"),
		BootUpstreamURL:  os.Getenv("BOOTD_BOOT_UPSTREAM_URL"),
	}
	if proxyCfg.Enabled() && proxyCfg.Addr == "" {
		proxyCfg.Addr = fmt.Sprintf("%s:80", serverIP.String())
	}

	return bootdConfig{
		Server: bootd.Config{
			Interface:       os.Getenv("BOOTD_DHCP_INTERFACE"),
			ServerIP:        serverIP,
			NextServerIP:    nextServerIP,
			ProvisioningNet: provisioningNet,
			BootFilename:    os.Getenv("BOOTD_BOOT_FILENAME"),
			RelayServerIP:   relayServerIP,
			TFTPDir:         tftpDir,
			AnswerAll:       os.Getenv("BOOTD_ANSWER_ALL") == "true",
			LeaseMode:       leaseMode,
			LeaseRangeStart: leaseRangeStart,
			LeaseRangeEnd:   leaseRangeEnd,
		},
		TFTPDir:     tftpDir,
		TFTPAddr:    os.Getenv("BOOTD_TFTP_ADDR"),
		RunDir:      os.Getenv("BOOTD_RUN_DIR"),
		DnsmasqPath: os.Getenv("BOOTD_DNSMASQ_PATH"),
		GrubConfig:  grubConfig,
		Proxy:       proxyCfg,
	}, nil
}
