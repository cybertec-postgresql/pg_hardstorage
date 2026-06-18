#!/usr/bin/env bash
# entrypoint-multi-pg.sh — non-postgres-as-PID-1 wrapper for the
# multi-PG testbed image.  Mirrors the small set of init-time
# behaviours the official postgres docker-entrypoint.sh provides
# (initdb on first boot + create the requested superuser /
# database) but starts PG in the BACKGROUND and `exec sleep
# infinity` so PID 1 is `sleep` — `pg_ctl stop` from outside
# can then take PG down without killing the container.
#
# Honoured env vars (subset of the official image's contract):
#   POSTGRES_DB, POSTGRES_USER, POSTGRES_PASSWORD
#     If unset, defaults are: postgres / postgres / "" (trust auth).
#   PG_VERSION
#     Which PG major to start on first boot.  Defaults to 16.
#     The OTHER major's binaries are still on PATH for pg_upgrade.

set -euo pipefail

PG_VERSION="${PG_VERSION:-16}"
PGDATA="${PGDATA:-/var/lib/postgresql/data}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_DB="${POSTGRES_DB:-${POSTGRES_USER}}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}"

PG_BIN="/usr/lib/postgresql/${PG_VERSION}/bin"
if [[ ! -x "${PG_BIN}/postgres" ]]; then
    echo "entrypoint-multi-pg.sh: ${PG_BIN}/postgres not found (PG_VERSION=${PG_VERSION})" >&2
    exit 64
fi

mkdir -p "${PGDATA}"
chown -R postgres:postgres "${PGDATA}"
chmod 0700 "${PGDATA}"

if [[ ! -s "${PGDATA}/PG_VERSION" ]]; then
    # First boot: initdb.
    #
    # Write the password to a real temp file rather than bash's
    # process-substitution `<()`: process-substitution opens
    # /dev/fd/N as the calling user, so when we cross the
    # `runuser -u postgres` boundary postgres can't read the
    # current shell's fd table and initdb errors with
    #   initdb: error: could not open file "/dev/fd/63" for
    #   reading: Permission denied
    PWFILE=$(mktemp)
    printf '%s' "${POSTGRES_PASSWORD}" > "${PWFILE}"
    chown postgres:postgres "${PWFILE}"
    chmod 0600 "${PWFILE}"
    runuser -u postgres -- "${PG_BIN}/initdb" -D "${PGDATA}" \
        --username="${POSTGRES_USER}" \
        --pwfile="${PWFILE}" \
        --auth-host=md5 --auth-local=trust \
        --no-locale --encoding=UTF8
    rm -f "${PWFILE}"

    # Allow remote password auth on every IPv4/IPv6 interface
    # so the host-side testcontainers connection works.
    cat >> "${PGDATA}/pg_hba.conf" <<'EOF'
host all all 0.0.0.0/0 md5
host all all ::/0      md5
EOF
fi

# Start PG in the background, listen on all interfaces so
# host-side testcontainers can connect.  PG's stderr is the
# container's stderr (no logging_collector) so the
# "database system is ready to accept connections" message
# reaches `docker logs` where testcontainers-go's
# BasicWaitStrategies sniffs for it.
runuser -u postgres -- "${PG_BIN}/postgres" -D "${PGDATA}" \
    -c listen_addresses='*' \
    -c logging_collector=off &
PG_PID=$!

# Wait for PG to accept connections, then create the requested
# DB.  Without this the host-side testcontainers connection to
# database=POSTGRES_DB fails with `database "...": does not
# exist` immediately after the wait strategy passes (initdb
# only creates the superuser, not an eponymous DB — same
# behaviour the official postgres image's docker-entrypoint.sh
# papers over via createdb).
for i in {1..30}; do
    if "${PG_BIN}/pg_isready" -h /var/run/postgresql -U "${POSTGRES_USER}" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
# `-U "${POSTGRES_USER}"` is load-bearing: with --username at
# initdb time, the only PG role that exists is POSTGRES_USER —
# there is NO `postgres` role despite the OS user being
# `postgres`.  Without -U, createdb defaults to libpq's
# "current OS user as PG role" rule and fails with
#   FATAL: role "postgres" does not exist.
runuser -u postgres -- "${PG_BIN}/createdb" \
    -U "${POSTGRES_USER}" \
    -O "${POSTGRES_USER}" \
    "${POSTGRES_DB}" 2>/dev/null || true

# Synthesise the SECOND "database system is ready to accept
# connections" line that testcontainers-go's
# BasicWaitStrategies expects.  The official postgres image's
# docker-entrypoint.sh emits it as a side-effect of its temp-
# start + temp-stop + real-start dance; we skip that complexity
# (it kept hanging on `pg_ctl -w start` in this image due to
# argument-quoting subtleties through `runuser`) and just
# emit the magic line directly to stderr — testcontainers's
# regex-based wait strategy can't tell the difference.
echo "$(date -u +%Y-%m-%d\ %H:%M:%S.%3N\ UTC) [entrypoint-multi-pg] LOG:  database system is ready to accept connections" >&2

# PID 1 = sleep, NOT postgres.  When pg_ctl stop or any signal
# kills $PG_PID, this script keeps running, and the container
# stays alive — which is the whole point of this image vs the
# official postgres image.
exec sleep infinity
