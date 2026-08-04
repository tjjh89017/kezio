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

// Command bootd runs kezio-bootd: the proxyDHCP and TFTP services a
// UEFI firmware talks to at the start of a network boot (see
// internal/bootd's package doc comment for the full design). Unlike the
// controller-manager binary (cmd/main.go), bootd does not reconcile
// anything - it runs a minimal controller-runtime manager purely to get
// a live-updated cache of Machine boot MAC addresses (internal/bootd's
// MACCache), with no leader election, no webhook server, and metrics
// disabled by default, since none of those apply to a per-site,
// non-reconciling process that is meant to run one replica per boot
// segment, not compete for leadership across sites.
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

	macCache, err := bootd.NewMACCache(ctx, mgr.GetCache())
	if err != nil {
		setupLog.Error(err, "unable to set up Machine MAC cache")
		os.Exit(1)
	}
	if err := mgr.Add(macCache); err != nil {
		setupLog.Error(err, "unable to add Machine MAC cache")
		os.Exit(1)
	}

	if err := mgr.Add(&bootd.Server{Config: cfg.Server, Gate: macCache}); err != nil {
		setupLog.Error(err, "unable to add proxyDHCP/PXE server")
		os.Exit(1)
	}

	if err := mgr.Add(&bootd.TFTPServer{Dir: cfg.TFTPDir, Addr: cfg.TFTPAddr}); err != nil {
		setupLog.Error(err, "unable to add TFTP server")
		os.Exit(1)
	}

	setupLog.Info("starting bootd", "answerAll", cfg.Server.AnswerAll, "tftpDir", cfg.TFTPDir)
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
}

// bootdConfigFromEnv builds bootdConfig from the process environment.
// Every variable below is required unless noted, since bootd - unlike
// the controller-manager's optional bootserver/seeder subsystems - has
// no useful inert default: a bootd process that fails to bind its
// listeners or serve TFTP files is not doing anything at all.
//
//   - BOOTD_SERVER_IP: bootd's own IP address on the boot network, sent
//     as the DHCP Server Identifier and (unless BOOTD_NEXT_SERVER_IP is
//     set) the PXE next-server. Required.
//   - BOOTD_NEXT_SERVER_IP: overrides the next-server address handed to
//     clients, when the TFTP service is reachable at a different
//     address than BOOTD_SERVER_IP. Optional, defaults to
//     BOOTD_SERVER_IP.
//   - BOOTD_TFTP_DIR: local directory containing shimx64.efi and
//     grubx64.efi (see internal/bootd.ShimFilename/GrubFilename).
//     Required.
//   - BOOTD_DHCP_ADDR / BOOTD_PXE_ADDR / BOOTD_TFTP_ADDR: optional
//     overrides for the three listen addresses (default ":67", ":4011",
//     ":69").
//   - BOOTD_BOOT_FILENAME: optional override for the PXE boot filename
//     handed out (default internal/bootd.DefaultBootFilename,
//     "shimx64.efi").
//   - BOOTD_HTTP_BOOT_URL: optional full HTTP(S) URL handed out to a
//     client advertising UEFI HTTP Boot (option 60 "HTTPClient")
//     instead of PXE, for example
//     "http://10.0.0.5/boot/http/shimx64.efi". Unset (the default)
//     disables HTTP Boot entirely and does not affect the PXE path;
//     see internal/bootd's package doc comment for what must serve the
//     artifact at that URL before this is set.
//   - BOOTD_ANSWER_ALL: set to "true" to disable the MAC gate (see
//     internal/bootd's package doc comment and MACCache) and answer
//     every architecture-matching client regardless of Machine
//     enrollment. Defaults to "false" - the fail-secure, known-MACs-only
//     mode.
func bootdConfigFromEnv() (bootdConfig, error) {
	serverIPStr := os.Getenv("BOOTD_SERVER_IP")
	if serverIPStr == "" {
		return bootdConfig{}, fmt.Errorf("BOOTD_SERVER_IP is required")
	}
	serverIP := net.ParseIP(serverIPStr)
	if serverIP == nil {
		return bootdConfig{}, fmt.Errorf("BOOTD_SERVER_IP %q is not a valid IP address", serverIPStr)
	}

	tftpDir := os.Getenv("BOOTD_TFTP_DIR")
	if tftpDir == "" {
		return bootdConfig{}, fmt.Errorf("BOOTD_TFTP_DIR is required")
	}

	var nextServerIP net.IP
	if s := os.Getenv("BOOTD_NEXT_SERVER_IP"); s != "" {
		nextServerIP = net.ParseIP(s)
		if nextServerIP == nil {
			return bootdConfig{}, fmt.Errorf("BOOTD_NEXT_SERVER_IP %q is not a valid IP address", s)
		}
	}

	return bootdConfig{
		Server: bootd.Config{
			DHCPAddr:     os.Getenv("BOOTD_DHCP_ADDR"),
			PXEAddr:      os.Getenv("BOOTD_PXE_ADDR"),
			ServerIP:     serverIP,
			NextServerIP: nextServerIP,
			BootFilename: os.Getenv("BOOTD_BOOT_FILENAME"),
			HTTPBootURL:  os.Getenv("BOOTD_HTTP_BOOT_URL"),
			TFTPDir:      tftpDir,
			AnswerAll:    os.Getenv("BOOTD_ANSWER_ALL") == "true",
		},
		TFTPDir:  tftpDir,
		TFTPAddr: os.Getenv("BOOTD_TFTP_ADDR"),
	}, nil
}
