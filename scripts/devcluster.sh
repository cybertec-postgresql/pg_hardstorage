#!/usr/bin/env bash
# devcluster.sh — boot a throwaway PostgreSQL + pg_hardstorage repo
# entirely on the local Docker daemon. Useful for sanity-checking a
# build, walking through the getting-started guide without touching
# a real DB, or running a quick demo.
#
# Up:    ./scripts/devcluster.sh up
# Status: ./scripts/devcluster.sh status
# Down:  ./scripts/devcluster.sh down
# Reset: ./scripts/devcluster.sh reset   # down + remove volumes
#
# What it brings up:
#   - A PostgreSQL container on host port 55432 (so it never collides
#     with a system PG on 5432).
#   - A bind-mounted directory at ~/.pg_hardstorage-devcluster/ for
#     the file:// repo + keyring.
#   - One pg_hardstorage agent process (run on the host) is the
#     intended driver — the script doesn't run the agent for you, it
#     prints the exact command to copy-paste.
#
# Stable across runs: same PG password, same port, same repo URL —
# scripts pinned against this devcluster won't break between runs.

set -euo pipefail

# ----- configuration --------------------------------------------------

PG_HARDSTORAGE_DEV_CONTAINER="${PG_HARDSTORAGE_DEV_CONTAINER:-pg_hardstorage-devcluster}"
PG_HARDSTORAGE_DEV_IMAGE="${PG_HARDSTORAGE_DEV_IMAGE:-postgres:17}"
PG_HARDSTORAGE_DEV_PORT="${PG_HARDSTORAGE_DEV_PORT:-55432}"
PG_HARDSTORAGE_DEV_PASSWORD="${PG_HARDSTORAGE_DEV_PASSWORD:-pgbackup-dev}"
PG_HARDSTORAGE_DEV_DATA_DIR="${PG_HARDSTORAGE_DEV_DATA_DIR:-${HOME}/.pg_hardstorage-devcluster}"
PG_HARDSTORAGE_DEV_REPO_URL="file://${PG_HARDSTORAGE_DEV_DATA_DIR}/repo"

# ----- helpers --------------------------------------------------------

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: $1 not found in PATH" >&2
        echo "       (devcluster.sh needs $1 to function)" >&2
        exit 1
    fi
}

container_running() {
    [ -n "$(docker ps --filter "name=^${PG_HARDSTORAGE_DEV_CONTAINER}$" --format '{{.ID}}' 2>/dev/null)" ]
}

container_exists() {
    [ -n "$(docker ps -a --filter "name=^${PG_HARDSTORAGE_DEV_CONTAINER}$" --format '{{.ID}}' 2>/dev/null)" ]
}

binary() {
    if [ -x "./bin/pg_hardstorage" ]; then
        echo "./bin/pg_hardstorage"
    elif command -v pg_hardstorage >/dev/null 2>&1; then
        command -v pg_hardstorage
    else
        echo "pg_hardstorage"   # let the user discover the missing binary
    fi
}

# ----- subcommands ----------------------------------------------------

cmd_up() {
    require docker
    mkdir -p "${PG_HARDSTORAGE_DEV_DATA_DIR}/repo"

    if container_running; then
        echo "✓ ${PG_HARDSTORAGE_DEV_CONTAINER} already running"
    elif container_exists; then
        echo "→ starting existing ${PG_HARDSTORAGE_DEV_CONTAINER}"
        docker start "${PG_HARDSTORAGE_DEV_CONTAINER}" >/dev/null
    else
        echo "→ creating ${PG_HARDSTORAGE_DEV_CONTAINER} (${PG_HARDSTORAGE_DEV_IMAGE} on :${PG_HARDSTORAGE_DEV_PORT})"
        docker run -d --rm=false \
            --name "${PG_HARDSTORAGE_DEV_CONTAINER}" \
            -p "${PG_HARDSTORAGE_DEV_PORT}:5432" \
            -e POSTGRES_USER=pgbackup \
            -e POSTGRES_PASSWORD="${PG_HARDSTORAGE_DEV_PASSWORD}" \
            -e POSTGRES_DB=postgres \
            -e POSTGRES_INITDB_ARGS="--data-checksums" \
            "${PG_HARDSTORAGE_DEV_IMAGE}" \
            -c wal_level=replica \
            -c max_wal_senders=10 \
            -c max_replication_slots=10 \
            >/dev/null
    fi

    # Wait for PG to accept connections.
    echo "→ waiting for PostgreSQL"
    for i in $(seq 1 30); do
        if docker exec "${PG_HARDSTORAGE_DEV_CONTAINER}" pg_isready -U pgbackup -d postgres >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done

    # Grant the REPLICATION attribute to the bootstrap role so wal stream
    # works without a separate role-creation step. POSTGRES_USER is
    # superuser by default but we make REPLICATION explicit so the
    # devcluster matches the production runbook.
    docker exec -u postgres "${PG_HARDSTORAGE_DEV_CONTAINER}" \
        psql -U pgbackup -d postgres -c \
        "ALTER ROLE pgbackup REPLICATION;" >/dev/null

    cat <<EOF

✓ devcluster up

  PG container:    ${PG_HARDSTORAGE_DEV_CONTAINER}
  PG host:port:    127.0.0.1:${PG_HARDSTORAGE_DEV_PORT}
  PG DSN:          postgres://pgbackup:${PG_HARDSTORAGE_DEV_PASSWORD}@127.0.0.1:${PG_HARDSTORAGE_DEV_PORT}/postgres?sslmode=disable
  Repo URL:        ${PG_HARDSTORAGE_DEV_REPO_URL}
  Data dir:        ${PG_HARDSTORAGE_DEV_DATA_DIR}/

Try a round trip:

  $(binary) repo init ${PG_HARDSTORAGE_DEV_REPO_URL}
  $(binary) doctor                                                         # the read-only state check
  $(binary) wal stream demo \\
      --pg-connection 'postgres://pgbackup:${PG_HARDSTORAGE_DEV_PASSWORD}@127.0.0.1:${PG_HARDSTORAGE_DEV_PORT}/postgres?sslmode=disable' \\
      --repo ${PG_HARDSTORAGE_DEV_REPO_URL} \\
      --once                                                               # exits after first segment commits

Tear down with:

  $0 down
EOF
}

cmd_down() {
    if container_running; then
        echo "→ stopping ${PG_HARDSTORAGE_DEV_CONTAINER}"
        docker stop "${PG_HARDSTORAGE_DEV_CONTAINER}" >/dev/null
    fi
    if container_exists; then
        echo "→ removing ${PG_HARDSTORAGE_DEV_CONTAINER}"
        docker rm -f "${PG_HARDSTORAGE_DEV_CONTAINER}" >/dev/null 2>&1 || true
    fi
    echo "✓ devcluster down"
    echo "  (data dir ${PG_HARDSTORAGE_DEV_DATA_DIR}/ kept; \`devcluster.sh reset\` removes it)"
}

cmd_reset() {
    cmd_down
    if [ -d "${PG_HARDSTORAGE_DEV_DATA_DIR}" ]; then
        echo "→ removing ${PG_HARDSTORAGE_DEV_DATA_DIR}"
        rm -rf "${PG_HARDSTORAGE_DEV_DATA_DIR}"
    fi
    echo "✓ devcluster reset"
}

cmd_status() {
    if container_running; then
        echo "✓ ${PG_HARDSTORAGE_DEV_CONTAINER} running on :${PG_HARDSTORAGE_DEV_PORT}"
    elif container_exists; then
        echo "○ ${PG_HARDSTORAGE_DEV_CONTAINER} stopped"
    else
        echo "× ${PG_HARDSTORAGE_DEV_CONTAINER} does not exist (run \`devcluster.sh up\`)"
    fi
    if [ -d "${PG_HARDSTORAGE_DEV_DATA_DIR}/repo" ]; then
        echo "  data dir: ${PG_HARDSTORAGE_DEV_DATA_DIR}/"
        if [ -f "${PG_HARDSTORAGE_DEV_DATA_DIR}/repo/HSREPO" ]; then
            echo "  repo:     initialised"
        else
            echo "  repo:     not initialised (run \`pg_hardstorage repo init ${PG_HARDSTORAGE_DEV_REPO_URL}\`)"
        fi
    fi
}

cmd_psql() {
    require docker
    if ! container_running; then
        echo "× devcluster not running; \`devcluster.sh up\` first" >&2
        exit 1
    fi
    docker exec -it -u postgres "${PG_HARDSTORAGE_DEV_CONTAINER}" \
        psql -U pgbackup -d postgres "$@"
}

cmd_help() {
    cat <<'EOF'
devcluster.sh — throwaway PG + repo for development

Usage:
    ./scripts/devcluster.sh up           # boot PG + repo dir, print connection info
    ./scripts/devcluster.sh status       # is the container running? is the repo initialised?
    ./scripts/devcluster.sh psql -c '...'  # shell into PG via container psql
    ./scripts/devcluster.sh down         # stop the container, keep data
    ./scripts/devcluster.sh reset        # stop + remove data dir

Configuration via environment variables (defaults shown):

    PG_HARDSTORAGE_DEV_CONTAINER  pg_hardstorage-devcluster
    PG_HARDSTORAGE_DEV_IMAGE      postgres:17
    PG_HARDSTORAGE_DEV_PORT       55432
    PG_HARDSTORAGE_DEV_PASSWORD   pgbackup-dev
    PG_HARDSTORAGE_DEV_DATA_DIR   ${HOME}/.pg_hardstorage-devcluster
EOF
}

# ----- dispatch -------------------------------------------------------

case "${1:-help}" in
    up)     cmd_up ;;
    down)   cmd_down ;;
    reset)  cmd_reset ;;
    status) cmd_status ;;
    psql)   shift; cmd_psql "$@" ;;
    help|-h|--help) cmd_help ;;
    *)
        echo "unknown subcommand: $1" >&2
        echo "" >&2
        cmd_help >&2
        exit 2
        ;;
esac
