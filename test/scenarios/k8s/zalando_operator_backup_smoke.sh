#!/usr/bin/env bash
# zalando_operator_backup_smoke.sh — drop-in coverage for the
# Zalando postgres-operator path.  Verifies that:
#
#   1. The Zalando operator brings up a Spilo cluster using
#      pg-hardstorage's spilo-shim image (wal-g binary swapped
#      for our compat shim).
#   2. PG's archive_command (which Spilo wires as
#      `envdir wal-g wal-push %p`) succeeds — meaning our
#      shim's auto-init kicked in on first push and translated
#      to a native pg_hardstorage WAL archive.
#   3. Real pg_hardstorage objects (HSREPO + chunks/sha256/...)
#      land in the in-cluster MinIO sink.
#
# Invoked by run_k8s_testing.sh.  Expected env:
#
#   KUBECTX        — minikube profile / kube context
#   SHIM_IMAGE     — local docker image tag for spilo-shim
#   CELL_DIR       — per-cell artefact dir (writes here)
#   TESTKIT_BIN    — pg_hardstorage_testkit binary
#
# Exit 0 on PASS, non-zero on FAIL.  All output goes to stderr
# (driver redirects to <CELL_DIR>/run.log).

set -euo pipefail

KUBECTX="${KUBECTX:?KUBECTX is required}"
SHIM_IMAGE="${SHIM_IMAGE:?SHIM_IMAGE is required}"
CELL_DIR="${CELL_DIR:?CELL_DIR is required}"

KCTL="kubectl --context $KUBECTX"
HELM="helm --kube-context $KUBECTX"

NAMESPACE=zalando-smoke

note()  { printf '[zalando-smoke] %s\n' "$*" >&2; }
fail()  { printf '[zalando-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

# ---- 1. Install Zalando postgres-operator ------------------------
note "installing Zalando postgres-operator (zalando-system namespace)"
$HELM repo add postgres-operator \
    https://opensource.zalando.com/postgres-operator/charts/postgres-operator \
    --force-update >&2 2>&1 || true
$HELM repo update >&2 2>&1
$HELM upgrade --install postgres-operator postgres-operator/postgres-operator \
    --namespace zalando-system --create-namespace \
    --wait --timeout 5m >&2 2>&1

# ---- 2. Apply MinIO + bucket bootstrap + cluster -----------------
note "applying scenario manifests"
cat <<EOF | $KCTL apply -f - >&2
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
---
apiVersion: v1
kind: Secret
metadata:
  name: minio-creds
  namespace: $NAMESPACE
type: Opaque
stringData:
  AWS_ACCESS_KEY_ID: minio
  AWS_SECRET_ACCESS_KEY: minio12345
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: $NAMESPACE
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: minio/minio:RELEASE.2024-12-18T13-15-44Z
          args: ["server", "/data", "--console-address", ":9001"]
          env:
            - { name: MINIO_ROOT_USER, valueFrom: { secretKeyRef: { name: minio-creds, key: AWS_ACCESS_KEY_ID } } }
            - { name: MINIO_ROOT_PASSWORD, valueFrom: { secretKeyRef: { name: minio-creds, key: AWS_SECRET_ACCESS_KEY } } }
          ports: [{ containerPort: 9000, name: api }]
          volumeMounts: [{ name: data, mountPath: /data }]
      volumes: [{ name: data, emptyDir: {} }]
---
apiVersion: v1
kind: Service
metadata: { name: minio, namespace: $NAMESPACE }
spec:
  selector: { app: minio }
  ports: [{ port: 9000, name: api }]
---
apiVersion: batch/v1
kind: Job
metadata: { name: minio-mkbucket, namespace: $NAMESPACE }
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: minio/mc:RELEASE.2024-11-21T17-21-54Z
          envFrom: [{ secretRef: { name: minio-creds } }]
          command:
            - sh
            - -c
            - |
              set -eux
              for i in \$(seq 1 60); do
                  if mc alias set m http://minio:9000 "\$AWS_ACCESS_KEY_ID" "\$AWS_SECRET_ACCESS_KEY"; then
                      break
                  fi
                  sleep 2
              done
              mc mb -p m/spilo-backups || true
---
apiVersion: v1
kind: ServiceAccount
metadata: { name: postgres-pod, namespace: $NAMESPACE }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: postgres-pod, namespace: $NAMESPACE }
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: postgres-pod
subjects:
  - { kind: ServiceAccount, name: postgres-pod, namespace: $NAMESPACE }
---
apiVersion: acid.zalan.do/v1
kind: postgresql
metadata:
  name: smoke
  namespace: $NAMESPACE
spec:
  teamId: smoke
  dockerImage: "$SHIM_IMAGE"
  numberOfInstances: 1
  postgresql: { version: "17" }
  volume: { size: 1Gi }
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits:   { cpu: "1",  memory: 1Gi }
  users: { app: [] }
  databases: { app: app }
  env:
    - { name: USE_WALG_BACKUP,        value: "true" }
    - { name: USE_WALG_RESTORE,       value: "true" }
    - { name: WAL_S3_BUCKET,          value: "spilo-backups" }
    - { name: AWS_ENDPOINT,           value: "http://minio.$NAMESPACE.svc.cluster.local:9000" }
    - { name: AWS_REGION,             value: "us-east-1" }
    - { name: AWS_S3_FORCE_PATH_STYLE, value: "true" }
    - { name: AWS_ACCESS_KEY_ID,      valueFrom: { secretKeyRef: { name: minio-creds, key: AWS_ACCESS_KEY_ID } } }
    - { name: AWS_SECRET_ACCESS_KEY,  valueFrom: { secretKeyRef: { name: minio-creds, key: AWS_SECRET_ACCESS_KEY } } }
EOF

# ---- 3. Wait for cluster ready -----------------------------------
note "waiting for cluster Running (max 5m)"
for i in $(seq 1 60); do
    status=$($KCTL -n "$NAMESPACE" get postgresql smoke -o jsonpath='{.status.PostgresClusterStatus}' 2>/dev/null || true)
    if [[ "$status" == "Running" ]]; then break; fi
    sleep 5
done
[[ "$status" == "Running" ]] || fail "cluster did not reach Running (last status: $status)"
note "cluster is Running"

# ---- 4. Drive WAL archiving --------------------------------------
note "forcing WAL switches via psql"
$KCTL -n "$NAMESPACE" exec smoke-0 -- su postgres -c \
    "psql -c 'CREATE TABLE smoke (i int); INSERT INTO smoke SELECT generate_series(1,1000); SELECT pg_switch_wal();'" \
    >&2 2>&1
sleep 8

# ---- 5. Assert archive_command succeeded -------------------------
archived=$($KCTL -n "$NAMESPACE" exec smoke-0 -- su postgres -c \
    "psql -tAc 'SELECT archived_count FROM pg_stat_archiver'" 2>/dev/null | tr -d '[:space:]')
note "pg_stat_archiver.archived_count = $archived"
[[ "${archived:-0}" -ge 1 ]] || fail "archived_count=0 — archive_command never succeeded"

# ---- 6. Assert pg_hardstorage objects landed in MinIO ------------
# `mc ls` runs inside the MinIO container; the filtering grep
# runs on the HOST (the MinIO image is busybox-minimal and
# ships no grep, so a `sh -c 'mc ls | grep ...'` inside the
# container silently exits 127 and fooled the original draft
# of this script into reporting "no HSREPO" while the file was
# right there).
note "checking MinIO for pg_hardstorage artefacts"
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc alias set m \
    http://localhost:9000 minio minio12345 >/dev/null 2>&1
minio_listing=$($KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/spilo-backups/ 2>/dev/null || true)
hsrepo_present=$(printf '%s\n' "$minio_listing" | grep -c HSREPO || true)
chunks_present=$(printf '%s\n' "$minio_listing" | grep -c "chunks/sha256/" || true)
note "MinIO objects: HSREPO=$hsrepo_present  chunks=$chunks_present"

[[ "${hsrepo_present:-0}" -ge 1 ]] || fail "no HSREPO file in MinIO — auto-init didn't run"
[[ "${chunks_present:-0}" -ge 1 ]] || fail "no chunks/sha256/* objects in MinIO — shim didn't write content"

# ---- 7. Persist findings to the cell artefact dir ----------------
mkdir -p "$CELL_DIR"
$KCTL -n "$NAMESPACE" exec smoke-0 -- su postgres -c \
    "psql -c 'SELECT * FROM pg_stat_archiver'" > "$CELL_DIR/pg_stat_archiver.txt" 2>&1 || true
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/spilo-backups/ \
    > "$CELL_DIR/minio-listing.txt" 2>&1 || true

note "PASS"
exit 0
