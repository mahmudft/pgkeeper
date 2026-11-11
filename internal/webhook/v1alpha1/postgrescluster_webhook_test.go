package v1alpha1

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

func cluster(mod func(*pgv1.PostgresCluster)) *pgv1.PostgresCluster {
	c := &pgv1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: pgv1.PostgresClusterSpec{
			Version:   "16",
			Instances: 2,
			Storage:   pgv1.StorageSpec{Size: resource.MustParse("1Gi")},
			Backup: &pgv1.BackupSpec{
				Schedule: "0 2 * * *",
				S3: pgv1.S3Spec{
					Endpoint:          "http://minio:9000",
					Bucket:            "backups",
					CredentialsSecret: corev1.LocalObjectReference{Name: "s3"},
				},
			},
		},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestValidateCreate(t *testing.T) {
	v := &PostgresClusterValidator{}
	tests := []struct {
		name    string
		c       *pgv1.PostgresCluster
		wantErr bool
	}{
		{"valid", cluster(nil), false},
		{"bad schedule", cluster(func(c *pgv1.PostgresCluster) { c.Spec.Backup.Schedule = "every day" }), true},
		{"descriptor schedule", cluster(func(c *pgv1.PostgresCluster) { c.Spec.Backup.Schedule = "@hourly" }), false},
		{"missing credentials", cluster(func(c *pgv1.PostgresCluster) { c.Spec.Backup.S3.CredentialsSecret.Name = "" }), true},
		{"zero storage", cluster(func(c *pgv1.PostgresCluster) { c.Spec.Storage.Size = resource.MustParse("0") }), true},
		{"restore without backup config", cluster(func(c *pgv1.PostgresCluster) {
			c.Spec.Backup = nil
			c.Spec.Bootstrap = &pgv1.BootstrapSpec{FromBackup: &pgv1.RestoreSource{ObjectKey: "x.sql.gz"}}
		}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), tt.c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	v := &PostgresClusterValidator{}
	sc := "fast"
	tests := []struct {
		name    string
		mod     func(*pgv1.PostgresCluster)
		wantErr bool
	}{
		{"scale out", func(c *pgv1.PostgresCluster) { c.Spec.Instances = 3 }, false},
		{"grow storage", func(c *pgv1.PostgresCluster) { c.Spec.Storage.Size = resource.MustParse("5Gi") }, false},
		{"shrink storage", func(c *pgv1.PostgresCluster) { c.Spec.Storage.Size = resource.MustParse("500Mi") }, true},
		{"major upgrade", func(c *pgv1.PostgresCluster) { c.Spec.Version = "17" }, true},
		{"storage class change", func(c *pgv1.PostgresCluster) { c.Spec.Storage.StorageClassName = &sc }, true},
		{"rename database", func(c *pgv1.PostgresCluster) { c.Spec.Database.Name = "other" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.ValidateUpdate(context.Background(), cluster(nil), cluster(tt.mod))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	w, err := (&PostgresClusterValidator{}).ValidateCreate(context.Background(), cluster(func(c *pgv1.PostgresCluster) {
		c.Spec.Instances = 1
		c.Spec.Backup = nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 {
		t.Fatalf("want 2 warnings, got %v", w)
	}
}
