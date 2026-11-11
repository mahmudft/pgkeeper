package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

// PostgresBackupReconciler reconciles a PostgresBackup object.
type PostgresBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Images   Images
}

// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile runs a backup Job and mirrors its state into the PostgresBackup status.
func (r *PostgresBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	backup := &pgv1.PostgresBackup{}
	if err := r.Get(ctx, req.NamespacedName, backup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if backup.IsFinished() || !backup.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	cluster := &pgv1.PostgresCluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: backup.Spec.Cluster}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, backup, nil, pgv1.BackupFailed, fmt.Sprintf("cluster %q not found", backup.Spec.Cluster))
		}
		return ctrl.Result{}, err
	}
	if cluster.Spec.Backup == nil {
		return r.finish(ctx, backup, cluster, pgv1.BackupFailed, "cluster has no spec.backup configuration")
	}

	objectKey := backupObjectKey(cluster, backup.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: backup.Name + "-job"}, job)
	if apierrors.IsNotFound(err) {
		job = buildBackupJob(cluster, backup, objectKey, r.Images)
		if err := controllerutil.SetControllerReference(backup, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		log.Info("backup job created", "job", job.Name, "objectKey", objectKey)
		r.Recorder.Eventf(backup, corev1.EventTypeNormal, "Started", "Backup job %s created", job.Name)

		backup.Status.Phase = pgv1.BackupRunning
		backup.Status.JobName = job.Name
		backup.Status.ObjectKey = objectKey
		backup.Status.StartedAt = &metav1.Time{Time: time.Now()}
		backup.Status.Message = ""
		return ctrl.Result{}, r.Status().Update(ctx, backup)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case job.Status.Succeeded > 0:
		return r.finish(ctx, backup, cluster, pgv1.BackupCompleted, "backup uploaded")
	case jobFailed(job):
		return r.finish(ctx, backup, cluster, pgv1.BackupFailed, "backup job failed, see logs of job "+job.Name)
	}

	if backup.Status.Phase != pgv1.BackupRunning {
		backup.Status.Phase = pgv1.BackupRunning
		backup.Status.JobName = job.Name
		backup.Status.ObjectKey = objectKey
		return ctrl.Result{}, r.Status().Update(ctx, backup)
	}
	return ctrl.Result{}, nil
}

// finish moves the backup into a terminal phase and records metrics.
func (r *PostgresBackupReconciler) finish(ctx context.Context, b *pgv1.PostgresBackup, c *pgv1.PostgresCluster, phase, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	b.Status.Phase = phase
	b.Status.Message = msg
	b.Status.CompletedAt = &now
	if err := r.Status().Update(ctx, b); err != nil {
		return ctrl.Result{}, err
	}

	result := "success"
	eventType := corev1.EventTypeNormal
	if phase == pgv1.BackupFailed {
		result = "failure"
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Event(b, eventType, phase, msg)
	backupsTotal.WithLabelValues(b.Namespace, b.Spec.Cluster, result).Inc()
	if b.Status.StartedAt != nil {
		backupDuration.WithLabelValues(b.Namespace, b.Spec.Cluster).Observe(now.Sub(b.Status.StartedAt.Time).Seconds())
	}

	if phase == pgv1.BackupCompleted && c != nil {
		backupLastSuccess.WithLabelValues(b.Namespace, b.Spec.Cluster).Set(float64(now.Unix()))
		patch := client.MergeFrom(c.DeepCopy())
		c.Status.LastSuccessfulBackup = b.Status.ObjectKey
		if err := r.Status().Patch(ctx, c, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgv1.PostgresBackup{}).
		Owns(&batchv1.Job{}).
		Named("postgresbackup").
		Complete(r)
}
