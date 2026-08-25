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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/agentserver"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/controller"
	"github.com/tjjh89017/kezio/internal/deployer"
	"github.com/tjjh89017/kezio/internal/planbuild"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
	webhookv1alpha3 "github.com/tjjh89017/kezio/internal/webhook/v1alpha3"

	// Blank-imported so their init() registers "redfish"/"ipmi" (and
	// related schemes) with internal/bmc's registry: the Machine webhook
	// validates spec.bmc.address against it, and nothing else in this
	// binary references these packages directly.
	_ "github.com/tjjh89017/kezio/internal/bmc/ipmi"
	_ "github.com/tjjh89017/kezio/internal/bmc/redfish"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(keziov1alpha3.AddToScheme(scheme))
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

	machineDeployer, err := deployerFromEnv(mgr.GetClient())
	if err != nil {
		setupLog.Error(err, "invalid DEPLOYER configuration")
		os.Exit(1)
	}
	if err := (&controller.MachineReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("machine-controller"),
		Deployer: machineDeployer,
		Builder:  planBuilder(mgr.GetClient()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Machine")
		os.Exit(1)
	}
	if err := (&controller.DeployRunReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DeployRun")
		os.Exit(1)
	}
	if err := (&controller.MachineClaimReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("machineclaim-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MachineClaim")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupMachineWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Machine")
			os.Exit(1)
		}
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupMachineClaimWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "MachineClaim")
			os.Exit(1)
		}
	}
	// Shared with PartitionContentReconciler below: both need the same
	// answer to "is a seeder image configured at all" - the Image
	// reconciler to act on it, PartitionContent to explain an unavailable
	// seeder that is not even trying to appear.
	seederConfig := imageSeederConfigFromEnv()
	if err := (&controller.ImageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Seeder: seederConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Image")
		os.Exit(1)
	}
	if err := (&controller.ImageImportReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Ingest: imageIngestConfigFromEnv(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageImport")
		os.Exit(1)
	}
	if err := (&controller.PartitionContentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("partitioncontent-controller"),
		Publish:  partitionContentPublishConfigFromEnv(),
		Seeder:   seederConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PartitionContent")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupImageWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Image")
			os.Exit(1)
		}
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupPartitionContentWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PartitionContent")
			os.Exit(1)
		}
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupDeployRunWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "DeployRun")
			os.Exit(1)
		}
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupMachineHardwareWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "MachineHardware")
			os.Exit(1)
		}
	}
	if err := (&controller.SubnetReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		BootdDeployment: bootdDeploymentConfigFromEnv(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Subnet")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupSubnetWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Subnet")
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
	agentConfig, err := agentServerConfigFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid agent server configuration")
		os.Exit(1)
	}
	if agentConfig != nil {
		if err := agentserver.SetupFieldIndexer(context.Background(), mgr); err != nil {
			setupLog.Error(err, "unable to set up agent token field indexers")
			os.Exit(1)
		}
		agentSrv := agentserver.New(mgr.GetClient(), *agentConfig)
		agentSrv.PlanBuilder = planBuilder(mgr.GetClient())
		agentSrv.Abort = agentserver.NewAbortDecider(mgr.GetClient())
		if err := mgr.Add(agentSrv); err != nil {
			setupLog.Error(err, "unable to add agent registration server")
			os.Exit(1)
		}
	}
	if err := (&controller.PostHookReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PostHook")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupPostHookWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PostHook")
			os.Exit(1)
		}
	}
	if err := (&controller.SiteReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		TrackerDeployment: trackerDeploymentConfigFromEnv(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Site")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha3.SetupSiteWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Site")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.Add(&posthookdefaults.Ensurer{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to add default PostHook ensurer")
		os.Exit(1)
	}

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

// deployerFromEnv selects the Machine reconciler's Deployer implementation.
// DEPLOYER unset or "fake" keeps deployer.FakeDeployer, which never dials
// real hardware and backs envtest and the fast e2e lane - kezio's default.
// DEPLOYER=agent builds deployer.AgentDeployer, which drives real BMCs and
// waits for kezio-agent registration. Any other value fails startup rather
// than silently falling back to the fake.
func deployerFromEnv(c client.Client) (deployer.Deployer, error) {
	switch v := os.Getenv("DEPLOYER"); v {
	case "", "fake":
		return &deployer.FakeDeployer{Client: c}, nil
	case "agent":
		return &deployer.AgentDeployer{Client: c, PlanBuilder: planBuilder(c)}, nil
	default:
		return nil, fmt.Errorf("unknown DEPLOYER %q (want \"fake\" or \"agent\")", v)
	}
}

// planBuilder builds the planbuild.Builder the AgentDeployer and the
// agent registration server each need their own instance of: resolving a
// Machine's deploy intent into a DeployPlan needs no state beyond c and
// the manager namespace, so both call sites build one identically.
// ManagerNamespace falls back to empty when POD_NAMESPACE is unset (a
// local `make run`), matching posthookdefaults.Ensurer's own fallback -
// a Machine relying on the default PostHook simply cannot resolve a plan
// in that case, the same "not ready" outcome an absent PostHook already
// produces.
func planBuilder(c client.Client) *planbuild.Builder {
	ns, _ := posthookdefaults.Namespace()
	return &planbuild.Builder{Client: c, ManagerNamespace: ns, LeecherEzio: leecherEzioConfigFromEnv()}
}

// imageIngestConfigFromEnv builds the Image reconciler's
// ImageIngestConfig from the environment. Leaving IMAGE_INGEST_IMAGE
// unset yields a config that is not ready(): the reconciler holds every
// source-bearing Image at Pending with a condition explaining why.
func imageIngestConfigFromEnv() controller.ImageIngestConfig {
	cfg := controller.ImageIngestConfig{
		Image:                   os.Getenv("IMAGE_INGEST_IMAGE"),
		SourceFormat:            os.Getenv("IMAGE_INGEST_SOURCE_FORMAT"),
		ServiceAccountName:      os.Getenv("IMAGE_INGEST_SERVICE_ACCOUNT"),
		ScratchStorageClassName: os.Getenv("IMAGE_INGEST_SCRATCH_STORAGE_CLASS"),
		StagingPVCName:          os.Getenv("IMAGE_INGEST_STAGING_PVC"),
	}
	if v := os.Getenv("IMAGE_INGEST_SCRATCH_SIZE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			setupLog.Error(err, "invalid IMAGE_INGEST_SCRATCH_SIZE_BYTES, using default", "value", v)
		} else {
			cfg.ScratchSizeBytes = n
		}
	}
	if modes := os.Getenv("IMAGE_INGEST_SCRATCH_ACCESS_MODES"); modes != "" {
		for _, m := range strings.Split(modes, ",") {
			cfg.ScratchAccessModes = append(cfg.ScratchAccessModes, corev1.PersistentVolumeAccessMode(strings.TrimSpace(m)))
		}
	}
	return cfg
}

// partitionContentPublishConfigFromEnv builds the PartitionContent
// reconciler's PartitionContentPublishConfig from the environment.
// Leaving PARTITIONCONTENT_PUBLISH_IMAGE unset yields a config that is
// not ready() (see its doc comment): the reconciler holds every
// PartitionContent at Pending until it is set. There is no cluster-wide
// tracker setting to read here - a PartitionContent needs none to reach
// Ready (see PartitionContentPublishConfig's doc comment).
func partitionContentPublishConfigFromEnv() controller.PartitionContentPublishConfig {
	cfg := controller.PartitionContentPublishConfig{
		Image:              os.Getenv("PARTITIONCONTENT_PUBLISH_IMAGE"),
		ServiceAccountName: os.Getenv("PARTITIONCONTENT_PUBLISH_SERVICE_ACCOUNT"),
		StorageClassName:   os.Getenv("PARTITIONCONTENT_STORAGE_CLASS"),
	}
	if modes := os.Getenv("PARTITIONCONTENT_ACCESS_MODES"); modes != "" {
		for _, m := range strings.Split(modes, ",") {
			cfg.AccessModes = append(cfg.AccessModes, corev1.PersistentVolumeAccessMode(strings.TrimSpace(m)))
		}
	}
	return cfg
}

// imageSeederConfigFromEnv builds the Image reconciler's ImageSeederConfig
// from the environment. Leaving PARTITIONCONTENT_SEEDER_IMAGE unset yields
// a config that is not ready(): seed-demand at any Site is simply not
// acted on until it is set, and PartitionContentReconciler's own status
// derivation is what surfaces that on the affected content.
// PARTITIONCONTENT_SEEDER_MAX_UPLOADS and
// PARTITIONCONTENT_SEEDER_MAX_CONNECTIONS are the cluster-wide default
// every seeder pod registers its content with; left unset each falls back
// to its own seeder.Default* constant. These are deliberately separate
// settings from LEECHER_MAX_UPLOADS/LEECHER_MAX_CONNECTIONS below (see
// ImageSeederConfig's doc comment for why).
func imageSeederConfigFromEnv() controller.ImageSeederConfig {
	cfg := controller.ImageSeederConfig{
		Image: os.Getenv("PARTITIONCONTENT_SEEDER_IMAGE"),
	}
	if v := os.Getenv("PARTITIONCONTENT_SEEDER_GRACE_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			setupLog.Error(err, "invalid PARTITIONCONTENT_SEEDER_GRACE_PERIOD, using default", "value", v)
		} else {
			cfg.GracePeriod = d
		}
	}
	if v := os.Getenv("PARTITIONCONTENT_SEEDER_MAX_UPLOADS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			setupLog.Error(err, "invalid PARTITIONCONTENT_SEEDER_MAX_UPLOADS, using default", "value", v)
		} else {
			cfg.MaxUploads = int32(n)
		}
	}
	if v := os.Getenv("PARTITIONCONTENT_SEEDER_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			setupLog.Error(err, "invalid PARTITIONCONTENT_SEEDER_MAX_CONNECTIONS, using default", "value", v)
		} else {
			cfg.MaxConnections = int32(n)
		}
	}
	return cfg
}

// leecherEzioConfigFromEnv builds the plan builder's cluster-wide leecher
// ezio tuning default from LEECHER_MAX_UPLOADS/LEECHER_MAX_CONNECTIONS,
// mirroring imageSeederConfigFromEnv's parsing. This is the layer-2
// default a DeployRun's leecher plan carries before any per-Machine
// override (Machine.spec.ezio) is applied - kept separate from the
// seeder's own cluster-wide default because a seeder serves every leecher
// at its Site at once, while a leecher serves only itself.
func leecherEzioConfigFromEnv() planbuild.LeecherEzioConfig {
	var cfg planbuild.LeecherEzioConfig
	if v := os.Getenv("LEECHER_MAX_UPLOADS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			setupLog.Error(err, "invalid LEECHER_MAX_UPLOADS, using default", "value", v)
		} else {
			cfg.MaxUploads = int32(n)
		}
	}
	if v := os.Getenv("LEECHER_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			setupLog.Error(err, "invalid LEECHER_MAX_CONNECTIONS, using default", "value", v)
		} else {
			cfg.MaxConnections = int32(n)
		}
	}
	return cfg
}

// bootServerConfigFromEnv builds the boot config server's
// bootserver.Config from the environment. Leaving BOOT_SERVER_ADDR unset
// returns a nil Config, and main does not add the server to the manager.
// Setting it opts in; BOOT_ARTIFACTS_DIR and BOOT_SERVER_URL must then
// also be set. BOOT_AGENT_SERVER_URL is ordinarily a different address
// from BOOT_SERVER_URL (a different Service/port fronting the same Pod);
// left unset it falls back to BOOT_SERVER_URL, correct only when both
// servers truly share one address. BOOT_KERNEL_PATH, BOOT_INITRD_PATH,
// and BOOT_SQUASHFS_PATH each default to their bootserver.Default* name.
// BOOT_EFI_DIR defaults to BOOT_ARTIFACTS_DIR.
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
		SquashfsPath:   os.Getenv("BOOT_SQUASHFS_PATH"),
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
// the server to the manager. AGENT_SESSION_TTL and AGENT_POLL_INTERVAL
// are both optional; left unset the server falls back to
// agentserver.DefaultSessionTTL and agentserver.DefaultPollInterval.
func agentServerConfigFromEnv() (*agentserver.Config, error) {
	addr := os.Getenv("AGENT_SERVER_ADDR")
	if addr == "" {
		return nil, nil
	}

	cfg := &agentserver.Config{Addr: addr}
	if ttl := os.Getenv("AGENT_SESSION_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_SESSION_TTL: %w", err)
		}
		cfg.SessionTTL = d
	}
	if interval := os.Getenv("AGENT_POLL_INTERVAL"); interval != "" {
		d, err := time.ParseDuration(interval)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_POLL_INTERVAL: %w", err)
		}
		cfg.PollInterval = d
	}
	return cfg, nil
}

// bootdDeploymentConfigFromEnv builds the Subnet reconciler's
// BootdDeploymentConfig from the environment. Leaving BOOTD_DEPLOYMENT_IMAGE
// or BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE unset yields a config that is
// not enabled() (see its doc comment): the reconciler still computes and
// writes Valid/Ready for every Subnet, but Ready stays False with a
// reason naming that no image is configured, and no Deployment is
// created.
func bootdDeploymentConfigFromEnv() controller.BootdDeploymentConfig {
	return controller.BootdDeploymentConfig{
		Image:              os.Getenv("BOOTD_DEPLOYMENT_IMAGE"),
		BootArtifactsImage: os.Getenv("BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE"),
		ServiceAccountName: os.Getenv("BOOTD_DEPLOYMENT_SERVICE_ACCOUNT"),
		AgentUpstreamURL:   os.Getenv("BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL"),
		BootUpstreamURL:    os.Getenv("BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL"),
		HTTPBootURL:        os.Getenv("BOOTD_DEPLOYMENT_HTTP_BOOT_URL"),
	}
}

// trackerDeploymentConfigFromEnv builds the Site reconciler's
// TrackerDeploymentConfig from the environment, mirroring
// bootdDeploymentConfigFromEnv's shape. Leaving TRACKER_DEPLOYMENT_IMAGE
// unset yields a config that is not enabled(): the reconciler still
// computes and writes Valid/Ready for every Site, but a seeding Site's
// Ready stays False with a reason naming that no image is configured, and
// no tracker Deployment is created.
func trackerDeploymentConfigFromEnv() controller.TrackerDeploymentConfig {
	return controller.TrackerDeploymentConfig{
		Image: os.Getenv("TRACKER_DEPLOYMENT_IMAGE"),
	}
}
