// Package v1alpha1 contains admission webhooks for the v1alpha1 API.
package v1alpha1

import (
	"context"
	"fmt"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pgv1 "github.com/mahmudft/pgkeeper/api/v1alpha1"
)

// SetupPostgresClusterWebhookWithManager registers the validating webhook.
func SetupPostgresClusterWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&pgv1.PostgresCluster{}).
		WithValidator(&PostgresClusterValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-postgres-pgkeeper-dev-v1alpha1-postgrescluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=postgres.pgkeeper.dev,resources=postgresclusters,verbs=create;update,versions=v1alpha1,name=vpostgrescluster-v1alpha1.pgkeeper.dev,admissionReviewVersions=v1

// PostgresClusterValidator validates PostgresCluster create and update requests.
type PostgresClusterValidator struct{}

var _ admission.CustomValidator = &PostgresClusterValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *PostgresClusterValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	c, ok := obj.(*pgv1.PostgresCluster)
	if !ok {
		return nil, fmt.Errorf("expected a PostgresCluster but got %T", obj)
	}
	return warnings(c), toError(c, ValidateSpec(c))
}

// ValidateUpdate implements admission.CustomValidator.
func (v *PostgresClusterValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldC, ok := oldObj.(*pgv1.PostgresCluster)
	if !ok {
		return nil, fmt.Errorf("expected a PostgresCluster but got %T", oldObj)
	}
	newC, ok := newObj.(*pgv1.PostgresCluster)
	if !ok {
		return nil, fmt.Errorf("expected a PostgresCluster but got %T", newObj)
	}
	errs := append(ValidateSpec(newC), ValidateUpdate(oldC, newC)...)
	return warnings(newC), toError(newC, errs)
}

// ValidateDelete implements admission.CustomValidator.
func (v *PostgresClusterValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateSpec checks rules that cannot be expressed with OpenAPI markers.
func ValidateSpec(c *pgv1.PostgresCluster) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if c.Spec.Storage.Size.Sign() <= 0 {
		errs = append(errs, field.Invalid(spec.Child("storage", "size"), c.Spec.Storage.Size.String(), "must be greater than zero"))
	}

	if b := c.Spec.Backup; b != nil {
		bp := spec.Child("backup")
		if b.Schedule != "" {
			if _, err := cron.ParseStandard(b.Schedule); err != nil {
				errs = append(errs, field.Invalid(bp.Child("schedule"), b.Schedule, err.Error()))
			}
		}
		if b.S3.CredentialsSecret.Name == "" {
			errs = append(errs, field.Required(bp.Child("s3", "credentialsSecret", "name"), "secret with S3 credentials is required"))
		}
	}

	if bs := c.Spec.Bootstrap; bs != nil && bs.FromBackup != nil && c.Spec.Backup == nil {
		errs = append(errs, field.Required(spec.Child("backup"), "bootstrap.fromBackup needs spec.backup.s3 to know where to read from"))
	}
	return errs
}

// ValidateUpdate checks transitions that the operator cannot perform safely.
func ValidateUpdate(oldC, newC *pgv1.PostgresCluster) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if oldC.PostgresVersion() != newC.PostgresVersion() {
		errs = append(errs, field.Forbidden(spec.Child("version"), "major version upgrades are not supported"))
	}
	if newC.Spec.Storage.Size.Cmp(oldC.Spec.Storage.Size) < 0 {
		errs = append(errs, field.Forbidden(spec.Child("storage", "size"), "volumes cannot shrink"))
	}
	if !equalStringPtr(oldC.Spec.Storage.StorageClassName, newC.Spec.Storage.StorageClassName) {
		errs = append(errs, field.Forbidden(spec.Child("storage", "storageClassName"), "field is immutable"))
	}
	if oldC.DatabaseName() != newC.DatabaseName() {
		errs = append(errs, field.Forbidden(spec.Child("database", "name"), "field is immutable"))
	}
	if oldC.DatabaseOwner() != newC.DatabaseOwner() {
		errs = append(errs, field.Forbidden(spec.Child("database", "owner"), "field is immutable"))
	}
	return errs
}

func warnings(c *pgv1.PostgresCluster) admission.Warnings {
	var w admission.Warnings
	if c.DesiredInstances() == 1 {
		w = append(w, "spec.instances is 1: the cluster has no replicas")
	}
	if c.Spec.Backup == nil {
		w = append(w, "spec.backup is not set: no backups will be taken")
	}
	return w
}

func toError(c *pgv1.PostgresCluster, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(pgv1.GroupVersion.WithKind("PostgresCluster").GroupKind(), c.Name, errs)
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
