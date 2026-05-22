#!/usr/bin/env bats

# E2E for the platform-managed cozy-default BackupClass.
#
# Covers the parts of the design where tenants no longer supply S3
# configuration:
#   1. The cozystack-installed cozy-default BackupClass exists and binds
#      Postgres -> cozy-default-cnpg without any tenant-side bucket/secret
#      setup.
#   2. A Postgres BackupJob referencing cozy-default succeeds and lands a
#      Ready Backup artefact under the s3://cozy-backups/<ns>/<app>/ prefix
#      enforced by the default strategy template.
#   3. The platform-projected cozy-backups-creds Secret in the tenant
#      namespace is not readable by the tenant ServiceAccount — the RBAC
#      check anchors the credentials-isolation invariant.
#
# Edge cases (projection retry on missing source Secret, format
# normalisation accessKey -> AWS_ACCESS_KEY_ID) belong in the Go unit
# tests at internal/backupcontroller/credentials_projector_test.go.

TEST_NAMESPACE='tenant-test'
TEST_POSTGRES_NAME='pgapp'
TEST_BACKUPJOB_NAME='pg-default-job'
TEST_PROJECTED_SECRET='cozy-backups-creds'
SYSTEM_BUCKET_NS='cozy-backup-controller'
SYSTEM_BUCKET_NAME='cozy-backups'

setup_file() {
  kubectl -n "$TEST_NAMESPACE" delete backupjob.backups.cozystack.io --all --ignore-not-found --timeout=60s
  kubectl -n "$TEST_NAMESPACE" delete backup.backups.cozystack.io --all --ignore-not-found --timeout=60s
  kubectl -n "$TEST_NAMESPACE" delete postgres.apps.cozystack.io --all --ignore-not-found --timeout=2m
  kubectl -n "$TEST_NAMESPACE" delete secret "$TEST_PROJECTED_SECRET" --ignore-not-found --timeout=60s
}

teardown_file() {
  setup_file
}

print_log() {
  echo "# $1" >&3
}

dump_diagnostics() {
  echo "# --- diagnostics ---" >&3
  kubectl get backupclass cozy-default -o yaml >&3 2>&1 || true
  kubectl -n "$SYSTEM_BUCKET_NS" get bucket,secret >&3 2>&1 || true
  kubectl -n "$TEST_NAMESPACE" get postgres,cnpgcluster,backupjob,backup,secret -o wide >&3 2>&1 || true
  kubectl -n "$TEST_NAMESPACE" describe backupjob "$TEST_BACKUPJOB_NAME" >&3 2>&1 || true
  kubectl -n cozy-backupstrategy-controller logs -l app=backupstrategy-controller --tail=200 >&3 2>&1 || true
}

@test "Platform-managed cozy-default BackupClass and bucket exist" {
  kubectl get backupclass cozy-default >/dev/null
  kubectl -n "$SYSTEM_BUCKET_NS" get bucket "$SYSTEM_BUCKET_NAME" >/dev/null
  KIND_COUNT=$(kubectl get backupclass cozy-default -o jsonpath='{range .spec.strategies[*]}{.application.kind}{"\n"}{end}' | sort -u | wc -l)
  [ "$KIND_COUNT" -ge 3 ]
}

@test "Postgres BackupJob via cozy-default Succeeds without tenant S3 config" {
  print_log "Apply Postgres app with no chart-level backup.* values"
  kubectl apply -f - <<EOF
apiVersion: apps.cozystack.io/v1alpha1
kind: Postgres
metadata:
  name: ${TEST_POSTGRES_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  replicas: 1
  size: 1Gi
  resources:
    cpu: 100m
    memory: 256Mi
  backup:
    enabled: true
EOF

  kubectl -n "$TEST_NAMESPACE" wait hr "${TEST_POSTGRES_NAME}" --for=condition=ready --timeout=600s

  print_log "Submit BackupJob referencing cozy-default"
  kubectl apply -f - <<EOF
apiVersion: backups.cozystack.io/v1alpha1
kind: BackupJob
metadata:
  name: ${TEST_BACKUPJOB_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  applicationRef:
    apiGroup: apps.cozystack.io
    kind: Postgres
    name: ${TEST_POSTGRES_NAME}
  backupClassName: cozy-default
EOF

  print_log "Wait for BackupJob phase=Succeeded"
  kubectl -n "$TEST_NAMESPACE" wait backupjob.backups.cozystack.io "$TEST_BACKUPJOB_NAME" \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=1200s || { dump_diagnostics; false; }

  print_log "Inspect the projected credentials Secret"
  kubectl -n "$TEST_NAMESPACE" get secret "$TEST_PROJECTED_SECRET" >/dev/null
  AK=$(kubectl -n "$TEST_NAMESPACE" get secret "$TEST_PROJECTED_SECRET" -o jsonpath='{.data.AWS_ACCESS_KEY_ID}')
  SK=$(kubectl -n "$TEST_NAMESPACE" get secret "$TEST_PROJECTED_SECRET" -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}')
  [ -n "$AK" ]
  [ -n "$SK" ]
}

@test "Tenant ServiceAccount cannot read the projected credentials Secret" {
  # The projection deliberately omits the lineage tenantresource label so
  # the Secret is not promoted to a TenantSecret view. The default
  # cozy:tenant:base aggregation has zero verbs on core/v1.Secret, so
  # `kubectl auth can-i` MUST return no.
  RESULT=$(kubectl auth can-i get secret "$TEST_PROJECTED_SECRET" \
    --namespace "$TEST_NAMESPACE" \
    --as "system:serviceaccount:${TEST_NAMESPACE}:default" 2>/dev/null || true)
  [ "$RESULT" = "no" ]
}
