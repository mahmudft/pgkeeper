package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Backup phases.
const (
	BackupPending   = "Pending"
	BackupRunning   = "Running"
	BackupCompleted = "Completed"
	BackupFailed    = "Failed"
)

// PostgresBackupSpec defines a one-off logical backup of a PostgresCluster.
type PostgresBackupSpec struct {
	// Name of the PostgresCluster in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster is immutable"
	Cluster string `json:"cluster"`
}

// PostgresBackupStatus defines the observed state of PostgresBackup.
type PostgresBackupStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// Object key of the backup inside the bucket.
	// +optional
	ObjectKey string `json:"objectKey,omitempty"`

	// +optional
	JobName string `json:"jobName,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgb;pgbackup,categories=pgkeeper
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Object",type=string,JSONPath=`.status.objectKey`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresBackup is the Schema for the postgresbackups API.
type PostgresBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresBackupSpec   `json:"spec,omitempty"`
	Status PostgresBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresBackupList contains a list of PostgresBackup.
type PostgresBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresBackup{}, &PostgresBackupList{})
}

// IsFinished reports whether the backup reached a terminal phase.
func (b *PostgresBackup) IsFinished() bool {
	return b.Status.Phase == BackupCompleted || b.Status.Phase == BackupFailed
}
