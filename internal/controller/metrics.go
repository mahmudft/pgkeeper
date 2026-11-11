package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	clusterReadyInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgkeeper_cluster_ready_instances",
		Help: "Number of ready PostgreSQL instances per cluster.",
	}, []string{"namespace", "cluster"})

	clusterDesiredInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgkeeper_cluster_desired_instances",
		Help: "Number of desired PostgreSQL instances per cluster.",
	}, []string{"namespace", "cluster"})

	backupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgkeeper_backups_total",
		Help: "Number of finished backups by result.",
	}, []string{"namespace", "cluster", "result"})

	backupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgkeeper_backup_last_success_timestamp_seconds",
		Help: "Unix time of the last successful backup.",
	}, []string{"namespace", "cluster"})

	backupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgkeeper_backup_duration_seconds",
		Help:    "Duration of finished backups.",
		Buckets: prometheus.ExponentialBuckets(5, 2, 10),
	}, []string{"namespace", "cluster"})
)

func init() {
	metrics.Registry.MustRegister(
		clusterReadyInstances,
		clusterDesiredInstances,
		backupsTotal,
		backupLastSuccess,
		backupDuration,
	)
}

func forgetClusterMetrics(namespace, name string) {
	clusterReadyInstances.DeleteLabelValues(namespace, name)
	clusterDesiredInstances.DeleteLabelValues(namespace, name)
	backupLastSuccess.DeleteLabelValues(namespace, name)
}
