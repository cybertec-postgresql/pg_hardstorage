#!/usr/bin/env bash
# cnpg_operator_backup_smoke.sh — drop-in coverage for the
# CloudNativePG path.  Mirrors the Zalando scenario but
# exercises the barman-cloud-* shim family.  Verifies that:
#
#   1. The CNPG operator brings up a Cluster CRD using
#      pg-hardstorage's cnpg-shim image (barman-cloud-* binaries
#      swapped for our compat shims).
#   2. CNPG's controller wires archive_command through our
#      barman-cloud-wal-archive shim, which translates argv
#      and dispatches to native `pg_hardstorage wal push` —
#      auto-init creates the repo on first invocation.
#   3. Real pg_hardstorage objects (HSREPO + chunks/sha256/...)
#      land in the in-cluster MinIO sink.
#
# Invoked by run_k8s_testing.sh.  Same env contract as the
# Zalando scenario.
#
# CAVEAT: CNPG 1.30+ removes native barman-cloud support in
# favour of a Plugin sidecar architecture.  This scenario
# pins a 1.29.x chart range; the broader plugin path is
# tracked separately.

set -euo pipefail

KUBECTX="${KUBECTX:?KUBECTX is required}"
SHIM_IMAGE="${SHIM_IMAGE:?SHIM_IMAGE is required}"
CELL_DIR="${CELL_DIR:?CELL_DIR is required}"

KCTL="kubectl --context $KUBECTX"
HELM="helm --kube-context $KUBECTX"

NAMESPACE=cnpg-smoke

note()  { printf '[cnpg-smoke] %s\n' "$*" >&2; }
fail()  { printf '[cnpg-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

# CNPG validates the image tag against a SemVer-shaped pattern
# (the tag must START with the PG major).  Build tag is
# `pg-hardstorage/cnpg-shim:pg17-<hash>`.  Re-tag with a
# CNPG-friendly variant: `17-<hash>` — keeps the hash so a
# new build forces minikube to refresh the cache, while
# satisfying CNPG's parser.
short_hash="${SHIM_IMAGE##*pg17-}"
short_hash="${short_hash%%[ /]*}"
CNPG_TAG="pg-hardstorage/cnpg-shim:17-${short_hash}"
note "re-tagging shim image for CNPG's tag validator: $CNPG_TAG"
docker tag "$SHIM_IMAGE" "$CNPG_TAG" >&2 2>&1 || true
minikube -p "$KUBECTX" image load "$CNPG_TAG" >&2 2>&1 || true
SHIM_IMAGE_CNPG="docker.io/$CNPG_TAG"

# ---- 1. Install CloudNativePG -----------------------------------
note "installing cloudnative-pg (cnpg-system namespace)"
$HELM repo add cnpg https://cloudnative-pg.github.io/charts \
    --force-update >&2 2>&1 || true
$HELM repo update >&2 2>&1
$HELM upgrade --install cnpg cnpg/cloudnative-pg \
    --namespace cnpg-system --create-namespace \
    --wait --timeout 5m >&2 2>&1

# ---- 1b. Force-clean any stale state from a prior failed run ----
# `kubectl apply` on a CNPG Cluster CRD whose underlying initdb
# pod is stuck (e.g. ErrImageNeverPull from a prior cell that
# preloaded a different image hash) does NOT make CNPG recreate
# the pod — the operator considers the cluster "in setup" and
# waits.  Deleting first guarantees a fresh reconcile loop on
# every scenario run, regardless of whether the minikube profile
# was reused from a previous failed cell.
#
# `--ignore-not-found --wait=true` is the safe shape: empty-state
# is success, populated-state blocks until finalizers complete.
note "force-clean any stale CNPG cluster + namespace from prior runs"
$KCTL delete cluster smoke -n "$NAMESPACE" --ignore-not-found --wait=true \
    --timeout 60s >&2 2>&1 || true
$KCTL delete namespace "$NAMESPACE" --ignore-not-found --wait=true \
    --timeout 60s >&2 2>&1 || true

# ---- 2. Apply MinIO + bucket bootstrap + cluster -----------------
note "applying scenario manifests"
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
              mc mb -p m/cnpg-backups || true
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: smoke
  namespace: $NAMESPACE
spec:
  instances: 2
  imageName: $SHIM_IMAGE_CNPG
  imagePullPolicy: Never
  storage:
    size: 1Gi
  bootstrap:
    initdb:
      database: app
      owner: app
  backup:
    barmanObjectStore:
      destinationPath: s3://cnpg-backups/smoke
      endpointURL: http://minio.$NAMESPACE.svc.cluster.local:9000
      s3Credentials:
        accessKeyId:
          name: minio-creds
          key: AWS_ACCESS_KEY_ID
        secretAccessKey:
          name: minio-creds
          key: AWS_SECRET_ACCESS_KEY
      wal:    { compression: gzip }
      data:   { compression: gzip }
EOF

# ---- 3. Wait for cluster healthy --------------------------------
note "waiting for cluster healthy state (max 5m)"
phase=""
for i in $(seq 1 60); do
    phase=$($KCTL -n "$NAMESPACE" get cluster smoke -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$phase" == "Cluster in healthy state" ]]; then break; fi
    sleep 5
done
[[ "$phase" == "Cluster in healthy state" ]] || fail "cluster did not reach healthy (last phase: $phase)"
note "cluster phase=$phase"

# ---- 4. Drive WAL archiving + force a few rotations --------------
note "forcing WAL switches via psql (multiple rotations to traverse auto-init + retries)"
for i in 1 2 3; do
    $KCTL -n "$NAMESPACE" exec smoke-1 -- psql -U postgres -c \
        "INSERT INTO IF NOT EXISTS dummy_for_wal_rotation (1); SELECT pg_switch_wal();" >&2 2>&1 || \
    $KCTL -n "$NAMESPACE" exec smoke-1 -- psql -U postgres -c \
        "CREATE TABLE IF NOT EXISTS smoke (i int); INSERT INTO smoke SELECT generate_series(1,1000); SELECT pg_switch_wal();" \
        >&2 2>&1 || true
    sleep 4
done

# ---- 5. Poll for archive_count > 0 with retry ---------------------
# CNPG's archive pipeline is async: the controller logs the first
# archive_command call, our shim returns notfound.repo, auto-init
# fires, then PG retries archive_command on the next archiver
# wakeup (~30s default).  Poll up to 90s rather than asserting on
# the first sample.
archived=0
for i in $(seq 1 18); do
    archived=$($KCTL -n "$NAMESPACE" exec smoke-1 -- psql -U postgres -tAc \
        'SELECT archived_count FROM pg_stat_archiver' 2>/dev/null | tr -d '[:space:]' || true)
    note "[$i/18] archived_count=$archived"
    [[ "${archived:-0}" -ge 1 ]] && break
    sleep 5
done
[[ "${archived:-0}" -ge 1 ]] || fail "archived_count=0 after 90s — archive_command never succeeded"

# ---- 6. Assert pg_hardstorage objects in MinIO -------------------
note "checking MinIO for pg_hardstorage artefacts"
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc alias set m \
    http://localhost:9000 minio minio12345 >/dev/null 2>&1
minio_listing=$($KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/cnpg-backups/ 2>/dev/null || true)
hsrepo_present=$(printf '%s\n' "$minio_listing" | grep -c HSREPO || true)
chunks_present=$(printf '%s\n' "$minio_listing" | grep -c "chunks/sha256/" || true)
note "MinIO objects: HSREPO=$hsrepo_present  chunks=$chunks_present"

[[ "${hsrepo_present:-0}" -ge 1 ]] || fail "no HSREPO file in MinIO — auto-init didn't run"
[[ "${chunks_present:-0}" -ge 1 ]] || fail "no chunks/sha256/* objects in MinIO — shim didn't write content"

# ---- 7. Persist artefacts ----------------------------------------
mkdir -p "$CELL_DIR"
$KCTL -n "$NAMESPACE" exec smoke-1 -- psql -U postgres -c \
    'SELECT * FROM pg_stat_archiver' > "$CELL_DIR/pg_stat_archiver.txt" 2>&1 || true
$KCTL -n "$NAMESPACE" exec deploy/minio -- mc ls -r m/cnpg-backups/ \
    > "$CELL_DIR/minio-listing.txt" 2>&1 || true

note "PASS"
exit 0
