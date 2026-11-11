package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
	"github.com/mahmudft/pgkeeper/internal/controller"
	webhookv1alpha1 "github.com/mahmudft/pgkeeper/internal/webhook/v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(pgv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		leaderElection bool
		webhookPort    int
		certDir        string
	)
	images := controller.DefaultImages()

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port of the admission webhook server.")
	flag.StringVar(&certDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory with tls.crt and tls.key for the webhook server.")
	flag.StringVar(&images.Postgres, "postgres-image", images.Postgres, "Postgres image repository; the cluster version is used as tag.")
	flag.StringVar(&images.Exporter, "exporter-image", images.Exporter, "postgres_exporter image.")
	flag.StringVar(&images.Tools, "tools-image", images.Tools, "Image with the aws CLI used by backup and restore jobs.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS") != "false"

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "pgkeeper.pgkeeper.dev",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: webhookPort, CertDir: certDir}),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.PostgresClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("pgkeeper"),
		Images:   images,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PostgresCluster")
		os.Exit(1)
	}
	if err := (&controller.PostgresBackupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("pgkeeper"),
		Images:   images,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PostgresBackup")
		os.Exit(1)
	}
	if enableWebhooks {
		if err := webhookv1alpha1.SetupPostgresClusterWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PostgresCluster")
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

	setupLog.Info("starting manager", "webhooks", enableWebhooks)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
