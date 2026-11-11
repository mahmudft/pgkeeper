package controller

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

var testNow = time.Date(2026, 1, 10, 12, 30, 0, 0, time.UTC)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := pgv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newCluster(mod func(*pgv1.PostgresCluster)) *pgv1.PostgresCluster {
	c := &pgv1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "demo",
			Namespace:         "default",
			UID:               "uid-demo",
			Generation:        1,
			CreationTimestamp: metav1.NewTime(testNow.Add(-2 * time.Hour)),
		},
		Spec: pgv1.PostgresClusterSpec{
			Version:   "16",
			Instances: 2,
			Storage:   pgv1.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func withBackup(schedule string) func(*pgv1.PostgresCluster) {
	return func(c *pgv1.PostgresCluster) {
		c.Spec.Backup = &pgv1.BackupSpec{
			Schedule:      schedule,
			RetentionDays: 3,
			HistoryLimit:  2,
			S3: pgv1.S3Spec{
				Endpoint:          "http://minio.minio.svc:9000",
				Bucket:            "backups",
				Path:              "/pg/",
				CredentialsSecret: corev1.LocalObjectReference{Name: "s3-creds"},
			},
		}
	}
}

type env struct {
	client  client.Client
	cluster *PostgresClusterReconciler
	backup  *PostgresBackupReconciler
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&pgv1.PostgresCluster{}, &pgv1.PostgresBackup{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		Build()
	rec := record.NewFakeRecorder(100)
	return &env{
		client:  c,
		cluster: &PostgresClusterReconciler{Client: c, Scheme: s, Recorder: rec, Images: DefaultImages(), Now: func() time.Time { return testNow }},
		backup:  &PostgresBackupReconciler{Client: c, Scheme: s, Recorder: rec, Images: DefaultImages()},
	}
}

func req(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func (e *env) reconcileCluster(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := e.cluster.Reconcile(context.Background(), req("demo"))
	if err != nil {
		t.Fatalf("reconcile cluster: %v", err)
	}
	return res
}

func (e *env) get(t *testing.T, name string, obj client.Object) {
	t.Helper()
	if err := e.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
}

func (e *env) setReady(t *testing.T, n int32) {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	e.get(t, "demo", sts)
	sts.Status.Replicas = n
	sts.Status.ReadyReplicas = n
	if err := e.client.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileCreatesResources(t *testing.T) {
	e := newEnv(t, newCluster(func(c *pgv1.PostgresCluster) { c.Spec.Monitoring.EnableExporter = true }))
	e.reconcileCluster(t)

	sts := &appsv1.StatefulSet{}
	e.get(t, "demo", sts)
	if got := *sts.Spec.Replicas; got != 2 {
		t.Errorf("replicas = %d, want 2", got)
	}
	if sts.Spec.ServiceName != "demo-headless" {
		t.Errorf("serviceName = %q", sts.Spec.ServiceName)
	}
	if img := sts.Spec.Template.Spec.Containers[0].Image; img != "postgres:16" {
		t.Errorf("image = %q", img)
	}
	if n := len(sts.Spec.Template.Spec.Containers); n != 2 {
		t.Errorf("containers = %d, want postgres + exporter", n)
	}
	if len(sts.OwnerReferences) != 1 || sts.OwnerReferences[0].Kind != "PostgresCluster" {
		t.Errorf("missing owner reference: %+v", sts.OwnerReferences)
	}

	rw := &corev1.Service{}
	e.get(t, "demo-rw", rw)
	if rw.Spec.Selector[labelPodName] != "demo-0" {
		t.Errorf("rw service must select the primary pod, got %v", rw.Spec.Selector)
	}
	e.get(t, "demo-ro", &corev1.Service{})
	headless := &corev1.Service{}
	e.get(t, "demo-headless", headless)
	if headless.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("headless service has clusterIP %q", headless.Spec.ClusterIP)
	}
	e.get(t, "demo-scripts", &corev1.ConfigMap{})

	app := &corev1.Secret{}
	e.get(t, "demo-app", app)
	if app.StringData["dbname"] != "app" || !strings.HasPrefix(app.StringData["uri"], "postgresql://app:") {
		t.Errorf("unexpected app secret: %v", app.StringData)
	}

	cluster := &pgv1.PostgresCluster{}
	e.get(t, "demo", cluster)
	if cluster.Status.Phase != pgv1.PhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", cluster.Status.Phase)
	}
	if cluster.Status.PrimaryEndpoint != "demo-rw.default.svc" {
		t.Errorf("primaryEndpoint = %q", cluster.Status.PrimaryEndpoint)
	}
}

func TestSecretsAreStable(t *testing.T) {
	e := newEnv(t, newCluster(nil))
	e.reconcileCluster(t)
	first := &corev1.Secret{}
	e.get(t, "demo-superuser", first)

	e.reconcileCluster(t)
	second := &corev1.Secret{}
	e.get(t, "demo-superuser", second)
	if first.StringData[keySuperuserPassword] != second.StringData[keySuperuserPassword] {
		t.Fatal("superuser password changed between reconciles")
	}
}

func TestPhaseFollowsReadyInstances(t *testing.T) {
	e := newEnv(t, newCluster(nil))
	e.reconcileCluster(t)

	e.setReady(t, 1)
	e.reconcileCluster(t)
	c := &pgv1.PostgresCluster{}
	e.get(t, "demo", c)
	if c.Status.Phase != pgv1.PhaseDegraded {
		t.Errorf("phase = %q, want Degraded", c.Status.Phase)
	}

	e.setReady(t, 2)
	e.reconcileCluster(t)
	e.get(t, "demo", c)
	if c.Status.Phase != pgv1.PhaseReady || !meta.IsStatusConditionTrue(c.Status.Conditions, pgv1.ConditionReady) {
		t.Errorf("phase = %q conditions = %+v, want Ready", c.Status.Phase, c.Status.Conditions)
	}
}

func TestScheduledBackupIsCreated(t *testing.T) {
	e := newEnv(t, newCluster(withBackup("0 * * * *")))
	e.reconcileCluster(t)
	e.setReady(t, 2)
	res := e.reconcileCluster(t)

	list := &pgv1.PostgresBackupList{}
	if err := e.client.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("backups = %d, want 1", len(list.Items))
	}
	want := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if list.Items[0].Name != "demo-"+itoa(want.Unix()) {
		t.Errorf("backup name = %q", list.Items[0].Name)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 31*time.Minute {
		t.Errorf("requeueAfter = %v, want ~30m", res.RequeueAfter)
	}

	// A second reconcile within the same slot must not create another backup.
	e.reconcileCluster(t)
	if err := e.client.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("backups = %d after second reconcile, want 1", len(list.Items))
	}
}

func TestBackupLifecycle(t *testing.T) {
	backup := &pgv1.PostgresBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "default", UID: "uid-backup"},
		Spec:       pgv1.PostgresBackupSpec{Cluster: "demo"},
	}
	e := newEnv(t, newCluster(withBackup("")), backup)
	ctx := context.Background()

	if _, err := e.backup.Reconcile(ctx, req("manual")); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	e.get(t, "manual-job", job)
	e.get(t, "manual", backup)
	if backup.Status.Phase != pgv1.BackupRunning {
		t.Fatalf("phase = %q, want Running", backup.Status.Phase)
	}
	if want := "pg/default/demo/manual.sql.gz"; backup.Status.ObjectKey != want {
		t.Errorf("objectKey = %q, want %q", backup.Status.ObjectKey, want)
	}

	job.Status.Succeeded = 1
	if err := e.client.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backup.Reconcile(ctx, req("manual")); err != nil {
		t.Fatal(err)
	}
	e.get(t, "manual", backup)
	if backup.Status.Phase != pgv1.BackupCompleted || backup.Status.CompletedAt == nil {
		t.Fatalf("status = %+v, want Completed", backup.Status)
	}
	c := &pgv1.PostgresCluster{}
	e.get(t, "demo", c)
	if c.Status.LastSuccessfulBackup != backup.Status.ObjectKey {
		t.Errorf("lastSuccessfulBackup = %q", c.Status.LastSuccessfulBackup)
	}
}

func TestBackupFailsWithoutConfig(t *testing.T) {
	backup := &pgv1.PostgresBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "default"},
		Spec:       pgv1.PostgresBackupSpec{Cluster: "demo"},
	}
	e := newEnv(t, newCluster(nil), backup)
	if _, err := e.backup.Reconcile(context.Background(), req("manual")); err != nil {
		t.Fatal(err)
	}
	e.get(t, "manual", backup)
	if backup.Status.Phase != pgv1.BackupFailed {
		t.Fatalf("phase = %q, want Failed", backup.Status.Phase)
	}
}

func TestRestoreFlow(t *testing.T) {
	e := newEnv(t, newCluster(func(c *pgv1.PostgresCluster) {
		withBackup("")(c)
		c.Spec.Bootstrap = &pgv1.BootstrapSpec{FromBackup: &pgv1.RestoreSource{ObjectKey: "pg/default/old/b.sql.gz"}}
	}))
	ctx := context.Background()

	e.reconcileCluster(t)
	c := &pgv1.PostgresCluster{}
	e.get(t, "demo", c)
	if c.Status.Phase != pgv1.PhaseRestoring {
		t.Fatalf("phase = %q, want Restoring", c.Status.Phase)
	}

	e.setReady(t, 1)
	e.reconcileCluster(t)
	job := &batchv1.Job{}
	e.get(t, "demo-restore", job)

	job.Status.Succeeded = 1
	if err := e.client.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	e.setReady(t, 2)
	e.reconcileCluster(t)
	e.get(t, "demo", c)
	if !meta.IsStatusConditionTrue(c.Status.Conditions, pgv1.ConditionRestored) || c.Status.Phase != pgv1.PhaseReady {
		t.Fatalf("phase = %q conditions = %+v", c.Status.Phase, c.Status.Conditions)
	}
}

func TestNextRuns(t *testing.T) {
	sched, err := parseSchedule("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	missed, next := nextRuns(sched, since, time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	if !missed.IsZero() {
		t.Errorf("missed = %v, want zero", missed)
	}
	if want := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}

	missed, _ = nextRuns(sched, since, time.Date(2026, 1, 3, 3, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 3, 2, 0, 0, 0, time.UTC); !missed.Equal(want) {
		t.Errorf("missed = %v, want latest run %v", missed, want)
	}
}

func TestBackupObjectKey(t *testing.T) {
	c := newCluster(withBackup(""))
	if got := backupObjectKey(c, "b1"); got != "pg/default/demo/b1.sql.gz" {
		t.Errorf("key = %q", got)
	}
	c.Spec.Backup.S3.Path = ""
	if got := backupObjectKey(c, "b1"); got != "default/demo/b1.sql.gz" {
		t.Errorf("key = %q", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
