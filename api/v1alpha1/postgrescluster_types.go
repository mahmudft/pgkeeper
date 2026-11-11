package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Cluster phases.
const (
	PhaseProvisioning = "Provisioning"
	PhaseRestoring    = "Restoring"
	PhaseReady        = "Ready"
	PhaseDegraded     = "Degraded"
	PhaseFailed       = "Failed"
)

// Condition types.
const (
	ConditionReady           = "Ready"
	ConditionRestored        = "Restored"
	ConditionBackupScheduled = "BackupScheduled"
)

// PostgresClusterSpec defines the desired state of a PostgreSQL cluster.
type PostgresClusterSpec struct {
	// Major PostgreSQL version. Changing it after creation is not supported.
	// +kubebuilder:validation:Enum="14";"15";"16";"17"
	// +kubebuilder:default="16"
	// +optional
	Version string `json:"version,omitempty"`

	// Image overrides the container image. Defaults to the official postgres:<version> image.
	// +optional
	Image string `json:"image,omitempty"`

	// Number of instances. Instance 0 is the primary, the rest are streaming read replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// Storage for each instance.
	Storage StorageSpec `json:"storage"`

	// Compute resources for the postgres container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Application database and owner created on first start.
	// +kubebuilder:default={}
	// +optional
	Database DatabaseSpec `json:"database,omitempty"`

	// Backup configuration. Required for scheduled backups and restores.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// Bootstrap the cluster from an existing backup.
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// Monitoring configuration.
	// +kubebuilder:default={}
	// +optional
	Monitoring MonitoringSpec `json:"monitoring,omitempty"`
}

// StorageSpec configures persistent storage.
type StorageSpec struct {
	// Volume size. Can be increased later if the storage class allows expansion.
	Size resource.Quantity `json:"size"`

	// Storage class name. Uses the cluster default when empty.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// DatabaseSpec describes the application database.
type DatabaseSpec struct {
	// +kubebuilder:default="app"
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]{0,62}$`
	// +optional
	Name string `json:"name,omitempty"`

	// +kubebuilder:default="app"
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]{0,62}$`
	// +optional
	Owner string `json:"owner,omitempty"`
}

// BackupSpec configures logical backups to S3-compatible storage.
type BackupSpec struct {
	// Cron schedule (standard 5-field format) for automatic backups. Empty disables scheduling.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Delete backup objects in S3 older than this many days.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=7
	// +optional
	RetentionDays int32 `json:"retentionDays,omitempty"`

	// Number of finished scheduled PostgresBackup objects to keep in the cluster.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	HistoryLimit int32 `json:"historyLimit,omitempty"`

	// S3-compatible object storage.
	S3 S3Spec `json:"s3"`
}

// S3Spec points to an S3-compatible bucket.
type S3Spec struct {
	// Endpoint URL, for example http://minio.minio.svc:9000 or https://s3.eu-central-1.amazonaws.com.
	// +kubebuilder:validation:Pattern=`^https?://.+`
	Endpoint string `json:"endpoint"`

	// +kubebuilder:validation:MinLength=3
	Bucket string `json:"bucket"`

	// Key prefix inside the bucket.
	// +optional
	Path string `json:"path,omitempty"`

	// +kubebuilder:default="us-east-1"
	// +optional
	Region string `json:"region,omitempty"`

	// Secret with keys ACCESS_KEY_ID and SECRET_ACCESS_KEY.
	CredentialsSecret corev1.LocalObjectReference `json:"credentialsSecret"`
}

// BootstrapSpec configures how a new cluster is initialised.
type BootstrapSpec struct {
	// Restore from a backup object stored in the bucket configured in spec.backup.s3.
	// +optional
	FromBackup *RestoreSource `json:"fromBackup,omitempty"`
}

// RestoreSource identifies a backup object.
type RestoreSource struct {
	// Object key inside the bucket, as reported in PostgresBackup.status.objectKey.
	// +kubebuilder:validation:MinLength=1
	ObjectKey string `json:"objectKey"`
}

// MonitoringSpec configures metrics.
type MonitoringSpec struct {
	// Run a postgres_exporter sidecar exposing metrics on port 9187.
	// +optional
	EnableExporter bool `json:"enableExporter,omitempty"`
}

// PostgresClusterStatus defines the observed state of PostgresCluster.
type PostgresClusterStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	ReadyInstances int32 `json:"readyInstances,omitempty"`

	// Read-write service host (primary).
	// +optional
	PrimaryEndpoint string `json:"primaryEndpoint,omitempty"`

	// Read-only service host (all instances).
	// +optional
	ReplicaEndpoint string `json:"replicaEndpoint,omitempty"`

	// Secret with application connection details.
	// +optional
	AppSecret string `json:"appSecret,omitempty"`

	// +optional
	LastScheduledBackupTime *metav1.Time `json:"lastScheduledBackupTime,omitempty"`

	// +optional
	LastSuccessfulBackup string `json:"lastSuccessfulBackup,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgc;pgcluster,categories=pgkeeper
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Instances",type=integer,JSONPath=`.spec.instances`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyInstances`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Backup",type=string,JSONPath=`.status.lastSuccessfulBackup`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresCluster is the Schema for the postgresclusters API.
type PostgresCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresClusterSpec   `json:"spec,omitempty"`
	Status PostgresClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresClusterList contains a list of PostgresCluster.
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresCluster{}, &PostgresClusterList{})
}

// DatabaseName returns the application database name with defaults applied.
func (c *PostgresCluster) DatabaseName() string {
	if c.Spec.Database.Name == "" {
		return "app"
	}
	return c.Spec.Database.Name
}

// DatabaseOwner returns the application role name with defaults applied.
func (c *PostgresCluster) DatabaseOwner() string {
	if c.Spec.Database.Owner == "" {
		return "app"
	}
	return c.Spec.Database.Owner
}

// DesiredInstances returns spec.instances with defaults applied.
func (c *PostgresCluster) DesiredInstances() int32 {
	if c.Spec.Instances < 1 {
		return 1
	}
	return c.Spec.Instances
}

// PostgresVersion returns spec.version with defaults applied.
func (c *PostgresCluster) PostgresVersion() string {
	if c.Spec.Version == "" {
		return "16"
	}
	return c.Spec.Version
}
