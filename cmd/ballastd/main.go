/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	promclient "github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/metricscollector"
	"github.com/tight-line/ballast/internal/controller/resourceadjuster"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/logger"
	appmetrics "github.com/tight-line/ballast/internal/metrics"
	"github.com/tight-line/ballast/internal/plugin"
	k8splugin "github.com/tight-line/ballast/internal/plugin/kubernetes"
	"github.com/tight-line/ballast/internal/store"
	ballastwebhook "github.com/tight-line/ballast/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(ballastv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

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

	var otelEndpoint, otelProtocol string
	var otelInterval time.Duration
	var otelInsecure bool
	flag.StringVar(&otelEndpoint, "otel-metrics-endpoint", "",
		"OTLP collector endpoint (e.g. localhost:4317). Leave empty to disable OTel push export.")
	flag.StringVar(&otelProtocol, "otel-metrics-protocol", "grpc",
		"OTLP transport protocol: grpc or http/protobuf.")
	flag.DurationVar(&otelInterval, "otel-metrics-interval", 30*time.Second,
		"Push interval for the OTLP metrics exporter.")
	flag.BoolVar(&otelInsecure, "otel-metrics-insecure", false,
		"Disable TLS for the OTLP metrics connection.")

	var dryRunMeasure bool
	flag.BoolVar(&dryRunMeasure, "dry-run-measure", false,
		"If set, metrics are computed but not written to Redis and WorkloadProfile status is not updated.")

	var dryRunApply bool
	flag.BoolVar(&dryRunApply, "dry-run-apply", false,
		"If set, resource recommendations are computed at admission time but the pod spec is not patched.")

	var dryRunResize bool
	flag.BoolVar(&dryRunResize, "dry-run-resize", false,
		"If set, resize decisions are logged but no in-place resize patches are issued.")

	var logLevel, logLevelWebhook, logLevelWatcher, logLevelCollector, logLevelAdjuster, logFormat string
	flag.StringVar(&logLevel, "log-level", "info", "Global log level (debug|info|warn|error).")
	flag.StringVar(&logLevelWebhook, "log-level-webhook", "", "Log level for the webhook component (overrides --log-level).")
	flag.StringVar(&logLevelWatcher, "log-level-watcher", "", "Log level for the workload watcher component.")
	flag.StringVar(&logLevelCollector, "log-level-collector", "", "Log level for the metrics collector component.")
	flag.StringVar(&logLevelAdjuster, "log-level-adjuster", "", "Log level for the resource adjuster component.")
	flag.StringVar(&logFormat, "log-format", "json", "Log output format (json|text).")

	var operatorNamespace string
	flag.StringVar(&operatorNamespace, "operator-namespace", getEnvOrDefault("POD_NAMESPACE", "ballast-system"),
		"Namespace where the operator is running (used for kill-switch ConfigMap watch).")

	var redisURL string
	flag.StringVar(&redisURL, "redis-url", getEnvOrDefault("REDIS_URL", "redis://localhost:6379"),
		"Redis/Valkey URL for metric storage (redis://[user:pass@]host:port/db).")

	flag.Parse()

	ctrl.SetLogger(logger.New("setup", logLevel, logFormat))

	// Disable HTTP/2 by default to avoid stream cancellation and rapid reset CVEs.
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServerOptions := webhook.Options{
		TLSOpts: tlsOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "910c37cf.tightlinesoftware.com",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	metricsClient, err := metricsclientset.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create metrics clientset")
		os.Exit(1)
	}
	plugin.Register(k8splugin.New(metricsClient.MetricsV1beta1().PodMetricses(""), k8splugin.DefaultOptions()))

	storeClient, err := store.NewClient(redisURL)
	if err != nil {
		setupLog.Error(err, "Failed to create Redis client")
		os.Exit(1)
	}

	var promRegisterer promclient.Registerer
	if metricsAddr != "0" {
		promRegisterer = promclient.DefaultRegisterer
	}
	metricProvider, shutdownMetrics, err := appmetrics.SetupProvider(context.Background(), appmetrics.Config{
		PrometheusRegisterer: promRegisterer,
		OTLPEndpoint:         otelEndpoint,
		OTLPProtocol:         otelProtocol,
		OTLPInterval:         otelInterval,
		OTLPInsecure:         otelInsecure,
	})
	if err != nil {
		setupLog.Error(err, "Failed to set up metrics provider")
		os.Exit(1)
	}
	defer func() { _ = shutdownMetrics(context.Background()) }()

	rec, err := appmetrics.NewRecorder(metricProvider)
	if err != nil {
		setupLog.Error(err, "Failed to create metrics recorder")
		_ = shutdownMetrics(context.Background())
		os.Exit(1)
	}

	ks := killswitch.New(mgr.GetClient(), operatorNamespace, rec)
	if err := ks.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up kill switch")
		os.Exit(1)
	}

	if err := workloadwatcher.New(mgr.GetClient(), ks, storeClient, rec).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up workloadwatcher controller")
		os.Exit(1)
	}

	if err := metricscollector.Setup(mgr, ks, storeClient, dryRunMeasure, rec); err != nil {
		setupLog.Error(err, "Failed to set up metricscollector controller")
		os.Exit(1)
	}

	ballastwebhook.NewPodMutator(mgr.GetClient(), ks, dryRunApply, rec).SetupWithManager(mgr)

	if err := resourceadjuster.Setup(mgr, ks, dryRunResize, rec); err != nil {
		setupLog.Error(err, "Failed to set up resourceadjuster controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
