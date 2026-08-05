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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentserver"
	"github.com/tjjh89017/kezio/internal/bootserver"

	// Blank-imported for their init() side effect: each registers its
	// URL scheme with internal/bmc's driver registry (see
	// internal/bmc/registry.go's Register), which deployerFactoryFromEnv's
	// AgentFactory path resolves Machine.spec.bmc.address against.
	_ "github.com/tjjh89017/kezio/internal/bmc/ipmi"
	_ "github.com/tjjh89017/kezio/internal/bmc/ipmitool"
	_ "github.com/tjjh89017/kezio/internal/bmc/redfish"
	"github.com/tjjh89017/kezio/internal/controller"
	"github.com/tjjh89017/kezio/internal/deployer"
	webhookv1alpha1 "github.com/tjjh89017/kezio/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(keziov1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4f000b8a.kojuro.date",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ingestConfig, err := ingestConfigFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid ingest configuration")
		os.Exit(1)
	}
	if err := (&controller.ImageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Ingest: ingestConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Image")
		os.Exit(1)
	}
	if err := (&controller.PostHookReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PostHook")
		os.Exit(1)
	}
	if err := (&controller.MachineReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		DeployerFactory: deployerFactoryFromEnv(mgr.GetClient()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Machine")
		os.Exit(1)
	}
	seederCfg, err := seederConfigFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid seeder configuration")
		os.Exit(1)
	}
	if err := (&controller.SeederReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Seeder: seederCfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Seeder")
		os.Exit(1)
	}
	if webhooksEnabled() {
		if err := webhookv1alpha1.SetupImageWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Image")
			os.Exit(1)
		}
	}
	if webhooksEnabled() {
		if err := webhookv1alpha1.SetupPostHookWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PostHook")
			os.Exit(1)
		}
	}
	if webhooksEnabled() {
		if err := webhookv1alpha1.SetupMachineWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Machine")
			os.Exit(1)
		}
	}

	bootConfig, err := bootServerConfigFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid boot server configuration")
		os.Exit(1)
	}
	if bootConfig != nil {
		if err := bootserver.SetupFieldIndexer(context.Background(), mgr); err != nil {
			setupLog.Error(err, "unable to set up boot MAC field indexer")
			os.Exit(1)
		}
		if err := mgr.Add(bootserver.New(mgr.GetClient(), *bootConfig)); err != nil {
			setupLog.Error(err, "unable to add boot config server")
			os.Exit(1)
		}
	}

	agentConfig, err := agentServerConfigFromEnv(seederCfg)
	if err != nil {
		setupLog.Error(err, "invalid agent server configuration")
		os.Exit(1)
	}
	if agentConfig != nil {
		if err := agentserver.SetupFieldIndexer(context.Background(), mgr); err != nil {
			setupLog.Error(err, "unable to set up agent token field indexer")
			os.Exit(1)
		}
		if err := mgr.Add(agentserver.New(mgr.GetClient(), *agentConfig)); err != nil {
			setupLog.Error(err, "unable to add agent registration server")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// webhooksEnabled reports whether webhooks should be registered with the
// manager. Set ENABLE_WEBHOOKS=false to run without them - for example
// locally, without the TLS serving certificate a real cluster provides via
// cert-manager.
func webhooksEnabled() bool {
	return os.Getenv("ENABLE_WEBHOOKS") != "false"
}

// ingestConfigFromEnv builds the Image reconciler's IngestConfig from
// the environment. Leaving INGEST_IMAGE unset (the default for every
// existing deployment and the e2e suite) yields the zero IngestConfig,
// which keeps the Image reconciler on its stub fast path - no ingest Job
// is ever created, and no store or staging volume is required. Setting
// INGEST_IMAGE opts into the real ingest pipeline; INGEST_STORE_PVC must
// then also be set, naming the PersistentVolumeClaim the ingest Job
// mounts as its store volume. INGEST_STAGING_PVC is optional (only
// needed to ingest kezio-staged:// sources); INGEST_SERVICE_ACCOUNT is
// optional.
func ingestConfigFromEnv() (controller.IngestConfig, error) {
	image := os.Getenv("INGEST_IMAGE")
	if image == "" {
		return controller.IngestConfig{}, nil
	}

	storePVC := os.Getenv("INGEST_STORE_PVC")
	if storePVC == "" {
		return controller.IngestConfig{}, fmt.Errorf("INGEST_IMAGE is set but INGEST_STORE_PVC is not")
	}

	cfg := controller.IngestConfig{
		Image: image,
		StoreVolume: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: storePVC},
		},
		ServiceAccountName: os.Getenv("INGEST_SERVICE_ACCOUNT"),
	}
	if stagingPVC := os.Getenv("INGEST_STAGING_PVC"); stagingPVC != "" {
		cfg.StagingVolume = &corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: stagingPVC},
		}
	}
	return cfg, nil
}

// seederConfigFromEnv builds the seeder reconciler's SeederConfig from
// the environment. Leaving SEEDER_TRACKER_URL unset (the default for
// every existing deployment and the e2e suite) yields the zero
// SeederConfig, which SeederReconciler.SetupWithManager recognizes as
// disabled and does not register a controller for at all - no seeder
// Service or store volume is ever required. Setting SEEDER_TRACKER_URL
// opts in; SEEDER_STORE_ROOT, SEEDER_SERVICE_NAMESPACE, and
// SEEDER_SERVICE_NAME must then also be set:
//
//   - SEEDER_TRACKER_URL is inserted as every built .torrent's announce
//     URL (see internal/store.BuildTorrentFile).
//   - SEEDER_STORE_ROOT is the store's local filesystem root. Unlike the
//     ingest Job (which gets its store volume from IngestConfig at Job
//     creation time), the manager container itself needs the store
//     mounted read-only for this: it reads contents/<hash>/torrent.info
//     directly to build the .torrent bytes it hands to ezio. Mounting
//     that volume onto the controller-manager Deployment is left to the
//     cluster operator (see config/seeder's README), the same
//     deploy-time customization gap IngestConfig's own volumes have
//     today.
//   - SEEDER_SERVICE_NAMESPACE and SEEDER_SERVICE_NAME locate the seeder
//     Service (see config/seeder/service.yaml); the reconciler resolves
//     it to individual Ready backends via EndpointSlices.
//   - SEEDER_GRPC_PORT_NAME optionally overrides the EndpointPort name
//     carrying the seeder's gRPC port (default "grpc").
//   - SEEDER_MAX_UPLOADS / SEEDER_MAX_CONNECTIONS optionally override
//     the cluster-wide default per-torrent AddTorrent tuning this
//     reconciler applies to every content torrent it adds (see
//     config/seeder/README.md's "WAN swarm tuning" section). Unset
//     leaves seeder.DefaultMaxUploads/DefaultMaxConnections in effect.
func seederConfigFromEnv() (controller.SeederConfig, error) {
	trackerURL := os.Getenv("SEEDER_TRACKER_URL")
	if trackerURL == "" {
		return controller.SeederConfig{}, nil
	}
	ezioTuning, err := ezioTuningFromEnv("SEEDER_MAX_UPLOADS", "SEEDER_MAX_CONNECTIONS")
	if err != nil {
		return controller.SeederConfig{}, err
	}
	return controller.SeederConfig{
		TrackerURL:       trackerURL,
		StoreRoot:        os.Getenv("SEEDER_STORE_ROOT"),
		ServiceNamespace: os.Getenv("SEEDER_SERVICE_NAMESPACE"),
		ServiceName:      os.Getenv("SEEDER_SERVICE_NAME"),
		GRPCPortName:     os.Getenv("SEEDER_GRPC_PORT_NAME"),
		EzioTuning:       ezioTuning,
	}, nil
}

// ezioTuningFromEnv reads a cluster-wide default MaxUploads/MaxConnections
// override from the two named environment variables, returning nil when
// neither is set (meaning "no cluster-wide override; fall back further",
// exactly like an absent Machine.spec.ezio field - see
// keziov1alpha1.MergeEzioTuning and internal/seeder's
// ResolveMaxUploads/ResolveMaxConnections). An unparsable non-empty
// value is a startup-time configuration error, not silently ignored.
func ezioTuningFromEnv(maxUploadsVar, maxConnectionsVar string) (*keziov1alpha1.MachineEzioTuning, error) {
	var tuning keziov1alpha1.MachineEzioTuning
	var set bool
	if v := os.Getenv(maxUploadsVar); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", maxUploadsVar, err)
		}
		n32 := int32(n)
		tuning.MaxUploads = &n32
		set = true
	}
	if v := os.Getenv(maxConnectionsVar); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", maxConnectionsVar, err)
		}
		n32 := int32(n)
		tuning.MaxConnections = &n32
		set = true
	}
	if !set {
		return nil, nil
	}
	return &tuning, nil
}

// bootServerConfigFromEnv builds the boot config server's
// bootserver.Config from the environment. Leaving BOOT_SERVER_ADDR unset
// (the default for every existing deployment and the e2e suite) returns
// a nil Config, and main does not add the server to the manager at all -
// the same inert-by-default gating shape INGEST_IMAGE and
// SEEDER_TRACKER_URL use. Setting BOOT_SERVER_ADDR opts in;
// BOOT_ARTIFACTS_DIR and BOOT_SERVER_URL must then also be set:
//
//   - BOOT_SERVER_ADDR is the address the boot HTTP server listens on,
//     for example ":8090".
//   - BOOT_ARTIFACTS_DIR is the local filesystem directory GET
//     /boot/artifacts/... serves from - a volume mounted into the
//     manager container out of band (see config/bootserver's README).
//   - BOOT_SERVER_URL is this server's own externally reachable base
//     URL, for example "http://10.0.0.5:8090": GRUB and the live
//     environment's initrd both need to reach it from the target
//     machine's network to fetch the kernel/initrd/squashfs, which is
//     never something this process can discover on its own.
//   - BOOT_AGENT_SERVER_URL is internal/agentserver's own externally
//     reachable base URL, for example "http://10.0.0.5:8091" - see
//     bootserver.Config.AgentServerURL's doc comment for why this is
//     ordinarily a different address from BOOT_SERVER_URL (a different
//     Service, on a different container port, fronting the same
//     controller-manager Pod), and why leaving it unset to silently fall
//     back to BOOT_SERVER_URL is only correct when both servers truly
//     share one address. A deployment that sets AGENT_SERVER_ADDR (see
//     agentServerConfigFromEnv) should set this too.
//   - BOOT_KERNEL_PATH / BOOT_INITRD_PATH optionally override the
//     artifact file names under BOOT_ARTIFACTS_DIR (default "vmlinuz" /
//     "initrd.img").
//   - BOOT_TOKEN_TTL optionally overrides how long a minted boot token
//     is accepted (a Go duration string, for example "30m"; default
//     bootserver.DefaultTokenTTL).
//   - BOOT_EFI_DIR optionally overrides the directory GET
//     /boot/http/<name> serves the signed shim/grub EFI binaries from,
//     for UEFI HTTP Boot (see internal/bootd's Config.HTTPBootURL).
//     Empty means BOOT_ARTIFACTS_DIR: the same volume already used for
//     the kernel/initrd/squashfs is the natural place to also stage
//     shimx64.efi/grubx64.efi, so this only needs setting if those live
//     somewhere else.
func bootServerConfigFromEnv() (*bootserver.Config, error) {
	addr := os.Getenv("BOOT_SERVER_ADDR")
	if addr == "" {
		return nil, nil
	}

	artifactsDir := os.Getenv("BOOT_ARTIFACTS_DIR")
	if artifactsDir == "" {
		return nil, fmt.Errorf("BOOT_SERVER_ADDR is set but BOOT_ARTIFACTS_DIR is not")
	}
	serverURL := os.Getenv("BOOT_SERVER_URL")
	if serverURL == "" {
		return nil, fmt.Errorf("BOOT_SERVER_ADDR is set but BOOT_SERVER_URL is not")
	}

	cfg := &bootserver.Config{
		Addr:           addr,
		ArtifactsDir:   artifactsDir,
		ServerURL:      serverURL,
		AgentServerURL: os.Getenv("BOOT_AGENT_SERVER_URL"),
		KernelPath:     os.Getenv("BOOT_KERNEL_PATH"),
		InitrdPath:     os.Getenv("BOOT_INITRD_PATH"),
		EFIDir:         os.Getenv("BOOT_EFI_DIR"),
	}
	if ttl := os.Getenv("BOOT_TOKEN_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return nil, fmt.Errorf("invalid BOOT_TOKEN_TTL: %w", err)
		}
		cfg.TokenTTL = d
	}
	return cfg, nil
}

// agentServerConfigFromEnv builds the agent registration server's
// agentserver.Config from the environment, mirroring
// bootServerConfigFromEnv's inert-by-default shape: leaving
// AGENT_SERVER_ADDR unset returns a nil Config, and main does not add
// the server to the manager at all. Setting AGENT_SERVER_ADDR (for
// example ":8091") opts in.
//
// Building a DeployPlan needs the same two things
// seederConfigFromEnv's SeederConfig already carries - a tracker URL and
// a read-only store mount, for the identical reason SeederConfig's own
// doc comment gives (reading contents/<hash>/torrent.info to build
// .torrent bytes) - so this reuses seeder verbatim rather than
// introducing a second, parallel set of AGENT_TRACKER_URL /
// AGENT_STORE_ROOT variables naming the same tracker and the same
// volume. A deployment that sets AGENT_SERVER_ADDR without also setting
// SEEDER_TRACKER_URL / SEEDER_STORE_ROOT (see config/seeder's README)
// still starts - GET .../next then never has enough to build a plan for
// an Image with a content partition, and answers ActionWait forever
// instead of failing outright, the same graceful degradation
// buildDeployPlan gives an unexpected build error.
//
// EZIO_DEFAULT_MAX_UPLOADS / EZIO_DEFAULT_MAX_CONNECTIONS optionally set
// the cluster-wide default per-torrent AddTorrent tuning applied to
// every DeployPlan this server builds, before any Machine.spec.ezio
// override (see keziov1alpha1.MergeEzioTuning). These are intentionally
// separate from SEEDER_MAX_UPLOADS/SEEDER_MAX_CONNECTIONS: the seeder
// pods and each machine's leecher can reasonably want different
// defaults (a seeder serves every site at once; a leecher serves only
// itself), so this reuses SeederConfig for the tracker/store settings
// alone, not its tuning.
func agentServerConfigFromEnv(seeder controller.SeederConfig) (*agentserver.Config, error) {
	addr := os.Getenv("AGENT_SERVER_ADDR")
	if addr == "" {
		return nil, nil
	}
	ezioDefaults, err := ezioTuningFromEnv("EZIO_DEFAULT_MAX_UPLOADS", "EZIO_DEFAULT_MAX_CONNECTIONS")
	if err != nil {
		return nil, err
	}
	return &agentserver.Config{
		Addr:         addr,
		StoreRoot:    seeder.StoreRoot,
		TrackerURL:   seeder.TrackerURL,
		EzioDefaults: ezioDefaults,
	}, nil
}

// deployerFactoryFromEnv selects the Deployer implementation the Machine
// reconciler drives, controlled by DEPLOYER: "agent" wires
// deployer.AgentFactory (registration- and inspection-driven, backed by
// the real kezio-agent flow); anything else, including unset, wires
// deployer.FakeFactory, which fabricates plausible results for every
// phase so the state machine runs end to end with no live hardware or
// agent - the existing default, kept as the default so every existing
// deployment and the fast-lane e2e suite are unaffected by this
// function's addition.
func deployerFactoryFromEnv(c client.Client) deployer.Factory {
	if os.Getenv("DEPLOYER") == "agent" {
		return deployer.NewAgentFactory(c).New
	}
	return deployer.NewFactory().New
}
