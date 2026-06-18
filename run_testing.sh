#!/usr/bin/env bash
# run_testing.sh — soak-test driver (single or parallel slots).
#
# Builds the binaries, generates a randomised fleet of
# <count> cells, brings up the docker-compose stack,
# runs the soak loop for <duration>, archives the report.
#
# With --parallel N (N > 1), the script forks N independent
# soak slots — disjoint compose project, host-port range,
# report dir, seed.  Each slot is a normal single-slot run;
# binary builds are shared, fleet + images are per-slot.
# Aggregated PASS/FAIL is printed at the end and the script
# exits non-zero if any slot failed.
#
# Usage:
#   ./run_testing.sh <count> <duration> [options]
#
# Options:
#   --seed N           rng seed (default: today's YYYYMMDD)
#   --report-dir P     report output dir (default: ./test-runs/run-<ts>)
#   --parallel N       run N slots concurrently (default 1)
#   --host-port-base N first PG host port (default 15432; slot k offsets by k*100)
#   --profile NAME     soak profile to drive (default oltp_smoke; see
#                      profiles.yaml — warehouse, schema_churn,
#                      enterprise_heavy ship by default)
#   --fault-rate F     per-iteration fault probability 0..1 (default
#                      testkit-internal 0.2)
#   --max-containers N soak the whole matrix in sequential batches,
#                      never exceeding N containers at once (one cell
#                      = 1 PG + 1 toxiproxy container, so a batch
#                      holds N/2 cells).  Mutually exclusive with
#                      --parallel.
#   --fleet PATH       use this fleet YAML verbatim instead of
#                      generating a random one
#   --no-build         skip make build / make build-testkit
#   --no-up            skip docker compose up / down (assume up)
#   --dry-run          drive the orchestrator against fake cells
#                      (no PG, no Docker required)
#   --keep-on-failure  preserve containers + bundle for forensics
#   --skip-mem-check   skip the host-RAM preflight (see issue #46)
#   --help             this message
#
# Examples:
#   ./run_testing.sh 5 4h
#   ./run_testing.sh 8 30m --seed 42
#   ./run_testing.sh 3 5m --dry-run
#   ./run_testing.sh 3 30m --parallel 4   # 4 slots × 3 cells = 12 cells of coverage
#   ./run_testing.sh 32 5m --max-containers 16  # whole matrix, ≤16 containers at a time

set -euo pipefail

# Pin TMPDIR off /tmp.  /tmp is a tmpfs with a fixed inode ceiling
# (~1 M on most distros) and minio bind-mounts run as root inside the
# container, so even sudo-less cleanup races accumulate root-owned
# scratch dirs that exhaust inodes.  Repo-local ext4 path has plenty.
#
# Override with HS_TMPDIR=<path> in the environment when the
# repo-local default lands on the wrong filesystem (e.g. a checkout
# on a small disk with a separate large mount available):
#
#     HS_TMPDIR=/data/tmp ./run_testing.sh 5 30m
#
# The resolved value is echoed below on every launch, so it's never
# invisible.
HS_TMPDIR="${HS_TMPDIR:-$(cd "$(dirname "$0")" && pwd)/test-runs/tmp}"
mkdir -p "$HS_TMPDIR"
export TMPDIR="$HS_TMPDIR"
echo "info: TMPDIR=$TMPDIR (override: HS_TMPDIR=<path> $0 ...)" >&2

usage() {
    # `sed -E` for POSIX ERE so `?` works as a quantifier on
    # both GNU sed (Linux) and BSD sed (macOS).  The pre-fix
    # `\?` form is a GNU extension and silently no-ops on
    # macOS, which made `./run_testing.sh` look like it did
    # nothing when invoked with no args.
    sed -nE '2,/^set -/{ /^set -/d; s/^# ?//p; }' "$0"
    exit "${1:-0}"
}

if [[ $# -lt 2 ]] || [[ "${1-}" == "--help" || "${1-}" == "-h" ]]; then
    if [[ $# -lt 2 && "${1-}" != "--help" && "${1-}" != "-h" ]]; then
        # Loud explanation BEFORE the usage block so a hurried
        # operator doesn't miss why the script bailed.  Stderr
        # so a `./run_testing.sh | tee log` still surfaces it.
        echo "run_testing.sh: missing required arguments <count> <duration>" >&2
        echo "                e.g. ./run_testing.sh 5 30m" >&2
        echo "                full usage:" >&2
        echo "" >&2
        usage 2
    fi
    usage 0
fi

COUNT="$1"
DURATION="$2"
shift 2

SEED=""
REPORT_DIR=""
NO_BUILD=0
NO_UP=0
DRY_RUN=0
KEEP_ON_FAILURE=0
PARALLEL=1
HOST_PORT_BASE=15432
PROFILE="oltp_smoke"
FAULT_RATE=""  # empty → testkit's own default (0.2)
FLEET_OVERRIDE=""  # --fleet PATH: use this fleet verbatim, skip `fleet random`
MAX_CONTAINERS=0   # --max-containers N: soak the matrix in batches of ≤N containers
SKIP_MEM_CHECK=0   # --skip-mem-check: bypass the host-RAM preflight (issue #46)

while [[ $# -gt 0 ]]; do
    # Accept the GNU `--opt=value` spelling as well as `--opt value`
    # by splitting the token before it reaches the case below — the
    # examples in --help only show the space form, but operators
    # routinely type `--parallel=4`, and a silent `usage 2` exit on
    # that is a needless papercut.
    if [[ "$1" == --*=* ]]; then
        set -- "${1%%=*}" "${1#*=}" "${@:2}"
    fi
    case "$1" in
        --seed) SEED="$2"; shift 2 ;;
        --report-dir) REPORT_DIR="$2"; shift 2 ;;
        --parallel) PARALLEL="$2"; shift 2 ;;
        --host-port-base) HOST_PORT_BASE="$2"; shift 2 ;;
        --profile) PROFILE="$2"; shift 2 ;;
        --fault-rate) FAULT_RATE="$2"; shift 2 ;;
        --max-containers) MAX_CONTAINERS="$2"; shift 2 ;;
        --fleet) FLEET_OVERRIDE="$2"; shift 2 ;;
        --no-build) NO_BUILD=1; shift ;;
        --no-up) NO_UP=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        --keep-on-failure) KEEP_ON_FAILURE=1; shift ;;
        --skip-mem-check) SKIP_MEM_CHECK=1; shift ;;
        --help|-h) usage 0 ;;
        *) echo "unknown option: $1" >&2; usage 2 ;;
    esac
done

# --parallel must be a positive integer.
if ! [[ "$PARALLEL" =~ ^[0-9]+$ ]] || [[ "$PARALLEL" -lt 1 ]]; then
    echo "run_testing.sh: --parallel must be ≥1 (got $PARALLEL)" >&2
    exit 2
fi
if ! [[ "$HOST_PORT_BASE" =~ ^[0-9]+$ ]]; then
    echo "run_testing.sh: --host-port-base must be a port number (got $HOST_PORT_BASE)" >&2
    exit 2
fi

# --max-containers N: soak the full matrix in sequential batches,
# each capped at N containers.  A cell is 1 PG + 1 toxiproxy
# container, so a batch holds floor(N/2) cells.  Mutually exclusive
# with --parallel — they are competing concurrency models and
# combining them would blow straight past the cap.
CELLS_PER_BATCH=0
if [[ "$MAX_CONTAINERS" != "0" ]]; then
    if ! [[ "$MAX_CONTAINERS" =~ ^[0-9]+$ ]] || [[ "$MAX_CONTAINERS" -lt 2 ]]; then
        echo "run_testing.sh: --max-containers must be an integer ≥2 (1 cell = 1 PG + 1 toxiproxy container; got $MAX_CONTAINERS)" >&2
        exit 2
    fi
    if [[ "$PARALLEL" -gt 1 ]]; then
        echo "run_testing.sh: --max-containers and --parallel are competing concurrency models — pass only one" >&2
        exit 2
    fi
    CELLS_PER_BATCH=$((MAX_CONTAINERS / 2))
fi

# Validate count is a positive integer.
if ! [[ "$COUNT" =~ ^[0-9]+$ ]] || [[ "$COUNT" -lt 1 ]]; then
    echo "run_testing.sh: <count> must be a positive integer (got $COUNT)" >&2
    exit 2
fi

# Validate / normalise <duration>.  Go's time.ParseDuration (which
# the orchestrator's --duration flag delegates to) accepts only
# `ns / us / µs / ms / s / m / h` as units; common natural-language
# spellings like `5min`, `1hour`, `30sec` are rejected.  Without
# this guard the rejection lands AFTER the 3-5 minute build +
# compose-up phase, wasting a soak slot.
#
# Strategy: best-effort normalise the obvious human variants
# (min/mins/minutes → m, sec/secs/seconds → s, hour/hours → h) so
# `./run_testing.sh 5 5min` "just works", then fail-fast with a
# pointer if the result still isn't shaped like a Go duration.
case "$DURATION" in
    *minutes) DURATION="${DURATION%minutes}m" ;;
    *minute)  DURATION="${DURATION%minute}m"  ;;
    *mins)    DURATION="${DURATION%mins}m"    ;;
    *min)     DURATION="${DURATION%min}m"     ;;
    *seconds) DURATION="${DURATION%seconds}s" ;;
    *second)  DURATION="${DURATION%second}s"  ;;
    *secs)    DURATION="${DURATION%secs}s"    ;;
    *sec)     DURATION="${DURATION%sec}s"     ;;
    *hours)   DURATION="${DURATION%hours}h"   ;;
    *hour)    DURATION="${DURATION%hour}h"    ;;
esac
if ! [[ "$DURATION" =~ ^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$ ]]; then
    echo "run_testing.sh: <duration> must be a Go duration like 30s, 5m, 4h, 1h30m (got $DURATION)" >&2
    exit 2
fi

# Default seed: stable per-day so reruns within a day are
# deterministic but different days explore different fleets.
if [[ -z "$SEED" ]]; then
    SEED="$(date +%Y%m%d)"
fi

# Default report dir.  Lowercase + numeric only — Docker Compose
# derives the project name from the basename and refuses uppercase
# letters (regex: lowercase alphanumeric + hyphens + underscores,
# starting with a letter or digit).  ISO-8601's `T` / `Z`
# separators trip that validator.
if [[ -z "$REPORT_DIR" ]]; then
    REPORT_DIR="./test-runs/run-$(date -u +%Y%m%d-%H%M%S)"
fi
mkdir -p "$REPORT_DIR"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

green() { printf '\033[32m✓\033[0m %s\n' "$*"; }
note()  { printf '\033[36m●\033[0m %s\n' "$*"; }

# ---------- Batched orchestration (--max-containers) ----------------
# Soak the whole matrix without ever exceeding --max-containers
# containers: generate the full fleet once, split it into batches
# of CELLS_PER_BATCH cells, and run each batch as an ordinary
# single-slot run, one after another.  Only one batch's containers
# are alive at a time; the previous batch is torn down before the
# next is brought up.
#
# Each batch re-execs this same script with `--fleet <batch>
# --parallel 1`, so a batch IS a normal single-slot run — no
# special-case code path downstream of here.
if [[ "$CELLS_PER_BATCH" -gt 0 ]]; then
    case "$(uname -m)" in
        x86_64|amd64)   TESTBED_GOARCH=amd64 ;;
        aarch64|arm64)  TESTBED_GOARCH=arm64 ;;
        *)              TESTBED_GOARCH="$(go env GOARCH)" ;;
    esac
    if [[ "$NO_BUILD" -eq 0 ]]; then
        note "Building binaries once for batched run"
        GOOS=linux GOARCH="$TESTBED_GOARCH" make build
        make build-testkit
        GOOS=linux GOARCH="$TESTBED_GOARCH" make build-compat || true
        green "binaries built"
    fi
    TESTKIT="$REPO_ROOT/bin/pg_hardstorage_testkit"
    if [[ ! -x "$TESTKIT" ]]; then
        echo "run_testing.sh: $TESTKIT not found; drop --no-build" >&2
        exit 1
    fi

    # Pin the generated fleet to the host arch (cross-arch images
    # need QEMU/buildx) — same rationale as the single-slot path.
    case "$(uname -m)" in
        x86_64)         BATCH_ARCH=amd64 ;;
        aarch64|arm64)  BATCH_ARCH=arm64 ;;
        *)              BATCH_ARCH="" ;;
    esac
    BATCH_ARCH_FLAG=()
    [[ -n "$BATCH_ARCH" ]] && BATCH_ARCH_FLAG=(--arch "$BATCH_ARCH")

    FULL_FLEET="$REPORT_DIR/fleet-full.yaml"
    BATCH_FLEET_DIR="$REPORT_DIR/batch-fleets"
    if [[ -n "$FLEET_OVERRIDE" ]]; then
        note "Using supplied fleet: $FLEET_OVERRIDE"
        cp "$FLEET_OVERRIDE" "$FULL_FLEET"
    else
        note "Generating full fleet (up to $COUNT cells, seed=$SEED, arch=${BATCH_ARCH:-all})"
        "$TESTKIT" fleet random --file "$FULL_FLEET" --count "$COUNT" \
            --seed "$SEED" ${BATCH_ARCH_FLAG[@]+"${BATCH_ARCH_FLAG[@]}"} --force
    fi
    "$TESTKIT" fleet split --file "$FULL_FLEET" --size "$CELLS_PER_BATCH" \
        --out-dir "$BATCH_FLEET_DIR"

    mapfile -t BATCH_FILES < <(ls "$BATCH_FLEET_DIR"/batch-*.yaml | sort)
    note "Soaking ${#BATCH_FILES[@]} batch(es) of ≤$CELLS_PER_BATCH cells (≤$MAX_CONTAINERS containers each), $DURATION per batch"

    batch_fail=0
    bi=0
    for bf in "${BATCH_FILES[@]}"; do
        bi=$((bi + 1))
        bdir="$REPORT_DIR/batch$(printf '%03d' "$bi")"
        mkdir -p "$bdir"
        forward=(
            "$COUNT" "$DURATION"
            --seed "$SEED"
            --report-dir "$bdir"
            --host-port-base "$HOST_PORT_BASE"
            --parallel 1
            --no-build
            --fleet "$bf"
        )
        [[ "$NO_UP" -eq 1 ]]           && forward+=(--no-up)
        [[ "$DRY_RUN" -eq 1 ]]         && forward+=(--dry-run)
        [[ "$KEEP_ON_FAILURE" -eq 1 ]] && forward+=(--keep-on-failure)
        [[ -n "$PROFILE" && "$PROFILE" != "oltp_smoke" ]] && forward+=(--profile "$PROFILE")
        [[ -n "$FAULT_RATE" ]]         && forward+=(--fault-rate "$FAULT_RATE")
        note "── batch $bi/${#BATCH_FILES[@]} ($(grep -c 'os:' "$bf" || echo '?') cells) → $bdir ──"
        # `if cmd; then` keeps `set -e` from aborting the run when a
        # batch fails; we want to soak the remaining batches and
        # report the total at the end.
        if bash "$0" "${forward[@]}" > "$bdir/run.log" 2>&1; then
            green "batch $bi passed"
        else
            note "batch $bi FAILED (see $bdir/run.log)"
            batch_fail=$((batch_fail + 1))
        fi
    done

    AGG_FILE="$REPORT_DIR/batched-summary.md"
    {
        echo "# Batched soak — ${#BATCH_FILES[@]} batches × ≤$CELLS_PER_BATCH cells (cap $MAX_CONTAINERS containers)"
        echo
        echo "**Duration target per batch:** $DURATION"
        echo
        for bi2 in $(seq 1 "${#BATCH_FILES[@]}"); do
            bdir="$REPORT_DIR/batch$(printf '%03d' "$bi2")"
            verdict="(no report)"
            [[ -f "$bdir/report.json" ]] && verdict="report.json written"
            echo "- batch $bi2: $bdir — $verdict"
        done
    } > "$AGG_FILE"
    green "aggregate summary at $AGG_FILE"

    if [[ "$batch_fail" -gt 0 ]]; then
        echo "ERROR: $batch_fail of ${#BATCH_FILES[@]} batches failed" >&2
        exit 1
    fi
    green "all ${#BATCH_FILES[@]} batches passed"
    exit 0
fi

# ---------- Parallel orchestration ---------------------------------
# When --parallel N (N > 1) is set, build the binaries once at this
# top-level invocation and then fork N self-recursive child runs,
# each with --no-build, --parallel 1, a disjoint slot directory,
# seed offset, project name (derived from REPORT_DIR basename per
# slot), and a 100-port-wide host-port-base window.
#
# We re-exec via `bash "$0" ...` rather than orchestrating slots
# inline so each slot is exactly the same code path as a normal
# single-slot run — easier to reason about, no ad-hoc per-slot
# state in this driver.  The child process's normal cleanup trap
# tears its own slot down on exit.
if [[ "$PARALLEL" -gt 1 ]]; then
    # GOOS=linux GOARCH=<host-arch> on the testbed-bound binaries —
    # see the single-slot build block below for the rationale (GH
    # issue #16: macOS hosts produced darwin binaries that COPY into
    # linux testbed containers and crashed on `RUN` with `Exec
    # format error`).  Cross-compile is trivial because CGO is off.
    case "$(uname -m)" in
        x86_64|amd64)        TESTBED_GOARCH=amd64 ;;
        aarch64|arm64)       TESTBED_GOARCH=arm64 ;;
        *)                   TESTBED_GOARCH="$(go env GOARCH)" ;;
    esac
    if [[ "$NO_BUILD" -eq 0 ]]; then
        note "Building binaries once for $PARALLEL slots"
        GOOS=linux GOARCH="$TESTBED_GOARCH" make build
        make build-testkit
        GOOS=linux GOARCH="$TESTBED_GOARCH" make build-compat || true
        green "binaries built"
    fi
    note "Spawning $PARALLEL parallel slots (base seed=$SEED, base port=$HOST_PORT_BASE)"
    declare -a SLOT_PIDS=()
    declare -a SLOT_DIRS=()
    for k in $(seq 0 $((PARALLEL-1))); do
        slot_dir="$REPORT_DIR/slot$k"
        slot_seed=$((SEED + k))
        slot_port_base=$((HOST_PORT_BASE + k * 100))
        mkdir -p "$slot_dir"
        SLOT_DIRS+=("$slot_dir")
        # Forward every flag the user might have set, with per-slot overrides.
        forward=(
            "$COUNT" "$DURATION"
            --seed "$slot_seed"
            --report-dir "$slot_dir"
            --host-port-base "$slot_port_base"
            --parallel 1
            --no-build
        )
        [[ "$NO_UP" -eq 1 ]]            && forward+=(--no-up)
        [[ "$DRY_RUN" -eq 1 ]]          && forward+=(--dry-run)
        [[ "$KEEP_ON_FAILURE" -eq 1 ]]  && forward+=(--keep-on-failure)
        [[ -n "$PROFILE" && "$PROFILE" != "oltp_smoke" ]] && forward+=(--profile "$PROFILE")
        [[ -n "$FAULT_RATE" ]]          && forward+=(--fault-rate "$FAULT_RATE")
        # Stagger slot starts by 2s — concurrent docker compose ups
        # against a single daemon occasionally race on network/volume
        # creation; the small offset eliminates the noise.
        ( sleep $((k * 2)); bash "$0" "${forward[@]}" ) \
            > "$slot_dir/run.log" 2>&1 &
        SLOT_PIDS+=($!)
        note "  slot $k: pid=${SLOT_PIDS[$k]}, dir=$slot_dir, seed=$slot_seed, port-base=$slot_port_base"
    done

    fail_count=0
    for k in "${!SLOT_PIDS[@]}"; do
        if wait "${SLOT_PIDS[$k]}"; then
            green "slot $k passed"
        else
            note "slot $k FAILED (see ${SLOT_DIRS[$k]}/run.log)"
            fail_count=$((fail_count+1))
        fi
    done

    # Aggregate verdict.  Each slot writes its own report.json;
    # we collect the per-slot overall_pass + cell counts into a
    # single human-readable summary and print it.
    AGG_FILE="$REPORT_DIR/parallel-summary.md"
    {
        echo "# Parallel soak run — $PARALLEL slots × $COUNT cells"
        echo
        echo "**Wall-clock duration target:** $DURATION  "
        echo "**Base seed:** $SEED  "
        echo
        echo "| Slot | Verdict | Cells | Failures | Report |"
        echo "| --- | --- | --- | --- | --- |"
        for k in "${!SLOT_DIRS[@]}"; do
            rj="${SLOT_DIRS[$k]}/report.json"
            if [[ -s "$rj" ]]; then
                pass=$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print('PASS' if d['overall_pass'] else 'FAIL')" "$rj" 2>/dev/null || echo "?")
                cells=$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(len(d.get('cells', [])))" "$rj" 2>/dev/null || echo "?")
                fails=$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(len(d.get('failures', [])))" "$rj" 2>/dev/null || echo "?")
            else
                pass="NO REPORT"; cells="?"; fails="?"
            fi
            echo "| $k | $pass | $cells | $fails | [report.md](slot$k/report.md) |"
        done
        echo
        echo "Logs: \`$REPORT_DIR/slot*/run.log\`"
    } > "$AGG_FILE"
    green "aggregate summary at $AGG_FILE"

    if [[ "$fail_count" -gt 0 ]]; then
        echo "ERROR: $fail_count of $PARALLEL slots failed" >&2
        exit 1
    fi
    exit 0
fi

FLEET_PATH="$REPORT_DIR/fleet.yaml"
COMPOSE_PATH="$REPORT_DIR/docker-compose.yaml"
FAULTS_PATH="$REPORT_DIR/faults.yaml"

# ---------- 1. Build the binaries -----------------------------------
#
# pg_hardstorage and pg-hardstorage-compat are COPYed into the
# linux testbed containers (see dockerfiles/testbed/Dockerfile.*'s
# `COPY bin/pg_hardstorage /usr/local/bin/pg_hardstorage` + `RUN
# chmod 755 ... && /usr/local/bin/pg_hardstorage version`).  Build
# them as linux/<host-arch> binaries regardless of host OS so this
# works on macOS / FreeBSD / etc.; without GOOS=linux the testbed
# image build instantly fails with `Exec format error` on the
# `RUN` line — see GH issue #16.  CGO is already off (Makefile
# defaults `CGO_ENABLED=0`) so the cross-compile is trivial.
#
# The testkit binary stays host-native — it's the orchestrator
# that runs OUTSIDE containers, on the same host this script is
# invoked from.
case "$(uname -m)" in
    x86_64|amd64)        TESTBED_GOARCH=amd64 ;;
    aarch64|arm64)       TESTBED_GOARCH=arm64 ;;
    *)                   TESTBED_GOARCH="$(go env GOARCH)" ;;
esac
if [[ "$NO_BUILD" -eq 0 ]]; then
    note "Building binaries (set --no-build to skip)"
    GOOS=linux GOARCH="$TESTBED_GOARCH" make build
    make build-testkit
    GOOS=linux GOARCH="$TESTBED_GOARCH" make build-compat || true   # compat shims optional for soak
    green "binaries built"
fi

TESTKIT="$REPO_ROOT/bin/pg_hardstorage_testkit"
if [[ ! -x "$TESTKIT" ]]; then
    echo "run_testing.sh: $TESTKIT not found; remove --no-build or run \`make build-testkit\`" >&2
    exit 1
fi

# ---------- 2. Generate the fleet -----------------------------------
# Pin to host arch by default — building cross-arch testbed
# images requires QEMU + buildx setup most operators don't
# have.  Operators wanting a mixed amd64/arm64 fleet pass
# --arch via --extra-fleet-args (or hand-edit fleet.yaml).
case "$(uname -m)" in
    x86_64)          HOST_ARCH=amd64 ;;
    aarch64|arm64)   HOST_ARCH=arm64 ;;
    *)               HOST_ARCH="" ;;  # unrecognised; let the catalog pick
esac

# <count> is an upper bound: `fleet random` emits each catalog
# (OS, PG, arch) cell at most once, so the fleet is capped at the
# catalog's distinct-cell count.  `fleet random` prints a loud
# warning when it clamps; the wording here stays "up to" so this
# line doesn't claim a cell count the catalog can't deliver.
if [[ -n "$FLEET_OVERRIDE" ]]; then
    # A batched run (--max-containers) re-execs this script once per
    # batch with the batch's pre-split fleet; use it verbatim rather
    # than generating a fresh random one.
    note "Using supplied fleet: $FLEET_OVERRIDE"
    cp "$FLEET_OVERRIDE" "$FLEET_PATH"
else
    note "Generating fleet (up to $COUNT cells, seed=$SEED, arch=${HOST_ARCH:-all})"
    ARCH_FLAG=()
    if [[ -n "$HOST_ARCH" ]]; then
        ARCH_FLAG=(--arch "$HOST_ARCH")
    fi
    "$TESTKIT" fleet random --file "$FLEET_PATH" --count "$COUNT" --seed "$SEED" \
        ${ARCH_FLAG[@]+"${ARCH_FLAG[@]}"} --force
fi
"$TESTKIT" fleet validate --file "$FLEET_PATH"
green "fleet at $FLEET_PATH"

# ---------- 3. Generate the faults catalogue (if missing) -----------
if [[ ! -s "$FAULTS_PATH" ]]; then
    cat > "$FAULTS_PATH" <<'EOF'
schema: pg_hardstorage.testkit.fault.v1
version: 1
faults:
  - { name: agent_kill_15, weight: 8, action: "signal(target=agent_random, sig=15)" }
  - { name: pg_kill_9,     weight: 4, action: "signal(target=pg_random, sig=9)" }
  - { name: disk_full_repo, weight: 5, action: "disk_full(target=repo, fill=98%)" }
  - { name: cgroup_squeeze, weight: 3, action: "cgroup_squeeze(target=pg_random, max_bytes=33554432)" }
  - { name: pause_archive,  weight: 4, action: "pause_archive(target=agent)" }
EOF
    green "faults catalogue at $FAULTS_PATH"
fi
"$TESTKIT" fault validate --file "$FAULTS_PATH"

# ---------- 4. Generate docker-compose.yaml --------------------------
# Absolutise the host repo dir before passing it to compose:
# Docker daemon resolves bind `device:` paths against ITS OWN
# cwd (typically `/`), not the operator's.  The compose
# generator also resolves to absolute defensively, but
# resolving here keeps the path the shell `mkdir` writes
# identical to the path compose emits — debuggable from `cat
# compose.yaml` alone.
mkdir -p "$REPORT_DIR/repo-data"
HOST_REPO_DIR="$(cd "$REPORT_DIR/repo-data" && pwd)"

# Pre-flight: make sure $HOST_REPO_DIR is actually writable
# under THIS user.  Bail out before the 3-5 minute build +
# compose-up phase if a (most likely operator-mistake) read-
# only path was supplied.  This catches GH issue #40 class
# failures on the HOST side; the testbed entrypoint's chown
# fallback covers the equivalent failure on the CONTAINER side.
if ! touch "$HOST_REPO_DIR/.writable-check" 2>/dev/null; then
    echo "run_testing.sh: ERROR cannot write to $HOST_REPO_DIR" >&2
    echo "                  the docker daemon will bind-mount this into the testbed at" >&2
    echo "                  /var/lib/pg_hardstorage/repo and the agent will fail with" >&2
    echo "                  \"HSREPO: permission denied\" five minutes from now." >&2
    echo "                  fix the host path permissions or pick a different --report-dir." >&2
    ls -ld "$HOST_REPO_DIR" >&2 || true
    exit 1
fi
rm -f "$HOST_REPO_DIR/.writable-check"

# Compose project name = lowercased basename, with anything
# outside [a-z0-9_-] coerced to '-' so an operator-supplied
# --report-dir can't break compose-up.  The leading prefix
# guarantees the result starts with a letter.  Computed once
# here and reused below.
#
# Use sed for the char-class substitution rather than `tr -c`:
# `tr -c` operates byte-for-byte on its input and translates
# the trailing newline to '-' too, producing a stray
# trailing hyphen.
PROJECT_BASE=$(basename "$REPORT_DIR" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9_-]+/-/g; s/^-+//; s/-+$//')
PROJECT="pgvalidate-$PROJECT_BASE"

note "Generating docker-compose.yaml"
"$TESTKIT" compose generate \
    --fleet "$FLEET_PATH" \
    --out "$COMPOSE_PATH" \
    --project "$PROJECT" \
    --host-repo-dir "$HOST_REPO_DIR" \
    --host-port-base "$HOST_PORT_BASE" \
    --force
green "compose at $COMPOSE_PATH"

# ---------- 4b. Clear leftovers + pre-flight the host ports ----------
# Batched mode (--max-containers) re-execs this script once per batch
# with a REUSED project name (pgvalidate-batchNNN) and the SAME fixed
# host-port base.  Two failure modes follow from that and both turned a
# single transient collision into a whole-run "16/16 batches FAILED":
#
#   1. A prior run that used this batch slot can leave containers behind
#      (interrupted run, crash, or a larger earlier fleet).  They linger
#      as compose "orphans" — `docker compose up` warns but does NOT
#      remove them, so they keep holding 127.0.0.1:<base>+i and every
#      new batch dies with "port is already allocated".
#   2. A *concurrent* run (or stray testbed) owns the same port range.
#
# Fix (1) by tearing this project down first (idempotent no-op when the
# slot is empty) and passing --remove-orphans on up/down below.  Catch
# (2) with an explicit port pre-flight that fails fast — before the
# multi-minute image build — with an actionable message, instead of
# letting `docker compose up` emit the cryptic daemon error N times.
if [[ "$NO_UP" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
    docker compose -f "$COMPOSE_PATH" -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true

    # bash /dev/tcp is portable (Linux + macOS) and needs no extra tool.
    port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; }
    ncells=$(grep -c 'os:' "$FLEET_PATH" 2>/dev/null) || ncells=0
    busy_ports=()
    for ((pi = 0; pi < ncells; pi++)); do
        if port_in_use "$((HOST_PORT_BASE + pi))"; then
            busy_ports+=("$((HOST_PORT_BASE + pi))")
        fi
    done
    if [[ "${#busy_ports[@]}" -gt 0 ]]; then
        echo >&2
        echo "run_testing.sh: ERROR host port(s) already in use: ${busy_ports[*]}" >&2
        echo "  this run publishes one PG port per cell on 127.0.0.1 starting at" >&2
        echo "  ${HOST_PORT_BASE}; something else already owns the port(s) above —" >&2
        echo "  most likely another soak run in progress, or a stale testbed." >&2
        echo "  fix one of:" >&2
        echo "    - wait for / stop the other run" >&2
        echo "    - run on a free range:   --host-port-base <N>   (e.g. $((HOST_PORT_BASE + 100)))" >&2
        echo "    - list/sweep stragglers: docker ps --filter name=pgvalidate" >&2
        exit 1
    fi
fi

# ---------- 5. Build testbed images locally -------------------------
# The compose YAML's image tags point at
# ghcr.io/cybertec-postgresql/pg-hardstorage-testbed which
# isn't a published public image set yet — `docker compose up`
# would fall through to a registry pull and fail with `denied`.
# Build only the (os, pg, arch) tuples THIS fleet uses
# (`--from-fleet`) — building the full catalog would mean ~70
# images per run.  Compose's `pull_policy: missing` picks
# the locally-built layers up.
if [[ "$NO_UP" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
    ARCH_BUILD_FLAG=()
    if [[ -n "$HOST_ARCH" ]]; then
        ARCH_BUILD_FLAG=(--only-arch "$HOST_ARCH")
    fi
    note "Building testbed images for the picked fleet (one-time per cell — cached on rebuild)"
    "$TESTKIT" image build --from-fleet "$FLEET_PATH" ${ARCH_BUILD_FLAG[@]+"${ARCH_BUILD_FLAG[@]}"}
    green "testbed images ready"
fi

# ---------- Host-memory preflight (issue #46) -----------------------
# A single (non-batched) soak brings up every cell's containers at
# once with no memory ceiling.  If the fleet does not fit in RAM the
# host OOM-killer picks a container off mid-run, and that cell can
# never complete.  Estimate the footprint up front and fail fast with
# an actionable message instead of crashing an hour into the soak.
#
# ~1 GiB per cell (PostgreSQL + agent + toxiproxy) is a deliberately
# conservative planning figure; --skip-mem-check overrides, and
# --max-containers is the real remedy on a memory-constrained host.
if [[ "$NO_UP" -eq 0 && "$DRY_RUN" -eq 0 && "$SKIP_MEM_CHECK" -eq 0 && -r /proc/meminfo ]]; then
    mem_cells=$(grep -c 'os:' "$FLEET_PATH") || mem_cells=0
    mem_avail_kib=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
    if [[ "$mem_cells" -gt 0 && -n "$mem_avail_kib" ]]; then
        mem_need_mib=$((mem_cells * 1024))
        mem_avail_mib=$((mem_avail_kib / 1024))
        if [[ "$mem_need_mib" -gt "$mem_avail_mib" ]]; then
            fit_cells=$((mem_avail_mib / 1024))
            echo >&2
            echo "run_testing.sh: not enough memory for a ${mem_cells}-cell soak (issue #46)." >&2
            echo "  estimated need: ~${mem_need_mib} MiB   host available: ~${mem_avail_mib} MiB" >&2
            echo "  a cell whose container is OOM-killed mid-soak cannot complete." >&2
            if [[ "$fit_cells" -ge 1 ]]; then
                echo "  fix — soak the matrix in batches that do fit:" >&2
                echo "    ./run_testing.sh $COUNT $DURATION --max-containers $((fit_cells * 2))" >&2
            else
                echo "  fix — free memory or run on a larger host." >&2
            fi
            echo "  override (not recommended): add --skip-mem-check" >&2
            exit 1
        fi
    fi
fi

# ---------- 6. Bring containers up ----------------------------------
if [[ "$NO_UP" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
    note "docker compose up -d (project=$PROJECT)"
    docker compose -f "$COMPOSE_PATH" -p "$PROJECT" up -d --remove-orphans
fi

cleanup() {
    local rc=$?
    if [[ "$NO_UP" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
        if [[ "$rc" -ne 0 && "$KEEP_ON_FAILURE" -eq 1 ]]; then
            note "Soak failed; preserving containers (project=$PROJECT) for forensics."
            note "Tear down manually: docker compose -f $COMPOSE_PATH -p $PROJECT down -v"
        else
            note "docker compose down -v"
            docker compose -f "$COMPOSE_PATH" -p "$PROJECT" down -v --remove-orphans || true
        fi
    fi
    return "$rc"
}
trap cleanup EXIT

# ---------- 6. Generate the profile catalogue (if missing) ----------
PROFILES_PATH="$REPORT_DIR/profiles.yaml"
if [[ ! -s "$PROFILES_PATH" ]]; then
    cat > "$PROFILES_PATH" <<'EOF'
schema: pg_hardstorage.testkit.profile.v1
version: 1
profiles:
  - { name: oltp_smoke,    target_size_gb: 5,  churn_mb_per_min: 50,  schema: tpcc-lite,    backup_every: 5m }
  - { name: warehouse,     target_size_gb: 50, churn_mb_per_min: 5,   schema: bulk-copy,    backup_every: 1h }
  - { name: schema_churn,  target_size_gb: 10, churn_mb_per_min: 25,  schema: schema-churn, backup_every: 10m, ddl_per_min: 2 }
  # enterprise_heavy is the "high constant load + replay" profile:
  # bulk-seed pgbench to ~10 GB then run a 16-client UPDATE-heavy
  # writer concurrently with the iteration loop.  Backup runs
  # against an actively-modified database; the report.md captures
  # TPS / p95 / WAL-bytes-written / WAL-stream-lag.  Pick this with
  # `--profile enterprise_heavy` when you actually want to measure
  # source overhead during backup.
  - { name: enterprise_heavy, target_size_gb: 10, seed_target_gb: 10, sustained_clients: 16, sustained_rate_tps: 0, schema: tpcc-lite, backup_every: 5m }
EOF
    green "profiles catalogue at $PROFILES_PATH"
fi

# ---------- 7. Run the soak ------------------------------------------
EXTRA=""
if [[ "$DRY_RUN" -eq 1 ]]; then
    EXTRA="--dry-run"
fi

note "Running soak: duration=$DURATION profile=$PROFILE fault-rate=${FAULT_RATE:-0.2 (default)}"
echo
echo "  Live view (run in another terminal):"
echo "    $TESTKIT watch $REPORT_DIR"
echo
# Bash 3.2 (still the macOS default) raises "unbound variable" under
# `set -u` when an *empty* array is expanded as "${arr[@]}".  The
# ${arr[@]+...} guard yields zero words when the array is empty and
# the elements — correctly quoted — when it is not; portable from
# Bash 3.2 through 5.x.  See issue #47.
FAULT_RATE_FLAG=()
if [[ -n "$FAULT_RATE" ]]; then
    FAULT_RATE_FLAG=(--fault-rate "$FAULT_RATE")
fi
"$TESTKIT" validate \
    --fleet "$FLEET_PATH" \
    --profiles "$PROFILES_PATH" \
    --profile "$PROFILE" \
    --faults "$FAULTS_PATH" \
    --duration "$DURATION" \
    --seed "$SEED" \
    --project "$PROJECT" \
    --report-dir "$REPORT_DIR" \
    --host-port-base "$HOST_PORT_BASE" \
    ${FAULT_RATE_FLAG[@]+"${FAULT_RATE_FLAG[@]}"} \
    $EXTRA

green "soak completed; reports in $REPORT_DIR"
echo
echo "  Read:   $REPORT_DIR/report.md"
echo "  Parse:  $REPORT_DIR/report.json"
echo "  Replay: $TESTKIT watch $REPORT_DIR  # tail events.ndjson with the same UI"
