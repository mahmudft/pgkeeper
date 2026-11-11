package controller

import (
	"fmt"
	"strings"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	labelPodName   = "statefulset.kubernetes.io/pod-name"

	// LabelCluster marks objects that belong to a PostgresCluster.
	LabelCluster = "pgkeeper.dev/cluster"
	// LabelScheduled marks PostgresBackups created by the scheduler.
	LabelScheduled = "pgkeeper.dev/scheduled"

	managedBy = "pgkeeper"

	postgresPort = 5432
	exporterPort = 9187

	// Keys in the superuser secret.
	keySuperuserPassword   = "password"
	keyReplicationPassword = "replication-password"

	// Keys in the S3 credentials secret.
	keyAccessKeyID     = "ACCESS_KEY_ID"
	keySecretAccessKey = "SECRET_ACCESS_KEY"

	dataMountPath = "/var/lib/postgresql/data"
	pgDataPath    = dataMountPath + "/pgdata"
)

func selectorLabels(c *pgv1.PostgresCluster) map[string]string {
	return map[string]string{
		labelName:     "postgres",
		labelInstance: c.Name,
	}
}

func commonLabels(c *pgv1.PostgresCluster) map[string]string {
	l := selectorLabels(c)
	l[labelManagedBy] = managedBy
	l[LabelCluster] = c.Name
	return l
}

func superuserSecretName(c *pgv1.PostgresCluster) string { return c.Name + "-superuser" }
func appSecretName(c *pgv1.PostgresCluster) string       { return c.Name + "-app" }
func scriptsConfigMapName(c *pgv1.PostgresCluster) string {
	return c.Name + "-scripts"
}
func headlessServiceName(c *pgv1.PostgresCluster) string { return c.Name + "-headless" }
func rwServiceName(c *pgv1.PostgresCluster) string       { return c.Name + "-rw" }
func roServiceName(c *pgv1.PostgresCluster) string       { return c.Name + "-ro" }
func restoreJobName(c *pgv1.PostgresCluster) string      { return c.Name + "-restore" }
func primaryPodName(c *pgv1.PostgresCluster) string      { return c.Name + "-0" }

func serviceHost(c *pgv1.PostgresCluster, svc string) string {
	return fmt.Sprintf("%s.%s.svc", svc, c.Namespace)
}

// backupPrefix returns the key prefix (with trailing slash) for a cluster's backups.
func backupPrefix(c *pgv1.PostgresCluster) string {
	p := ""
	if c.Spec.Backup != nil {
		p = strings.Trim(c.Spec.Backup.S3.Path, "/")
	}
	if p != "" {
		p += "/"
	}
	return fmt.Sprintf("%s%s/%s/", p, c.Namespace, c.Name)
}

// backupObjectKey returns the object key for a given backup.
func backupObjectKey(c *pgv1.PostgresCluster, backupName string) string {
	return backupPrefix(c) + backupName + ".sql.gz"
}
