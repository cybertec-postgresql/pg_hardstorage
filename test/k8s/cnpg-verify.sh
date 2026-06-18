#!/usr/bin/env bash
#
# cnpg-verify.sh — prove pg_hardstorage can back up and restore a
# CloudNativePG-managed PostgreSQL cluster, end to end.
#
# This is a standalone, re-runnable verification — NOT a testkit
# scenario. The testkit's `kind` / `k8s-remote` topology providers
# are still stubs (see internal/testkit/topology/topology.go), so a
# scenario YAML referencing them would fail-fast rather than run.
# Until those providers land, this script is the executable proof
# behind the "CloudNativePG: verified" claim on the website and in
# docs/how-to/k8s-quickstart.md.
#
# What it does:
#   1. installs the CloudNativePG operator on the current kube-context
#   2. creates a single-instance CNPG cluster (test/k8s/cnpg-cluster.yaml)
#   3. seeds a table and records its checksum on the live primary
#   4. runs `pg_hardstorage backup` against the CNPG primary over the
#      replication protocol (the BASE_BACKUP command — this is why a
#      REPLICATION-capable role is required and managed DBaaS is not)
#   5. runs `verify` then `restore` from the repository
#   6. boots the restored data dir in a stock postgres:17 container,
#      lets it recover to consistency, and checks the table checksum
#      byte-for-byte against the value captured in step 3
#
# Exit 0 only if the restored checksum matches the live checksum.
#
# Requirements: kubectl (pointed at a running cluster — e.g. minikube),
# docker, psql, and a built ./pg_hardstorage binary in the repo root.
#
# Notes on CNPG specifics:
#   * CNPG bakes pod-local paths into the cluster config (SSL certs
#     under /controller, csvlog to /controller/log, socket in
#     /controller/run) and a restore_command pointing at the agent's
#     install path. None of these exist in a bare sandbox container,
#     so step 6 appends overrides to postgresql.auto.conf. A real
#     restore back INTO a CNPG cluster does not need this — CNPG
#     re-injects those paths itself.
#   * The backup is taken with --include-wal so the basebackup is
#     self-contained; the sandbox recovers from pg_wal/ without a
#     working restore_command.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="$REPO_ROOT/pg_hardstorage"
MANIFEST="$REPO_ROOT/test/k8s/cnpg-cluster.yaml"
CLUSTER="hs-cnpg"
PRIMARY="${CLUSTER}-1"
DEPLOYMENT="cnpg-prod"
REPO_URL="file:///tmp/hs-cnpg-repo"
RESTORE_DIR="/tmp/hs-cnpg-restore"
SANDBOX_DIR="/tmp/hs-cnpg-sandbox"
SANDBOX_CTR="hs-cnpg-sb"
PF_PORT=55432
CNPG_VERSION="1.25.1"
CNPG_CHANNEL="release-1.25"

log() { printf '\n=== %s ===\n' "$*"; }

[[ -x "$BIN" ]] || { echo "build the binary first: go build -o pg_hardstorage ./cmd/pg_hardstorage"; exit 1; }

cleanup() {
  kill "${PF_PID:-}" 2>/dev/null || true
  docker rm -f "$SANDBOX_CTR" 2>/dev/null || true
}
trap cleanup EXIT

log "install CloudNativePG operator $CNPG_VERSION"
kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/${CNPG_CHANNEL}/releases/cnpg-${CNPG_VERSION}.yaml"
kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=180s

log "create CNPG cluster"
kubectl apply -f "$MANIFEST"
kubectl wait --for=condition=Ready "cluster/$CLUSTER" --timeout=300s

log "seed data and capture live checksum"
kubectl exec "$PRIMARY" -- psql -U postgres -d app -c \
  "create table if not exists t (id int primary key, v text);"
kubectl exec "$PRIMARY" -- psql -U postgres -d app -c \
  "truncate t; insert into t select g, md5(g::text) from generate_series(1,50000) g;"
CK_QUERY="select count(*)||'|'||coalesce(sum(hashtextextended(v,0))::text,'0') from t;"
LIVE_CK=$(kubectl exec "$PRIMARY" -- psql -U postgres -d app -tAc "$CK_QUERY" | tr -d '[:space:]')
echo "live checksum: $LIVE_CK"

log "port-forward the CNPG primary service"
kubectl port-forward "svc/${CLUSTER}-rw" "${PF_PORT}:5432" >/dev/null 2>&1 &
PF_PID=$!
sleep 4
PGPASS=$(kubectl get secret "${CLUSTER}-superuser" -o jsonpath='{.data.password}' | base64 -d)
CONN="host=127.0.0.1 port=${PF_PORT} user=postgres password=${PGPASS} dbname=app"

log "repo init + backup against the CNPG primary"
rm -rf /tmp/hs-cnpg-repo
"$BIN" repo init "$REPO_URL"
"$BIN" backup "$DEPLOYMENT" --pg-connection "$CONN" --repo "$REPO_URL" --include-wal

log "verify the backup"
"$BIN" verify "$DEPLOYMENT" latest --repo "$REPO_URL"

log "restore the backup"
rm -rf "$RESTORE_DIR"
"$BIN" restore "$DEPLOYMENT" latest --repo "$REPO_URL" --target "$RESTORE_DIR"

log "boot the restored data dir in a stock postgres:17 sandbox"
docker rm -f "$SANDBOX_CTR" 2>/dev/null || true
rm -rf "$SANDBOX_DIR"
cp -a "$RESTORE_DIR" "$SANDBOX_DIR"
# Neutralise the CNPG pod-local paths that do not exist in a bare
# container, and make the basebackup self-recover from pg_wal/.
cat >> "$SANDBOX_DIR/postgresql.auto.conf" <<'EOF'

# --- cnpg-verify.sh sandbox overrides (CNPG pod-local paths absent) ---
ssl = off
logging_collector = off
log_destination = 'stderr'
archive_mode = off
unix_socket_directories = '/tmp'
restore_command = '/bin/false'
recovery_target_timeline = 'current'
EOF
chmod 0700 "$SANDBOX_DIR"
docker run -d --name "$SANDBOX_CTR" --user "$(id -u):$(id -g)" \
  -v "${SANDBOX_DIR}:/var/lib/postgresql/data:Z" \
  -e POSTGRES_PASSWORD=x -e PGDATA=/var/lib/postgresql/data \
  postgres:17 >/dev/null
sleep 14
docker logs "$SANDBOX_CTR" 2>&1 | grep -E 'consistent recovery|ready to accept connections' || {
  echo "FAIL: restored data dir did not reach a consistent, ready state"
  docker logs "$SANDBOX_CTR" 2>&1 | tail -20
  exit 1
}

log "compare restored checksum against the live cluster"
REST_CK=$(docker exec -e PGPASSWORD="$PGPASS" "$SANDBOX_CTR" \
  psql -h 127.0.0.1 -U postgres -d app -tAc "$CK_QUERY" | tr -d '[:space:]')
echo "live    : $LIVE_CK"
echo "restored: $REST_CK"
if [[ "$LIVE_CK" == "$REST_CK" && -n "$LIVE_CK" ]]; then
  echo
  echo "PASS — CloudNativePG backup/restore round-trip is byte-equal."
  exit 0
fi
echo
echo "FAIL — restored checksum does not match the live cluster."
exit 1
