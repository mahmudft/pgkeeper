package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

// PostgresClusterReconciler reconciles a PostgresCluster object.
type PostgresClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Images   Images
	// Now returns the current time; overridable in tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgres.pgkeeper.dev,resources=postgresbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the cluster towards the desired state.
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cluster := &pgv1.PostgresCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			forgetClusterMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.ensureSecrets(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("secrets: %w", err)
	}
	if err := r.ensureOwned(ctx, cluster, &corev1.ConfigMap{ObjectMeta: objMeta(cluster, scriptsConfigMapName(cluster))},
		func(o client.Object) { mutateScriptsConfigMap(cluster, o.(*corev1.ConfigMap)) }); err != nil {
		return ctrl.Result{}, fmt.Errorf("configmap: %w", err)
	}
	if err := r.ensureServices(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("services: %w", err)
	}

	sts := &appsv1.StatefulSet{ObjectMeta: objMeta(cluster, cluster.Name)}
	if err := r.ensureOwned(ctx, cluster, sts,
		func(o client.Object) { mutateStatefulSet(cluster, o.(*appsv1.StatefulSet), r.Images) }); err != nil {
		return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
	}
	if err := r.expandVolumes(ctx, cluster); err != nil {
		log.Error(err, "volume expansion failed")
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "VolumeExpansionFailed", "%v", err)
	}

	// Work on a copy of the status and write it once at the end.
	status := cluster.Status.DeepCopy()
	status.ObservedGeneration = cluster.Generation
	status.ReadyInstances = sts.Status.ReadyReplicas
	status.PrimaryEndpoint = serviceHost(cluster, rwServiceName(cluster))
	status.ReplicaEndpoint = serviceHost(cluster, roServiceName(cluster))
	status.AppSecret = appSecretName(cluster)

	primaryReady := sts.Status.ReadyReplicas > 0
	restoring, err := r.reconcileRestore(ctx, cluster, status, primaryReady)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("restore: %w", err)
	}

	var requeueAfter time.Duration
	if !restoring {
		requeueAfter, err = r.reconcileSchedule(ctx, cluster, status)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("schedule: %w", err)
		}
	}

	desired := cluster.DesiredInstances()
	switch {
	case restoring:
		status.Phase = pgv1.PhaseRestoring
	case status.ReadyInstances >= desired:
		status.Phase = pgv1.PhaseReady
	case status.ReadyInstances > 0:
		status.Phase = pgv1.PhaseDegraded
	default:
		status.Phase = pgv1.PhaseProvisioning
	}
	ready := metav1.Condition{
		Type:               pgv1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             status.Phase,
		Message:            fmt.Sprintf("%d/%d instances ready", status.ReadyInstances, desired),
		ObservedGeneration: cluster.Generation,
	}
	if status.Phase == pgv1.PhaseReady {
		ready.Status = metav1.ConditionTrue
	}
	if prev := meta.FindStatusCondition(status.Conditions, pgv1.ConditionReady); prev != nil && prev.Status != ready.Status {
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, status.Phase, "%s", ready.Message)
	}
	meta.SetStatusCondition(&status.Conditions, ready)

	clusterReadyInstances.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(status.ReadyInstances))
	clusterDesiredInstances.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(desired))

	cluster.Status = *status
	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Poll while instances come up so the phase stays fresh even without events.
	if status.Phase != pgv1.PhaseReady && (requeueAfter == 0 || requeueAfter > 15*time.Second) {
		requeueAfter = 15 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func objMeta(c *pgv1.PostgresCluster, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: c.Namespace}
}

// ensureOwned creates or updates obj with the controller reference set.
func (r *PostgresClusterReconciler) ensureOwned(ctx context.Context, c *pgv1.PostgresCluster, obj client.Object, mutate func(client.Object)) error {
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		mutate(obj)
		return controllerutil.SetControllerReference(c, obj, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("created", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
	}
	return nil
}

func (r *PostgresClusterReconciler) ensureServices(ctx context.Context, c *pgv1.PostgresCluster) error {
	services := []struct {
		name   string
		mutate func(*pgv1.PostgresCluster, *corev1.Service)
	}{
		{headlessServiceName(c), mutateHeadlessService},
		{rwServiceName(c), mutateRWService},
		{roServiceName(c), mutateROService},
	}
	for _, s := range services {
		svc := &corev1.Service{ObjectMeta: objMeta(c, s.name)}
		mutate := s.mutate
		if err := r.ensureOwned(ctx, c, svc, func(o client.Object) { mutate(c, o.(*corev1.Service)) }); err != nil {
			return err
		}
	}
	return nil
}

// ensureSecrets creates credentials once. Existing secrets are never rotated.
func (r *PostgresClusterReconciler) ensureSecrets(ctx context.Context, c *pgv1.PostgresCluster) error {
	su := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: superuserSecretName(c)}, su)
	if apierrors.IsNotFound(err) {
		su = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: superuserSecretName(c), Namespace: c.Namespace, Labels: commonLabels(c)},
			Type:       corev1.SecretTypeBasicAuth,
			StringData: map[string]string{
				"username":             "postgres",
				keySuperuserPassword:   randomPassword(),
				keyReplicationPassword: randomPassword(),
			},
		}
		if err := r.createOwned(ctx, c, su); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	app := &corev1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: appSecretName(c)}, app)
	if apierrors.IsNotFound(err) {
		host := serviceHost(c, rwServiceName(c))
		password := randomPassword()
		app = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: appSecretName(c), Namespace: c.Namespace, Labels: commonLabels(c)},
			Type:       corev1.SecretTypeBasicAuth,
			StringData: map[string]string{
				"username": c.DatabaseOwner(),
				"password": password,
				"dbname":   c.DatabaseName(),
				"host":     host,
				"port":     fmt.Sprint(postgresPort),
				"uri": fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
					c.DatabaseOwner(), password, host, postgresPort, c.DatabaseName()),
			},
		}
		return r.createOwned(ctx, c, app)
	}
	return err
}

func (r *PostgresClusterReconciler) createOwned(ctx context.Context, c *pgv1.PostgresCluster, obj client.Object) error {
	if err := controllerutil.SetControllerReference(c, obj, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// expandVolumes grows existing PVCs when spec.storage.size increases.
func (r *PostgresClusterReconciler) expandVolumes(ctx context.Context, c *pgv1.PostgresCluster) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(c.Namespace), client.MatchingLabels(selectorLabels(c))); err != nil {
		return err
	}
	want := c.Spec.Storage.Size
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		have := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if want.Cmp(have) <= 0 {
			continue
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		if err := r.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("expand %s: %w", pvc.Name, err)
		}
		r.Recorder.Eventf(c, corev1.EventTypeNormal, "VolumeExpanded", "Requested %s for %s", want.String(), pvc.Name)
	}
	return nil
}

// reconcileRestore runs the restore Job once after the primary is ready.
// It returns true while the restore is still in progress.
func (r *PostgresClusterReconciler) reconcileRestore(ctx context.Context, c *pgv1.PostgresCluster, status *pgv1.PostgresClusterStatus, primaryReady bool) (bool, error) {
	if c.Spec.Bootstrap == nil || c.Spec.Bootstrap.FromBackup == nil || c.Spec.Backup == nil {
		return false, nil
	}
	if meta.IsStatusConditionTrue(status.Conditions, pgv1.ConditionRestored) {
		return false, nil
	}
	if prev := meta.FindStatusCondition(status.Conditions, pgv1.ConditionRestored); prev != nil && prev.Reason == "RestoreFailed" {
		return false, nil
	}
	if !primaryReady {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: pgv1.ConditionRestored, Status: metav1.ConditionFalse, Reason: "WaitingForPrimary",
			Message: "restore starts once the primary is ready", ObservedGeneration: c.Generation,
		})
		return true, nil
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: restoreJobName(c)}, job)
	if apierrors.IsNotFound(err) {
		job = buildRestoreJob(c, r.Images)
		if err := r.createOwned(ctx, c, job); err != nil {
			return true, err
		}
		r.Recorder.Eventf(c, corev1.EventTypeNormal, "RestoreStarted", "Restoring from %s", c.Spec.Bootstrap.FromBackup.ObjectKey)
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: pgv1.ConditionRestored, Status: metav1.ConditionFalse, Reason: "RestoreRunning",
			Message: "restore job created", ObservedGeneration: c.Generation,
		})
		return true, nil
	} else if err != nil {
		return true, err
	}

	switch {
	case job.Status.Succeeded > 0:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: pgv1.ConditionRestored, Status: metav1.ConditionTrue, Reason: "RestoreSucceeded",
			Message: "restored from " + c.Spec.Bootstrap.FromBackup.ObjectKey, ObservedGeneration: c.Generation,
		})
		r.Recorder.Event(c, corev1.EventTypeNormal, "RestoreSucceeded", "Restore finished")
		return false, nil
	case jobFailed(job):
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: pgv1.ConditionRestored, Status: metav1.ConditionFalse, Reason: "RestoreFailed",
			Message: "restore job failed, see logs of job " + job.Name, ObservedGeneration: c.Generation,
		})
		r.Recorder.Event(c, corev1.EventTypeWarning, "RestoreFailed", "Restore job failed")
		return false, nil
	default:
		return true, nil
	}
}

// reconcileSchedule creates PostgresBackups according to spec.backup.schedule
// and prunes old ones. It returns when the next run is due.
func (r *PostgresClusterReconciler) reconcileSchedule(ctx context.Context, c *pgv1.PostgresCluster, status *pgv1.PostgresClusterStatus) (time.Duration, error) {
	if c.Spec.Backup == nil || c.Spec.Backup.Schedule == "" {
		meta.RemoveStatusCondition(&status.Conditions, pgv1.ConditionBackupScheduled)
		return 0, nil
	}
	sched, err := parseSchedule(c.Spec.Backup.Schedule)
	if err != nil {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: pgv1.ConditionBackupScheduled, Status: metav1.ConditionFalse, Reason: "InvalidSchedule",
			Message: err.Error(), ObservedGeneration: c.Generation,
		})
		return 0, nil
	}

	now := r.now()
	since := c.CreationTimestamp.Time
	if status.LastScheduledBackupTime != nil {
		since = status.LastScheduledBackupTime.Time
	}
	missed, next := nextRuns(sched, since, now)

	// Only back up a cluster that is serving.
	if !missed.IsZero() && status.ReadyInstances > 0 {
		backup := &pgv1.PostgresBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", c.Name, missed.Unix()),
				Namespace: c.Namespace,
				Labels:    map[string]string{LabelCluster: c.Name, LabelScheduled: "true"},
			},
			Spec: pgv1.PostgresBackupSpec{Cluster: c.Name},
		}
		if err := r.createOwned(ctx, c, backup); err != nil {
			return 0, err
		}
		status.LastScheduledBackupTime = &metav1.Time{Time: missed}
		r.Recorder.Eventf(c, corev1.EventTypeNormal, "BackupScheduled", "Created backup %s", backup.Name)
	}

	if err := r.pruneBackups(ctx, c); err != nil {
		return 0, err
	}

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: pgv1.ConditionBackupScheduled, Status: metav1.ConditionTrue, Reason: "Scheduled",
		Message: "next backup at " + next.UTC().Format(time.RFC3339), ObservedGeneration: c.Generation,
	})
	return next.Sub(now) + time.Second, nil
}

// pruneBackups keeps the newest historyLimit finished scheduled backups.
func (r *PostgresClusterReconciler) pruneBackups(ctx context.Context, c *pgv1.PostgresCluster) error {
	limit := int(c.Spec.Backup.HistoryLimit)
	if limit < 1 {
		limit = 5
	}
	list := &pgv1.PostgresBackupList{}
	if err := r.List(ctx, list, client.InNamespace(c.Namespace),
		client.MatchingLabels{LabelCluster: c.Name, LabelScheduled: "true"}); err != nil {
		return err
	}
	var finished []pgv1.PostgresBackup
	for _, b := range list.Items {
		if b.IsFinished() {
			finished = append(finished, b)
		}
	}
	if len(finished) <= limit {
		return nil
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].CreationTimestamp.After(finished[j].CreationTimestamp.Time)
	})
	for i := limit; i < len(finished); i++ {
		if err := r.Delete(ctx, &finished[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func jobFailed(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomPassword() string {
	b := make([]byte, 24)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgv1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&pgv1.PostgresBackup{}).
		Named("postgrescluster").
		Complete(r)
}
