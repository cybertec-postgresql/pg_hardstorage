#!/usr/bin/env bash
# entrypoint-pg.sh — minimal PG + agent supervisor for testbed
# containers.  Real soak runs override this with the driver-
# prescribed command; the script is here so a `docker run` of
# a freshly-built image is non-fatal.
#
# Behaviour:
#   1. initdb on first boot (idempotent)
#   2. start postgres in the background
#   3. wait for it to accept connections
#   4. start the host agent if /etc/pg_hardstorage/pg_hardstorage.yaml exists
#   5. exec a long-running shell so docker keeps the container alive

set -euo pipefail

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PGUSER="${PGUSER:-postgres}"

# /var/lib/pg_hardstorage is bind-mounted from the host by the
# soak driver's compose file.  The bind shadows the build-time
# `chown pgbackup` from the Dockerfile — the mount inherits the
# HOST directory's owner (typically the developer's UID, not
# anything matching pgbackup inside the container).  Re-chown at
# entrypoint time, after the bind is in place, so the agent
# (running as pgbackup via `docker exec -u pgbackup`) can write
# its HSREPO, chunks/, manifests/, etc.
#
# Defensive: do the same for the keyring dir and the agent log,
# since both are written by the agent + may be on bind mounts
# in some compose generators.
#
# CRITICAL: the chown failure path used to be `2>/dev/null || true`
# which made GH issue #40 invisible — in environments where chown
# silently no-ops (rootless docker user-namespace mapping,
# NFS-backed bind sources with id-squash, certain Docker storage
# drivers), the chown returned 0 but the directory's effective
# owner in the container DIDN'T change, and the agent then failed
# its first `repo init` with `HSREPO: permission denied`.
#
# Now: log the chown attempt + result loudly, and add a defensive
# fallback: if pgbackup still can't write to the repo dir after
# the chown attempt, loosen permissions to 0777 (group + others
# write).  This is a soak-testbed container, not production;
# permissive directory modes inside an ephemeral test container
# are acceptable in exchange for "the test always runs."
if id pgbackup >/dev/null 2>&1; then
    echo "entrypoint-pg.sh: chowning /var/lib/pg_hardstorage to pgbackup"
    if ! chown -R pgbackup:pgbackup /var/lib/pg_hardstorage 2>&1; then
        echo "entrypoint-pg.sh: WARN chown /var/lib/pg_hardstorage failed (likely rootless-docker uid mapping)" >&2
    fi
    if ! chown -R pgbackup:pgbackup /etc/pg_hardstorage/keyring 2>&1; then
        echo "entrypoint-pg.sh: WARN chown /etc/pg_hardstorage/keyring failed" >&2
    fi
    # Pre-flight: confirm pgbackup can actually write into the
    # repo dir.  When this check fails the agent's first repo
    # init will surface as "HSREPO: permission denied" three
    # layers downstream — surface the root cause HERE instead.
    if ! sudo -u pgbackup test -w /var/lib/pg_hardstorage/repo 2>/dev/null; then
        echo "entrypoint-pg.sh: WARN pgbackup cannot write to /var/lib/pg_hardstorage/repo; loosening to 0777 as fallback" >&2
        chmod 0777 /var/lib/pg_hardstorage /var/lib/pg_hardstorage/repo 2>/dev/null || true
        if ! sudo -u pgbackup test -w /var/lib/pg_hardstorage/repo 2>/dev/null; then
            echo "entrypoint-pg.sh: ERROR pgbackup STILL cannot write to /var/lib/pg_hardstorage/repo after chmod 0777" >&2
            echo "entrypoint-pg.sh: ls -laZ /var/lib/pg_hardstorage:" >&2
            ls -la /var/lib/pg_hardstorage >&2 || true
            echo "entrypoint-pg.sh: ls -la /var/lib/pg_hardstorage/repo:" >&2
            ls -la /var/lib/pg_hardstorage/repo >&2 || true
            echo "entrypoint-pg.sh: id pgbackup:" >&2
            id pgbackup >&2 || true
            echo "entrypoint-pg.sh: host bind mount may be read-only or under userns id-mapping that blocks the chown."
            echo "entrypoint-pg.sh: HINT: run \`docker info | grep -E 'rootless|userns'\` on the host to confirm."
            # NOT exit 1 — let the agent fail with its own error
            # so the existing test infrastructure observes the
            # failure cleanly.  We've now made the cause loud.
        else
            echo "entrypoint-pg.sh: chmod 0777 fallback restored write access"
        fi
    else
        echo "entrypoint-pg.sh: pgbackup can write to /var/lib/pg_hardstorage/repo ✓"
    fi
fi

# Pick the right pg_ctl binary by walking the standard install
# locations.  Both pgdg-apt (/usr/lib/postgresql/N/bin) and
# distro-packaged (/usr/bin) PG live in this list.
locate_pg_ctl() {
    for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /usr/bin; do
        if [[ -x "$d/pg_ctl" ]]; then
            echo "$d/pg_ctl"
            return 0
        fi
    done
    echo "entrypoint-pg.sh: pg_ctl not found" >&2
    return 1
}

PG_CTL=$(locate_pg_ctl)
PG_BIN_DIR=$(dirname "$PG_CTL")

# Make $PGDATA exist + initdb if empty.
mkdir -p "$PGDATA"
chown -R "$PGUSER":"$PGUSER" "$PGDATA" 2>/dev/null || true
chmod 700 "$PGDATA"

if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
    echo "entrypoint-pg.sh: initdb in $PGDATA"
    sudo -u "$PGUSER" "$PG_BIN_DIR/initdb" -D "$PGDATA" --auth=trust

    # Make PG reachable from outside the container.  initdb's
    # defaults only listen on localhost and only let
    # 127.0.0.1/::1 in via pg_hba.  But the soak driver drives
    # PG via the host port mapping (127.0.0.1:1543N → docker
    # bridge → container) — connections arrive via the bridge
    # interface with a non-localhost source address, so we need
    # both listen_addresses=* and a pg_hba rule that accepts
    # them.  Trust auth is fine inside an ephemeral testbed
    # container.
    cat >> "$PGDATA/postgresql.conf" <<'EOF'

# pg_hardstorage testbed: listen on all interfaces so the
# soak driver can reach PG via the docker port mapping.
listen_addresses = '*'
# Fedora / RHEL distro packages enable logging_collector by
# default, which silently redirects postgres' stdout/stderr
# to $PGDATA/log/*.log BEFORE the failure-reason ever reaches
# pg_ctl's -l file.  Force it off so a startup failure shows
# up in /var/log/postgresql.log directly — debian / suse
# already default this off, this just normalises across all
# three families.
logging_collector = off
EOF
    # SECURITY: restrict trust auth to docker's bridge subnets,
    # NOT 0.0.0.0/0.  The agent + sustained-load pgbench connect
    # from the docker bridge gateway (172.16.0.0/12) and that's
    # the only source we need to accept; combining `host all all
    # 0.0.0.0/0 trust` with the compose file's `0.0.0.0:NNNN`
    # port bind (now fixed to bind 127.0.0.1 only) exposes a
    # no-auth PG to the LAN.  Soak testing saw a cryptominer
    # dropped at /tmp/mysql via `COPY ... FROM PROGRAM` within
    # minutes of the first port bind — exactly that footgun.
    #
    # 172.16.0.0/12 covers docker's default bridge subnet range
    # (172.17.x for the default bridge, 172.18-31.x for
    # compose-created networks).  10.0.0.0/8 covers the
    # alternative ranges some operators configure.  Keep both
    # for the cell containers; localhost (initdb's 127.0.0.1/32
    # rule) covers in-container loopback.  `replication` needs
    # the same posture for `wal stream`.
    cat >> "$PGDATA/pg_hba.conf" <<'EOF'

# pg_hardstorage testbed: docker bridge subnets only.
# See entrypoint-pg.sh for the security rationale (soak testing
# cryptominer incident).  Do NOT widen this to 0.0.0.0/0.
host    all          all   172.16.0.0/12   trust
host    all          all   10.0.0.0/8      trust
host    replication  all   172.16.0.0/12   trust
host    replication  all   10.0.0.0/8      trust
EOF
    # initdb's --auth=trust covers `local` and `host` for
    # localhost, but `replication` is a separate connection
    # type that defaults to peer/scram even with --auth=trust.
    # The soak's WAL-streaming tests need it.
    chown "$PGUSER":"$PGUSER" "$PGDATA/postgresql.conf" "$PGDATA/pg_hba.conf" 2>/dev/null || true
fi

# PG's default unix_socket_directories points at
# /var/run/postgresql.  Debian/Ubuntu's postgresql package
# creates this dir as part of its postinstall; Fedora /
# RHEL ship the dir via systemd-tmpfiles.d, which doesn't
# run inside a Docker container.  Without it, postmaster
# bootstrap fails with
#   FATAL: could not create lock file
#   "/var/run/postgresql/.s.PGSQL.5432.lock": No such file
#   or directory
# even after the network listener is up.  Create + chown
# unconditionally — `mkdir -p` is a no-op on debian/suse
# where the package created it already, and the chown is
# cheap.
mkdir -p /var/run/postgresql
chown "$PGUSER":"$PGUSER" /var/run/postgresql
chmod 2775 /var/run/postgresql

# Pre-create the log files PG and the agent will write to.
# `pg_ctl -l <path>` opens the log via the postgres user (we
# sudo to it).  /var/log/ is root-only on every distro family
# we ship — debian, rhel, suse — so the redirect fails with
#
#   /bin/sh: 1: cannot create /var/log/postgresql.log: Permission denied
#   pg_ctl: could not start server
#
# unless we touch + chown the file ahead of time as root (the
# entrypoint's effective uid).
#
# pg_hardstorage.log is chowned to pgbackup because the agent
# runs as that user — the host CLI gate
# (internal/cli/refuse_root.go) rejects euid 0 and the testkit
# invokes the agent via `docker exec -u pgbackup`, so leaving
# the file as root:root would have the agent fail to open it
# for write on first message.
mkdir -p /var/log
touch /var/log/postgresql.log /var/log/pg_hardstorage.log
chown "$PGUSER":"$PGUSER" /var/log/postgresql.log
chown pgbackup:pgbackup /var/log/pg_hardstorage.log 2>/dev/null || true
chmod 0644 /var/log/postgresql.log /var/log/pg_hardstorage.log

# Start PG in the background.  Catch failure explicitly: with
# `set -e` an `if !` branch would otherwise short-circuit the
# log dump that's our only window into "pg_ctl said `could not
# start server` — examine the log output" failures.  Without
# this, `docker logs` shows the entrypoint's complaint but
# never the actual postgres reason (port already in use,
# shared memory exhaustion, SELinux denial, distro-package
# quirk, etc.) — exactly the rhel/fedora regression we just
# debugged where the symptom was three layers removed from
# the cause.
set +e
sudo -u "$PGUSER" "$PG_CTL" -D "$PGDATA" -l /var/log/postgresql.log start
pg_ctl_rc=$?
set -e
if [[ "$pg_ctl_rc" -ne 0 ]]; then
    echo "entrypoint-pg.sh: pg_ctl start exited $pg_ctl_rc — dumping postgresql.log" >&2
    echo "------------------------ /var/log/postgresql.log ------------------------" >&2
    cat /var/log/postgresql.log >&2 || true
    echo "-----------------------------------------------------------------------" >&2
    # Belt-and-suspenders for distros where logging_collector
    # somehow stayed on: dump anything in $PGDATA/log/ too.
    if compgen -G "$PGDATA/log/*.log" > /dev/null; then
        echo "------------------------ \$PGDATA/log/*.log ----------------------------" >&2
        # shellcheck disable=SC2086
        for f in "$PGDATA"/log/*.log; do
            echo "--- $f ---" >&2
            cat "$f" >&2 || true
        done
        echo "-----------------------------------------------------------------------" >&2
    fi
    echo "entrypoint-pg.sh: postgresql.conf tail:" >&2
    tail -20 "$PGDATA/postgresql.conf" >&2 || true
    echo "entrypoint-pg.sh: PG_VERSION + PGDATA listing:" >&2
    cat "$PGDATA/PG_VERSION" >&2 2>&1 || true
    ls -la "$PGDATA" >&2 || true
    exit 1
fi

# Wait until the socket accepts connections.  60 s upper bound
# covers slow first-boot images (opensuse + glibc-bound initdb
# can take ~15 s on a small VM).
ready=0
for _ in {1..60}; do
    if sudo -u "$PGUSER" "$PG_BIN_DIR/psql" -c 'SELECT 1' >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done

# If PG never came up, dump the postgresql log + a clear
# marker line so `docker logs <container>` shows what went
# wrong instead of just an idle `tail -F`.
if [[ "$ready" -ne 1 ]]; then
    echo "entrypoint-pg.sh: PG never became ready within 60 s." >&2
    echo "------------------------ /var/log/postgresql.log ------------------------" >&2
    cat /var/log/postgresql.log >&2 || true
    echo "-----------------------------------------------------------------------" >&2
    exit 1
fi
echo "entrypoint-pg.sh: PG is ready (listen_addresses=*, pg_hba allows bridge)."

# Start the agent if a config is mounted.  `sudo -u pgbackup`
# matches production posture: the host CLI refuses euid 0, and
# this entrypoint path is the standalone `docker run` fallback
# (the soak driver invokes the agent via `docker exec -u
# pgbackup` directly and doesn't traverse this branch).
if [[ -s /etc/pg_hardstorage/pg_hardstorage.yaml ]]; then
    sudo -u pgbackup /usr/local/bin/pg_hardstorage agent \
        --config /etc/pg_hardstorage/pg_hardstorage.yaml \
        > /var/log/pg_hardstorage.log 2>&1 &
fi

# Keep the container alive.  The soak driver tails the logs
# over `docker logs` and signals via `docker kill` /
# fault-injection helpers.
exec tail -F /var/log/postgresql.log /var/log/pg_hardstorage.log 2>/dev/null
