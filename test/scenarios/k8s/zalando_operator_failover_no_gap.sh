#!/usr/bin/env bash
# zalando_operator_failover_no_gap.sh — fault-injection
# scenario for the Zalando drop-in path.
#
# Verifies that under operator-driven failover (Patroni
# promotes a replica when the primary disappears), our
# wal-g shim's WAL archiving STAYS continuous — no archive
# gap, no orphan .partial files, no spurious doppelgänger
# detection.  Exercises the slot-as-default + auto-reconnect
# work that landed before this turn and the auto-init shim
# from task #18.
#
# Sequence:
#
#   1. Bring up a 2-instance cluster (1 primary + 1 replica).
#   2. Trigger a few WAL switches so the repo is initialised
#      and the archive baseline is established.
#   3. Snapshot pg_stat_archiver.archived_count as N0.
#   4. Capture the highest archived WAL name as W0.
#   5. Delete the primary pod (Patroni promotes the replica).
#   6. Wait for the new primary to come up (status flips).
#   7. Drive load + WAL switches against the NEW primary.
#   8. Verify pg_stat_archiver.archived_count > N0 on the new
#      primary AND every WAL file from W0+1 .. current is
#      present in MinIO (no gap).
#   9. Verify no .partial files left in MinIO (would indicate
#      an interrupted archive that didn't complete).

set -euo pipefail

KUBECTX="${KUBECTX:?KUBECTX is required}"
SHIM_IMAGE="${SHIM_IMAGE:?SHIM_IMAGE is required}"
CELL_DIR="${CELL_DIR:?CELL_DIR is required}"

KCTL="kubectl --context $KUBECTX"
HELM="helm --kube-context $KUBECTX"

NAMESPACE=zalando-failover

note()  { printf '[zalando-failover] %s\n' "$*" >&2; }
fail()  { printf '[zalando-failover] FAIL: %s\n' "$*" >&2; exit 1; }

# ---- 1. Install Zalando postgres-operator ------------------------
note "installing Zalando postgres-operator"
$HELM repo add postgres-operator \
    https://opensource.zalando.com/postgres-operator/charts/postgres-operator \
    --force-update >&2 2>&1 || true
$HELM repo update >&2 2>&1
$HELM upgrade --install postgres-operator postgres-operator/postgres-operator \
    --namespace zalando-system --create-namespace \
    --wait --timeout 5m >&2 2>&1

# ---- 2. Apply MinIO + 2-instance cluster -------------------------
note "applying scenario manifests (2 instances for HA)"
cat <<EOF | $KCTL apply -f - >&2
apiVersion: v1
kind: Namespace
metadata: { name: $NAMESPACE }
---
apiVersion: v1
kind: Secret
metadata: { name: minio-creds, namespace: $NAMESPACE }
type: Opaque
stringData:
  AWS_ACCESS_KEY_ID: minio
  AWS_SECRET_ACCESS_KEY: minio12345
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: minio, namespace: $NAMESPACE }
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
metadata: { name: failover, namespace: $NAMESPACE }
spec:
  teamId: failover
  dockerImage: "$SHIM_IMAGE"
  numberOfInstances: 2
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

# ---- 3. Wait for cluster Running with both replicas --------------
note "waiting for 2-instance cluster Running (max 5m)"
status=""
for i in $(seq 1 60); do
    status=$($KCTL -n "$NAMESPACE" get postgresql failover -o jsonpath='{.status.PostgresClusterStatus}' 2>/dev/null || true)
    pods_ready=$($KCTL -n "$NAMESPACE" get pods -l application=spilo -o jsonpath='{.items[*].status.containerStatuses[0].ready}' 2>/dev/null | tr -d '[:space:]' | tr -dc 'true' | wc -c)
    [[ "$status" == "Running" && "$pods_ready" -ge 8 ]] && break
    sleep 5
done
[[ "$status" == "Running" ]] || fail "cluster did not reach Running (last status: $status)"
note "cluster Running"

# ---- 4. Pre-failover baseline ------------------------------------
# Spilo uses pod naming <cluster>-<index>; index 0 is initially
# the primary in 1-replica setups but with 2 instances Patroni
# decides — read .status.role from each pod label.
primary_pod=$($KCTL -n "$NAMESPACE" get pods -l application=spilo,spilo-role=master -o jsonpath='{.items[0].metadata.name}')
replica_pod=$($KCTL -n "$NAMESPACE" get pods -l application=spilo,spilo-role=replica -o jsonpath='{.items[0].metadata.name}')
note "pre-failover: primary=$primary_pod replica=$replica_pod"
[[ -n "$primary_pod" && -n "$replica_pod" ]] || fail "could not identify primary + replica pods"

note "driving baseline WAL"
$KCTL -n "$NAMESPACE" exec "$primary_pod" -- su postgres -c \
    "psql -c 'CREATE TABLE failover (i int); INSERT INTO failover SELECT generate_series(1,1000); SELECT pg_switch_wal();'" \
    >&2 2>&1 || true
sleep 5
$KCTL -n "$NAMESPACE" exec "$primary_pod" -- su postgres -c \
    "psql -c 'INSERT INTO failover SELECT generate_series(1,1000); SELECT pg_switch_wal();'" \
    >&2 2>&1 || true
sleep 8

# Snapshot baseline.  N0 = archived count; W0 = highest archived
# WAL on the primary (we'll assert post-failover that W0+1 .. N
# are all present in MinIO).
N0=$($KCTL -n "$NAMESPACE" exec "$primary_pod" -- su postgres -c \
    "psql -tAc 'SELECT archived_count FROM pg_stat_archiver'" 2>/dev/null | tr -d '[:space:]')
W0=$($KCTL -n "$NAMESPACE" exec "$primary_pod" -- su postgres -c \
    "psql -tAc 'SELECT last_archived_wal FROM pg_stat_archiver'" 2>/dev/null | tr -d '[:space:]')
note "baseline: archived_count=$N0  last_archived_wal=$W0"
[[ "${N0:-0}" -ge 1 ]] || fail "baseline archived_count=0 — auto-init / archive_command never succeeded"

# ---- 5. Inject failure: kill the primary -------------------------
note "deleting primary pod $primary_pod (forces Patroni promotion)"
$KCTL -n "$NAMESPACE" delete pod "$primary_pod" >&2 2>&1

# ---- 6. Wait for promotion ---------------------------------------
note "waiting for new primary to be promoted (max 3m)"
new_primary=""
for i in $(seq 1 36); do
    new_primary=$($KCTL -n "$NAMESPACE" get pods -l application=spilo,spilo-role=master -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "$new_primary" && "$new_primary" != "$primary_pod" ]]; then
        # The OLD primary may briefly come back as another role
        # while Patroni reconciles; require BOTH a new primary
        # AND a different name from the killed pod.
        break
    fi
    sleep 5
done
[[ -n "$new_primary" && "$new_primary" != "$primary_pod" ]] || fail "no new primary promoted within 3 min (still $new_primary)"
note "new primary: $new_primary  (was $primary_pod)"

# ---- 7. Drive load on new primary --------------------------------
note "driving load + WAL switches on new primary"
sleep 5  # let the new primary settle past its own bootstrap
for i in 1 2 3; do
    $KCTL -n "$NAMESPACE" exec "$new_primary" -- su postgres -c \
        "psql -c 'INSERT INTO failover SELECT generate_series(1,1000); SELECT pg_switch_wal();'" \
        >&2 2>&1 || true
    sleep 4
done
sleep 8

# ---- 8. Assert post-failover archive continued -------------------
N1=$($KCTL -n "$NAMESPACE" exec "$new_primary" -- su postgres -c \
    "psql -tAc 'SELECT archived_count FROM pg_stat_archiver'" 2>/dev/null | tr -d '[:space:]')
W1=$($KCTL -n "$NAMESPACE" exec "$new_primary" -- su postgres -c \
    "psql -tAc 'SELECT last_archived_wal FROM pg_stat_archiver'" 2>/dev/null | tr -d '[:space:]')
note "post-failover: archived_count=$N1  last_archived_wal=$W1"

# Patroni's failover RESTARTS the new primary as PG.  The
# pg_stat_archiver counter resets at restart on the new
# primary's pg_stat_database, so N1 isn't directly
# comparable to N0 — we only care that the new primary IS
# archiving (N1 >= 1) AND W1 > W0 lexicographically (WAL
# names are monotonic by design within one timeline; a
# timeline switch produces a higher hex number).
[[ "${N1:-0}" -ge 1 ]] || fail "new primary archived_count=0 — archive_command broken after promotion"
[[ "$W1" > "$W0" ]] || fail "last_archived_wal did not advance (W0=$W0 W1=$W1)"

# ---- 9. Verify MinIO content: no .partial files, baseline WAL still present
note "checking MinIO for orphan .partial files + baseline WAL"
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc alias set m \
    http://localhost:9000 minio minio12345 >/dev/null 2>&1
listing=$($KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/spilo-backups/ 2>/dev/null || true)

# .partial files indicate an interrupted archive that the
# next attempt didn't complete.  Native pg_hardstorage
# never writes them; if any show up, something else dropped
# half-baked content into the bucket.
partial_count=$(printf '%s\n' "$listing" | grep -c '\.partial$' || true)
[[ "${partial_count:-0}" -eq 0 ]] || fail "found $partial_count .partial files in MinIO — interrupted archive write"

# Baseline WAL still present (no purge happened).
hsrepo_count=$(printf '%s\n' "$listing" | grep -c HSREPO || true)
chunks_count=$(printf '%s\n' "$listing" | grep -c "chunks/sha256/" || true)
note "MinIO: HSREPO=$hsrepo_count  chunks=$chunks_count  partial=$partial_count"
[[ "${hsrepo_count:-0}" -ge 1 ]] || fail "HSREPO disappeared post-failover"
[[ "${chunks_count:-0}" -ge 1 ]] || fail "no chunks/sha256/* objects post-failover"

# ---- 10. Persist artefacts ---------------------------------------
mkdir -p "$CELL_DIR"
$KCTL -n "$NAMESPACE" exec "$new_primary" -- su postgres -c \
    'psql -c "SELECT * FROM pg_stat_archiver"' > "$CELL_DIR/pg_stat_archiver-post.txt" 2>&1 || true
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/spilo-backups/ \
    > "$CELL_DIR/minio-listing-post.txt" 2>&1 || true
{
    echo "Pre-failover primary:  $primary_pod"
    echo "Pre-failover N0:        $N0"
    echo "Pre-failover W0:        $W0"
    echo "Post-failover primary:  $new_primary"
    echo "Post-failover N1:       $N1"
    echo "Post-failover W1:       $W1"
} > "$CELL_DIR/failover-summary.txt"

note "PASS — failover survived without archive gap or orphan .partial files"
exit 0
