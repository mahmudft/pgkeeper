package controller

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

// Images holds the container images used by the operator.
type Images struct {
	// Postgres is the image repository; the cluster version is used as tag.
	Postgres string
	// Exporter is the full postgres_exporter image reference.
	Exporter string
	// Tools is an image that ships the aws CLI, used for backup and restore jobs.
	Tools string
}

// DefaultImages returns the images used when no flags are given.
func DefaultImages() Images {
	return Images{
		Postgres: "postgres",
		Exporter: "quay.io/prometheuscommunity/postgres-exporter:v0.17.1",
		Tools:    "amazon/aws-cli:2.27.0",
	}
}

const postgresUID int64 = 999

func postgresImage(c *pgv1.PostgresCluster, images Images) string {
	if c.Spec.Image != "" {
		return c.Spec.Image
	}
	return fmt.Sprintf("%s:%s", images.Postgres, c.PostgresVersion())
}

// initdbScript runs once on the primary during initdb.
const initdbScript = `#!/bin/bash
set -euo pipefail
echo "pgkeeper: creating replication role, application role and database"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<EOSQL
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '${REPLICATION_PASSWORD}';
CREATE ROLE "${APP_USER}" WITH LOGIN PASSWORD '${APP_PASSWORD}';
CREATE DATABASE "${APP_DB}" OWNER "${APP_USER}";
EOSQL
echo "host replication replicator all scram-sha-256" >> "$PGDATA/pg_hba.conf"
`

// cloneScript runs as an init container on every instance. Instance 0 is the
// primary and is skipped; replicas are cloned from the primary with pg_basebackup.
const cloneScript = `#!/bin/bash
set -euo pipefail
ordinal="${HOSTNAME##*-}"
if [ "$ordinal" = "0" ]; then
  echo "pgkeeper: instance 0 is the primary, nothing to clone"
  exit 0
fi
if [ -s "$PGDATA/PG_VERSION" ]; then
  echo "pgkeeper: data directory already initialised"
  exit 0
fi
echo "pgkeeper: waiting for primary ${PRIMARY_HOST}"
until pg_isready -h "$PRIMARY_HOST" -p 5432 -U replicator; do sleep 2; done
rm -rf "${PGDATA:?}"/* 2>/dev/null || true
mkdir -p "$PGDATA"
chmod 700 "$PGDATA"
echo "pgkeeper: cloning primary with pg_basebackup"
PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup -h "$PRIMARY_HOST" -U replicator -D "$PGDATA" -X stream -R -P
echo "pgkeeper: replica ready"
`

const dumpScript = `#!/bin/bash
set -euo pipefail
until pg_isready -h "$PGHOST" -U postgres; do sleep 2; done
echo "pgkeeper: dumping ${PGHOST}"
pg_dumpall -h "$PGHOST" -U postgres --no-role-passwords | gzip > /work/dump.sql.gz
ls -lh /work/dump.sql.gz
`

const uploadScript = `#!/bin/bash
set -euo pipefail
aws configure set default.s3.addressing_style path
aws --endpoint-url "$S3_ENDPOINT" s3 cp /work/dump.sql.gz "s3://${S3_BUCKET}/${OBJECT_KEY}"
echo "pgkeeper: uploaded s3://${S3_BUCKET}/${OBJECT_KEY}"
cutoff=$(date -u -d "-${RETENTION_DAYS} days" +%Y-%m-%dT%H:%M:%S)
aws --endpoint-url "$S3_ENDPOINT" s3api list-objects-v2 --bucket "$S3_BUCKET" --prefix "$S3_PREFIX" \
  --query 'Contents[].[Key,LastModified]' --output text | while read -r key modified; do
  if [ -z "$key" ] || [ "$key" = "None" ]; then continue; fi
  if [[ "$modified" < "$cutoff" ]]; then
    echo "pgkeeper: retention removes $key"
    aws --endpoint-url "$S3_ENDPOINT" s3 rm "s3://${S3_BUCKET}/${key}"
  fi
done
`

const downloadScript = `#!/bin/bash
set -euo pipefail
aws configure set default.s3.addressing_style path
aws --endpoint-url "$S3_ENDPOINT" s3 cp "s3://${S3_BUCKET}/${OBJECT_KEY}" /work/dump.sql.gz
echo "pgkeeper: downloaded s3://${S3_BUCKET}/${OBJECT_KEY}"
`

const restoreScript = `#!/bin/bash
set -euo pipefail
until pg_isready -h "$PGHOST" -U postgres; do sleep 2; done
echo "pgkeeper: restoring into ${PGHOST}"
gunzip -c /work/dump.sql.gz | psql -h "$PGHOST" -U postgres -d postgres -q
echo "pgkeeper: restore finished"
`

func mutateScriptsConfigMap(c *pgv1.PostgresCluster, cm *corev1.ConfigMap) {
	cm.Labels = mergeLabels(cm.Labels, commonLabels(c))
	cm.Data = map[string]string{
		"initdb.sh": initdbScript,
		"clone.sh":  cloneScript,
	}
}

func mutateHeadlessService(c *pgv1.PostgresCluster, svc *corev1.Service) {
	svc.Labels = mergeLabels(svc.Labels, commonLabels(c))
	svc.Spec.ClusterIP = corev1.ClusterIPNone
	svc.Spec.PublishNotReadyAddresses = true
	svc.Spec.Selector = selectorLabels(c)
	ports := []corev1.ServicePort{postgresServicePort()}
	if c.Spec.Monitoring.EnableExporter {
		ports = append(ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       exporterPort,
			TargetPort: intstr.FromString("metrics"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	svc.Spec.Ports = ports
}

func mutateRWService(c *pgv1.PostgresCluster, svc *corev1.Service) {
	svc.Labels = mergeLabels(svc.Labels, commonLabels(c))
	svc.Labels[labelComponent] = "primary"
	sel := selectorLabels(c)
	sel[labelPodName] = primaryPodName(c)
	svc.Spec.Selector = sel
	svc.Spec.Ports = []corev1.ServicePort{postgresServicePort()}
}

func mutateROService(c *pgv1.PostgresCluster, svc *corev1.Service) {
	svc.Labels = mergeLabels(svc.Labels, commonLabels(c))
	svc.Labels[labelComponent] = "read"
	svc.Spec.Selector = selectorLabels(c)
	svc.Spec.Ports = []corev1.ServicePort{postgresServicePort()}
}

func postgresServicePort() corev1.ServicePort {
	return corev1.ServicePort{
		Name:       "postgres",
		Port:       postgresPort,
		TargetPort: intstr.FromString("postgres"),
		Protocol:   corev1.ProtocolTCP,
	}
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			Key:                  key,
		}},
	}
}

func pgIsReady() []string {
	return []string{"pg_isready", "-h", "127.0.0.1", "-p", strconv.Itoa(postgresPort), "-U", "postgres"}
}

// mutateStatefulSet sets the desired state on sts. Immutable fields (selector,
// serviceName, volumeClaimTemplates) are only set when the object is new.
func mutateStatefulSet(c *pgv1.PostgresCluster, sts *appsv1.StatefulSet, images Images) {
	labels := commonLabels(c)
	sts.Labels = mergeLabels(sts.Labels, labels)

	if sts.CreationTimestamp.IsZero() {
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(c)}
		sts.Spec.ServiceName = headlessServiceName(c)
		sts.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: c.Spec.Storage.StorageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: c.Spec.Storage.Size},
				},
			},
		}}
	}

	sts.Spec.Replicas = ptr.To(c.DesiredInstances())
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType}
	sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	sts.Spec.Template = podTemplate(c, images)
}

func podTemplate(c *pgv1.PostgresCluster, images Images) corev1.PodTemplateSpec {
	image := postgresImage(c, images)
	su := superuserSecretName(c)

	baseEnv := []corev1.EnvVar{
		{Name: "PGDATA", Value: pgDataPath},
		secretEnv("REPLICATION_PASSWORD", su, keyReplicationPassword),
	}

	dataMount := corev1.VolumeMount{Name: "data", MountPath: dataMountPath}

	clone := corev1.Container{
		Name:    "clone",
		Image:   image,
		Command: []string{"/bin/bash", "/pgkeeper/clone.sh"},
		Env: append([]corev1.EnvVar{
			{Name: "PRIMARY_HOST", Value: serviceHost(c, rwServiceName(c))},
		}, baseEnv...),
		VolumeMounts: []corev1.VolumeMount{
			dataMount,
			{Name: "scripts", MountPath: "/pgkeeper"},
		},
		SecurityContext: containerSecurityContext(),
	}

	pgEnv := append([]corev1.EnvVar{
		secretEnv("POSTGRES_PASSWORD", su, keySuperuserPassword),
		{Name: "APP_USER", Value: c.DatabaseOwner()},
		{Name: "APP_DB", Value: c.DatabaseName()},
		secretEnv("APP_PASSWORD", appSecretName(c), "password"),
	}, baseEnv...)

	postgres := corev1.Container{
		Name:  "postgres",
		Image: image,
		Args:  []string{"postgres", "-c", "wal_keep_size=512MB", "-c", "max_wal_senders=10"},
		Env:   pgEnv,
		Ports: []corev1.ContainerPort{{Name: "postgres", ContainerPort: postgresPort, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: []corev1.VolumeMount{
			dataMount,
			{Name: "initdb", MountPath: "/docker-entrypoint-initdb.d"},
		},
		Resources: c.Spec.Resources,
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: pgIsReady()}},
			PeriodSeconds:    5,
			FailureThreshold: 60,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: pgIsReady()}},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: pgIsReady()}},
			PeriodSeconds:    10,
			FailureThreshold: 6,
		},
		SecurityContext: containerSecurityContext(),
	}

	containers := []corev1.Container{postgres}
	annotations := map[string]string{}
	if c.Spec.Monitoring.EnableExporter {
		containers = append(containers, corev1.Container{
			Name:  "exporter",
			Image: images.Exporter,
			Env: []corev1.EnvVar{
				{Name: "DATA_SOURCE_URI", Value: fmt.Sprintf("127.0.0.1:%d/postgres?sslmode=disable", postgresPort)},
				{Name: "DATA_SOURCE_USER", Value: "postgres"},
				secretEnv("DATA_SOURCE_PASS", su, keySuperuserPassword),
			},
			Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: exporterPort, Protocol: corev1.ProtocolTCP}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    mustQuantity("10m"),
					corev1.ResourceMemory: mustQuantity("32Mi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: mustQuantity("128Mi")},
			},
			SecurityContext: containerSecurityContext(),
		})
		annotations["prometheus.io/scrape"] = "true"
		annotations["prometheus.io/port"] = strconv.Itoa(exporterPort)
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: commonLabels(c), Annotations: annotations},
		Spec: corev1.PodSpec{
			SecurityContext:               podSecurityContext(),
			TerminationGracePeriodSeconds: ptr.To[int64](30),
			InitContainers:                []corev1.Container{clone},
			Containers:                    containers,
			Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey:   corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
					},
				}},
			}},
			Volumes: []corev1.Volume{
				{Name: "scripts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: scriptsConfigMapName(c)},
					Items:                []corev1.KeyToPath{{Key: "clone.sh", Path: "clone.sh"}},
					DefaultMode:          ptr.To[int32](0o555),
				}}},
				{Name: "initdb", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: scriptsConfigMapName(c)},
					Items:                []corev1.KeyToPath{{Key: "initdb.sh", Path: "10-pgkeeper.sh"}},
					DefaultMode:          ptr.To[int32](0o555),
				}}},
			},
		},
	}
}

func podSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsUser:      ptr.To(postgresUID),
		RunAsGroup:     ptr.To(postgresUID),
		FSGroup:        ptr.To(postgresUID),
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// s3Env returns environment variables shared by backup and restore jobs.
func s3Env(c *pgv1.PostgresCluster, objectKey string) []corev1.EnvVar {
	s3 := c.Spec.Backup.S3
	region := s3.Region
	if region == "" {
		region = "us-east-1"
	}
	return []corev1.EnvVar{
		{Name: "HOME", Value: "/tmp"},
		{Name: "S3_ENDPOINT", Value: s3.Endpoint},
		{Name: "S3_BUCKET", Value: s3.Bucket},
		{Name: "S3_PREFIX", Value: backupPrefix(c)},
		{Name: "OBJECT_KEY", Value: objectKey},
		{Name: "AWS_DEFAULT_REGION", Value: region},
		{Name: "AWS_REQUEST_CHECKSUM_CALCULATION", Value: "when_required"},
		{Name: "AWS_RESPONSE_CHECKSUM_VALIDATION", Value: "when_required"},
		secretEnv("AWS_ACCESS_KEY_ID", s3.CredentialsSecret.Name, keyAccessKeyID),
		secretEnv("AWS_SECRET_ACCESS_KEY", s3.CredentialsSecret.Name, keySecretAccessKey),
	}
}

func pgClientEnv(c *pgv1.PostgresCluster) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: serviceHost(c, rwServiceName(c))},
		secretEnv("PGPASSWORD", superuserSecretName(c), keySuperuserPassword),
	}
}

func jobPodSpec(initContainer, container corev1.Container) corev1.PodSpec {
	work := corev1.VolumeMount{Name: "work", MountPath: "/work"}
	initContainer.VolumeMounts = append(initContainer.VolumeMounts, work)
	container.VolumeMounts = append(container.VolumeMounts, work)
	initContainer.SecurityContext = containerSecurityContext()
	container.SecurityContext = containerSecurityContext()
	return corev1.PodSpec{
		RestartPolicy:   corev1.RestartPolicyNever,
		SecurityContext: podSecurityContext(),
		InitContainers:  []corev1.Container{initContainer},
		Containers:      []corev1.Container{container},
		Volumes: []corev1.Volume{{
			Name:         "work",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}
}

// buildBackupJob returns the Job that dumps the cluster and uploads it to S3.
func buildBackupJob(c *pgv1.PostgresCluster, b *pgv1.PostgresBackup, objectKey string, images Images) *batchv1.Job {
	retention := c.Spec.Backup.RetentionDays
	if retention < 1 {
		retention = 7
	}
	dump := corev1.Container{
		Name:    "dump",
		Image:   postgresImage(c, images),
		Command: []string{"/bin/bash", "-c", dumpScript},
		Env:     pgClientEnv(c),
	}
	upload := corev1.Container{
		Name:    "upload",
		Image:   images.Tools,
		Command: []string{"/bin/bash", "-c", uploadScript},
		Env: append(s3Env(c, objectKey),
			corev1.EnvVar{Name: "RETENTION_DAYS", Value: strconv.Itoa(int(retention))}),
	}
	labels := commonLabels(c)
	labels[labelComponent] = "backup"
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Name + "-job",
			Namespace: b.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](2),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       jobPodSpec(dump, upload),
			},
		},
	}
}

// buildRestoreJob returns the Job that downloads a backup and loads it into the primary.
func buildRestoreJob(c *pgv1.PostgresCluster, images Images) *batchv1.Job {
	key := c.Spec.Bootstrap.FromBackup.ObjectKey
	download := corev1.Container{
		Name:    "download",
		Image:   images.Tools,
		Command: []string{"/bin/bash", "-c", downloadScript},
		Env:     s3Env(c, key),
	}
	restore := corev1.Container{
		Name:    "restore",
		Image:   postgresImage(c, images),
		Command: []string{"/bin/bash", "-c", restoreScript},
		Env:     pgClientEnv(c),
	}
	labels := commonLabels(c)
	labels[labelComponent] = "restore"
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restoreJobName(c),
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](2),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       jobPodSpec(download, restore),
			},
		},
	}
}

func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }

func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
