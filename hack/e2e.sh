#!/usr/bin/env bash
# End-to-end test: installs cert-manager, MinIO and the operator into the current
# kube context, then checks replication, backup and restore.
set -euo pipefail

IMG=${IMG:-pgkeeper:e2e}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.17.2}
OPERATOR_NS=pgkeeper-system

cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; dump; exit 1; }

dump() {
  step "Debug output"
  kubectl get pgc,pgb,sts,pods,jobs,pvc -o wide || true
  kubectl describe pgc || true
  kubectl get events --sort-by=.lastTimestamp | tail -n 40 || true
  kubectl -n "$OPERATOR_NS" logs deploy/pgkeeper --tail=200 || true
  for p in $(kubectl get pods -o name); do
    echo "--- logs $p"
    kubectl logs "$p" --all-containers --tail=50 || true
  done
}
trap 'dump' ERR

# retry N CMD... runs CMD until it succeeds or N attempts are used.
retry() {
  local n=$1; shift
  for i in $(seq 1 "$n"); do
    if "$@"; then return 0; fi
    sleep 5
  done
  return 1
}

psql_in() {
  local pod=$1 db=$2 sql=$3
  kubectl exec "$pod" -c postgres -- psql -U postgres -d "$db" -tAc "$sql"
}

step "Installing cert-manager ${CERT_MANAGER_VERSION}"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
kubectl -n cert-manager wait deploy --all --for=condition=Available --timeout=300s

step "Installing MinIO"
kubectl apply -f hack/minio.yaml
kubectl -n minio rollout status deploy/minio --timeout=300s
kubectl -n minio wait job/create-bucket --for=condition=Complete --timeout=300s

step "Installing the operator (${IMG})"
helm upgrade --install pgkeeper charts/pgkeeper \
  --namespace "$OPERATOR_NS" --create-namespace \
  --set image.repository="${IMG%:*}" \
  --set image.tag="${IMG##*:}" \
  --set image.pullPolicy=Never \
  --wait --timeout 5m

step "Webhook rejects an invalid schedule"
invalid() {
  cat <<EOF | kubectl apply --dry-run=server -f - 2>&1
apiVersion: postgres.pgkeeper.dev/v1alpha1
kind: PostgresCluster
metadata:
  name: invalid
spec:
  storage: {size: 1Gi}
  backup:
    schedule: "every day"
    s3:
      endpoint: http://minio.minio.svc:9000
      bucket: pg-backups
      credentialsSecret: {name: s3-credentials}
EOF
}
check_rejected() { invalid | grep -q "spec.backup.schedule"; }
retry 30 check_rejected || fail "webhook did not reject the invalid cluster"

step "Creating cluster demo (2 instances)"
kubectl apply -f config/samples/s3-credentials.yaml
retry 30 kubectl apply -f config/samples/postgres_v1alpha1_postgrescluster.yaml
kubectl wait pgc/demo --for=jsonpath='{.status.phase}'=Ready --timeout=600s
kubectl get pgc demo

step "Writing data on the primary"
psql_in demo-0 shop "CREATE TABLE orders (id int PRIMARY KEY, item text); INSERT INTO orders VALUES (1, 'keyboard'), (2, 'mouse');"

step "Replica is in recovery and receives the data"
[ "$(psql_in demo-1 postgres 'SELECT pg_is_in_recovery()')" = "t" ] || fail "demo-1 is not a replica"
replica_has_rows() { [ "$(psql_in demo-1 shop 'SELECT count(*) FROM orders' 2>/dev/null)" = "2" ]; }
retry 24 replica_has_rows || fail "replica did not receive rows"

step "Application credentials work"
uri=$(kubectl get secret demo-app -o jsonpath='{.data.uri}' | base64 -d)
kubectl exec demo-0 -c postgres -- psql "$uri" -tAc "SELECT current_user" | grep -qx shop || fail "app login failed"

step "Taking a backup"
kubectl apply -f config/samples/postgres_v1alpha1_postgresbackup.yaml
kubectl wait pgb/demo-manual --for=jsonpath='{.status.phase}'=Completed --timeout=300s
key=$(kubectl get pgb demo-manual -o jsonpath='{.status.objectKey}')
echo "backup object: $key"
[ "$(kubectl get pgc demo -o jsonpath='{.status.lastSuccessfulBackup}')" = "$key" ] || fail "cluster status not updated"

step "Restoring into a new cluster"
sed "s#objectKey: .*#objectKey: ${key}#" config/samples/postgres_v1alpha1_restore.yaml | kubectl apply -f -
kubectl wait pgc/demo-restored --for=condition=Restored --timeout=600s
kubectl wait pgc/demo-restored --for=jsonpath='{.status.phase}'=Ready --timeout=300s
[ "$(psql_in demo-restored-0 shop 'SELECT count(*) FROM orders')" = "2" ] || fail "restored cluster is missing rows"

step "Scaling demo to 3 instances"
kubectl patch pgc demo --type merge -p '{"spec":{"instances":3}}'
kubectl wait pgc/demo --for=jsonpath='{.status.readyInstances}'=3 --timeout=600s
retry 24 bash -c "[ \"\$(kubectl exec demo-2 -c postgres -- psql -U postgres -d shop -tAc 'SELECT count(*) FROM orders')\" = 2 ]" \
  || fail "new replica did not receive rows"

step "Operator metrics"
metrics=$(kubectl run metrics-probe --rm -i --restart=Never --quiet --image=curlimages/curl:8.13.0 -- \
  curl -s "http://pgkeeper-metrics.${OPERATOR_NS}.svc:8080/metrics")
echo "$metrics" | grep '^pgkeeper_' | sort | head -n 20
echo "$metrics" | grep -q 'pgkeeper_backups_total{.*result="success"' || fail "backup metric missing"

step "All e2e checks passed"
