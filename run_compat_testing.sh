#!/usr/bin/env bash
# run_compat_testing.sh — multi-distro driver for the compat-shim
# scenarios under test/scenarios/L2_compat_*.scenario.yaml.
#
# The compat shims (pg-hardstorage-pgbackrest /
# pg-hardstorage-barman / pg-hardstorage-barman-wal-archive) are
# user-facing CLI binaries that operators drop into PATH on every
# distro the project supports.  Unit tests stub the dispatcher,
# so they cannot catch the silent-dispatch / dispatch-translation
# class of bugs (the actual reason the Barman shim shipped
# broken before the fix).  This driver runs the shims through
# REAL container processes against a matrix of distro images,
# producing a per-cell pass/fail report.
#
# Usage:
#   ./run_compat_testing.sh [options]
#
# Options:
#   --matrix <preset>     "default" (5 cells) | "wide" (12 cells) |
#                         "smoke" (2 cells, fastest).  Default: default.
#   --shim <list>         Comma-separated subset of {pgbackrest,barman}
#                         to drive.  Default: both.
#   --coverage <list>     Comma-separated subset of
#                         {file,s3,tls,migration,doppelganger}.
#                         file = file:// repos (default
#                         scenarios), s3 = MinIO over HTTP,
#                         tls = MinIO over HTTPS with a self-
#                         signed cert, migration = pgbackrest+
#                         filesystem → pg_hardstorage+S3
#                         transition with a PG failover,
#                         doppelganger = split-brain archive
#                         collision detection (two clusters
#                         with cloned system_identifier).
#                         Default: file,s3,tls.
#   --report-dir <p>      Output dir.  Default: ./test-runs/compat-<ts>.
#   --no-build            Skip make build / build-compat.
#   --keep-on-failure     Preserve artefact dirs of failing cells.
#   --seed N              Shuffle (cell × scenario) order with this
#                         seed.  Useful with --parallel to avoid
#                         deterministic head-of-line bias when N
#                         is small (cells run in matrix order
#                         otherwise; the first --parallel N
#                         always start with the same heavy cells).
#                         Default: today's YYYYMMDD.
#   --parallel N          Run N cells concurrently. Each cell still
#                         runs its OWN scenario set serially inside
#                         (so 5 cells × 4 scenarios stays 5×4=20
#                         scenarios; --parallel 3 means 3 cells
#                         work through their scenarios concurrently).
#                         Default 1.
#   --help                This message.
#
# Each cell is one (os_image × pg_version) pair from the matrix.
# Per cell, both pgbackrest + barman scenarios run (unless --shim
# narrows the set), and their result.json files land under
# <report-dir>/<cell>/<scenario>/.
#
# Exit code: 0 iff every (cell × scenario) pair passed.  The
# script aggregates a markdown summary at <report-dir>/summary.md.
#
# Why a separate driver instead of folding compat into
# run_testing.sh?  run_testing.sh's soak orchestrator drives a
# random fleet through workload profiles (oltp_smoke, warehouse,
# ...).  Compat tests don't need workload — they need a static
# input matrix run once per distro.  Keeping the two flows
# separate keeps each runner readable and lets compat coverage
# evolve (e.g. WAL-G shim) without re-shaping the soak driver.

set -euo pipefail

# Pin TMPDIR off /tmp — see run_testing.sh for the rationale (tmpfs
# inode ceiling + root-owned minio scratch dirs).  Override with
# HS_TMPDIR=<path> when the repo-local default lands on the wrong
# filesystem (the resolved value is echoed below on every launch).
HS_TMPDIR="${HS_TMPDIR:-$(cd "$(dirname "$0")" && pwd)/test-runs/tmp}"
mkdir -p "$HS_TMPDIR"
export TMPDIR="$HS_TMPDIR"
echo "info: TMPDIR=$TMPDIR (override: HS_TMPDIR=<path> $0 ...)" >&2

# Disable testcontainers' Ryuk reaper.  Why: testcontainers/ryuk:0.13.0
# crashes ~2s after start (exit code 1) on Docker 29.x + Fedora 42
# with cgroup-v2 / overlay2.  Once ryuk dies, every cell that
# subsequently asks for a reaper hits "container status 'removing':
# could not start container" and the L2 compat scenarios fail before
# they even bring up postgres.  Ryuk's job — kill leaked
# testcontainers containers if the test process dies — is redundant
# here: the testkit's own per-cell cleanup (docker rm -f on the
# scenario-scoped names) covers the same ground, and `--keep-on-
# failure` deliberately keeps containers around for forensics.
# Override by setting TESTCONTAINERS_RYUK_DISABLED=false explicitly.
export TESTCONTAINERS_RYUK_DISABLED="${TESTCONTAINERS_RYUK_DISABLED:-true}"

usage() {
    # `sed -E` for POSIX ERE so `?` works as a quantifier on
    # both GNU sed (Linux) and BSD sed (macOS).  The pre-fix
    # `\?` form is a GNU extension and silently no-ops on
    # BSD sed, which made `./run_compat_testing.sh --help`
    # look like it did nothing on macOS.
    sed -nE '2,/^set -/{ /^set -/d; s/^# ?//p; }' "$0"
    exit "${1:-0}"
}

MATRIX="default"
SHIM_LIST="pgbackrest,barman,walg"
COVERAGE_LIST="file,s3,tls"
REPORT_DIR=""
NO_BUILD=0
KEEP_ON_FAILURE=0
SEED=""
PARALLEL=1

while [[ $# -gt 0 ]]; do
    case "$1" in
        --matrix) MATRIX="$2"; shift 2 ;;
        --shim) SHIM_LIST="$2"; shift 2 ;;
        --coverage) COVERAGE_LIST="$2"; shift 2 ;;
        --report-dir) REPORT_DIR="$2"; shift 2 ;;
        --no-build) NO_BUILD=1; shift ;;
        --keep-on-failure) KEEP_ON_FAILURE=1; shift ;;
        --seed) SEED="$2"; shift 2 ;;
        --parallel) PARALLEL="$2"; shift 2 ;;
        --help|-h) usage 0 ;;
        *) echo "unknown option: $1" >&2; usage 2 ;;
    esac
done

if ! [[ "$PARALLEL" =~ ^[0-9]+$ ]] || [[ "$PARALLEL" -lt 1 ]]; then
    echo "run_compat_testing.sh: --parallel must be ≥1 (got $PARALLEL)" >&2
    exit 2
fi
# Default seed: today's YYYYMMDD — same convention as
# run_testing.sh, so reruns within a day shuffle the same way
# but different days explore different orderings.
if [[ -z "$SEED" ]]; then
    SEED="$(date +%Y%m%d)"
fi
if ! [[ "$SEED" =~ ^[0-9]+$ ]]; then
    echo "run_compat_testing.sh: --seed must be numeric (got $SEED)" >&2
    exit 2
fi

if [[ -z "$REPORT_DIR" ]]; then
    REPORT_DIR="./test-runs/compat-$(date -u +%Y%m%d-%H%M%S)"
fi
mkdir -p "$REPORT_DIR"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

green()  { printf '\033[32m✓\033[0m %s\n' "$*"; }
note()   { printf '\033[36m●\033[0m %s\n' "$*"; }
red()    { printf '\033[31m✗\033[0m %s\n' "$*"; }

# ---------- 1. Define the matrix -----------------------------------
# Each entry: "os_image|pg_version|label" — pg_version is opaque
# to the compat_archive step (no PG bring-up happens) but the
# scenario's topology requires one, and operators reading the
# report want to know what version was nominally targeted.
#
# Image picks rationale:
#   - debian:12        — current Debian stable; PGDG default target.
#   - ubuntu:24.04     — current Ubuntu LTS.
#   - rockylinux:9     — current RHEL family release; tests RHEL
#                        glibc + dnf/rpm-installed locale data.
#   - opensuse/leap:15 — SUSE family coverage; older glibc.
#   - alpine:3.20      — musl variant, exposes any glibc-only assumptions.
#   - archlinux:latest — rolling-release; catches breakage against
#                        glibc/tooling tip before it hits LTS distros.
case "$MATRIX" in
    smoke)
        CELLS=(
            "debian:12|17|debian-12-pg17"
            "rockylinux:9|17|rocky-9-pg17"
        )
        ;;
    default)
        CELLS=(
            "debian:12|17|debian-12-pg17"
            "ubuntu:24.04|17|ubuntu-2404-pg17"
            "rockylinux:9|17|rocky-9-pg17"
            "opensuse/leap:15|16|opensuse-leap15-pg16"
            "alpine:3.20|17|alpine-320-pg17"
            "archlinux:latest|18|archlinux-pg18"
        )
        ;;
    wide)
        CELLS=(
            "debian:12|15|debian-12-pg15"
            "debian:12|17|debian-12-pg17"
            "debian:13|17|debian-13-pg17"
            "ubuntu:22.04|16|ubuntu-2204-pg16"
            "ubuntu:24.04|17|ubuntu-2404-pg17"
            "rockylinux:9|15|rocky-9-pg15"
            "rockylinux:9|17|rocky-9-pg17"
            "rockylinux:9|18|rocky-9-pg18"
            "fedora:40|17|fedora-40-pg17"
            "opensuse/leap:15|15|opensuse-leap15-pg15"
            "opensuse/leap:15|16|opensuse-leap15-pg16"
            "alpine:3.20|17|alpine-320-pg17"
            "archlinux:latest|18|archlinux-pg18"
        )
        ;;
    *)
        echo "run_compat_testing.sh: --matrix must be one of {smoke|default|wide} (got $MATRIX)" >&2
        exit 2
        ;;
esac

# ---------- 2. Build binaries --------------------------------------
if [[ "$NO_BUILD" -eq 0 ]]; then
    note "Building binaries (--no-build to skip)"
    make build
    make build-compat
    make build-testkit
    green "binaries built"
fi

NATIVE_BIN="$REPO_ROOT/bin/pg_hardstorage"
TESTKIT_BIN="$REPO_ROOT/bin/pg_hardstorage_testkit"
PGBR_BIN="$REPO_ROOT/bin/pg-hardstorage-pgbackrest"
BARMAN_WAL_BIN="$REPO_ROOT/bin/pg-hardstorage-barman-wal-archive"
WALG_BIN="$REPO_ROOT/bin/pg-hardstorage-walg"

for bin in "$NATIVE_BIN" "$TESTKIT_BIN" "$PGBR_BIN" "$BARMAN_WAL_BIN" "$WALG_BIN"; do
    if [[ ! -x "$bin" ]]; then
        echo "run_compat_testing.sh: $bin not found; remove --no-build or run \`make build build-compat build-testkit\`" >&2
        exit 1
    fi
done

# Export binary paths so the testkit's resolver picks them up.
export PG_HARDSTORAGE_BIN="$NATIVE_BIN"
export PG_HARDSTORAGE_PGBACKREST_BIN="$PGBR_BIN"
export PG_HARDSTORAGE_BARMAN_WAL_ARCHIVE_BIN="$BARMAN_WAL_BIN"
export PG_HARDSTORAGE_BARMAN_BIN="$REPO_ROOT/bin/pg-hardstorage-barman"
export PG_HARDSTORAGE_WALG_BIN="$WALG_BIN"

# ---------- 3. Pre-pull the matrix images -------------------------
# Pull serially so a flaky registry doesn't fan out into parallel
# error noise.  Re-pulls are no-ops once the local layer cache is
# warm; first runs of `--matrix wide` may take a few minutes.
note "Pre-pulling docker images for ${#CELLS[@]} cells"
for cell in "${CELLS[@]}"; do
    IFS='|' read -r image _pg _label <<<"$cell"
    if ! docker image inspect "$image" >/dev/null 2>&1; then
        note "  pulling $image"
        docker pull -q "$image"
    fi
done
green "images ready"

# ---------- 4. Pick the scenarios to run --------------------------
# Cross product of {shim} × {coverage}.  Coverage axes:
#
#   file — file:// repos under artefactDir (no sink).  Smallest
#          surface; catches dispatch / argv-translation bugs.
#   s3   — MinIO over plain HTTP.  Adds the S3 storage plugin
#          path + AWS SDK creds-via-env wiring.
#   tls  — MinIO over HTTPS with a self-signed cert.  Adds
#          AWS_CA_BUNDLE plumbing + TLS handshake exercising
#          glibc/libssl on every os_image cell.
declare -a SCENARIO_FILES=()
IFS=',' read -r -a SHIMS <<<"$SHIM_LIST"
IFS=',' read -r -a COVERAGES <<<"$COVERAGE_LIST"

# Allowed shim × coverage combinations.  An "x" marks "scenario
# exists at test/scenarios/L2_compat_<shim><suffix>.scenario.yaml".
# Today we ship file for both shims, S3+TLS for pgbackrest, TLS
# for barman.  Plain S3-on-barman would be near-duplicate of TLS
# coverage (same code path bar the scheme), so it's omitted.
for shim in "${SHIMS[@]}"; do
    for cov in "${COVERAGES[@]}"; do
        case "$shim:$cov" in
            pgbackrest:file) SCENARIO_FILES+=("test/scenarios/L2_compat_pgbackrest.scenario.yaml") ;;
            pgbackrest:s3)   SCENARIO_FILES+=("test/scenarios/L2_compat_pgbackrest_s3.scenario.yaml") ;;
            pgbackrest:tls)  SCENARIO_FILES+=("test/scenarios/L2_compat_pgbackrest_tls.scenario.yaml") ;;
            barman:file)     SCENARIO_FILES+=("test/scenarios/L2_compat_barman.scenario.yaml") ;;
            barman:tls)      SCENARIO_FILES+=("test/scenarios/L2_compat_barman_tls.scenario.yaml") ;;
            barman:s3)
                note "skipping barman:s3 — TLS coverage already exercises the same code path with stricter trust"
                ;;
            walg:file)       SCENARIO_FILES+=("test/scenarios/L2_compat_walg.scenario.yaml") ;;
            walg:tls)        SCENARIO_FILES+=("test/scenarios/L2_compat_walg_tls.scenario.yaml") ;;
            walg:s3)
                note "skipping walg:s3 — TLS coverage already exercises the same env-var path with stricter trust"
                ;;
            pgbackrest:migration)
                # The migration story is shim-anchored on the
                # pgbackrest side (most operators we see migrate
                # FROM pgbackrest); no equivalent for barman /
                # walg today.  Quietly skip those combos.
                SCENARIO_FILES+=("test/scenarios/L3_compat_migration_pgbackrest_to_native_s3.scenario.yaml") ;;
            barman:migration|walg:migration)
                note "skipping $shim:migration — migration scenario is pgbackrest-anchored (no equivalent shape today)"
                ;;
            pgbackrest:doppelganger)
                # The doppelgänger scenario itself drives all
                # three shim variants (native + tls + pgbackrest)
                # in its own steps; we add it once under the
                # pgbackrest shim slot so it appears once per
                # cell, not three times.
                SCENARIO_FILES+=("test/scenarios/L4_doppelganger.scenario.yaml") ;;
            barman:doppelganger|walg:doppelganger)
                note "skipping $shim:doppelganger — single-scenario fan-out covers all shims"
                ;;
            *)
                echo "run_compat_testing.sh: invalid --shim/--coverage combo: $shim:$cov (got --shim=$SHIM_LIST --coverage=$COVERAGE_LIST)" >&2
                exit 2
                ;;
        esac
    done
done

if [[ ${#SCENARIO_FILES[@]} -eq 0 ]]; then
    echo "run_compat_testing.sh: --shim/--coverage produced no scenarios; nothing to do" >&2
    exit 2
fi

# ---------- 5. Run the matrix -------------------------------------
# When --parallel > 1, each cell still runs its scenario set
# serially inside its own worker (a single docker compose stack
# per cell prefers serial scenario invocations against itself —
# parallelising at the SCENARIO level inside one cell would
# clash on container names + ports).  Parallelism is at the
# CELL level: --parallel N means N cells are working through
# their scenarios concurrently.
#
# Each cell writes its per-scenario verdicts as
# PASS|label|scn|dir or FAIL|label|scn|dir lines into
# $TMP_RESULTS_DIR/<label>.results.  The aggregator scans
# those files at the end.

# Shuffle the cell list by SEED so --parallel N doesn't always
# start with the same N heaviest cells.  Uses awk's srand for
# portability — `shuf` is GNU-only and missing on macOS.
if [[ "${#CELLS[@]}" -gt 1 ]]; then
    mapfile -t CELLS < <(
        printf '%s\n' "${CELLS[@]}" | awk -v seed="$SEED" '
            BEGIN { srand(seed) }
            { lines[NR] = $0 }
            END {
                n = NR
                for (i = n; i > 1; i--) {
                    j = int(rand() * i) + 1
                    t = lines[i]; lines[i] = lines[j]; lines[j] = t
                }
                for (i = 1; i <= n; i++) print lines[i]
            }
        '
    )
fi

TOTAL=0
TMP_RESULTS_DIR="$REPORT_DIR/.tmp-results"
mkdir -p "$TMP_RESULTS_DIR"

# Count scenarios per cell so the summary can announce the
# total up front.  Scenarios is the same for every cell, so
# total = #cells * #scenarios.
TOTAL=$(( ${#CELLS[@]} * ${#SCENARIO_FILES[@]} ))

# run_one_cell takes "image|pgver|label" and runs every
# scenario in SCENARIO_FILES against it serially.  Writes one
# verdict line per scenario into $TMP_RESULTS_DIR/<label>.results.
run_one_cell() {
    local cell="$1"
    local image pgver label
    IFS='|' read -r image pgver label <<<"$cell"
    local cell_results_file="$TMP_RESULTS_DIR/$label.results"
    : > "$cell_results_file"

    for scn in "${SCENARIO_FILES[@]}"; do
        local scn_label="$(basename "$scn" .scenario.yaml)"
        local cell_dir="$REPORT_DIR/$label/$scn_label"
        mkdir -p "$cell_dir"
        local log_file="$cell_dir/run.log"

        note "[$label] $scn_label  →  os_image=$image pg=$pgver"

        # Patch the scenario YAML in place via a temp copy so we
        # can substitute os_image: per cell.  Each cell has its own
        # patched scenario file under cell_dir so artefacts are
        # self-contained.
        local patched_scn="$cell_dir/scenario.yaml"
        # The compat_archive step body reads st.OSImage from the
        # `os_image:` field; the source scenarios omit it (host
        # default).  We inject it via sed: any `compat_archive:`
        # mapping gets an os_image line appended.
        awk -v img="$image" '
            /^  - compat_archive:$/ { print; printf "      os_image: %s\n", img; next }
            { print }
        ' "$scn" > "$patched_scn"

        if "$TESTKIT_BIN" scenario run "$patched_scn" \
                --artefact-dir "$cell_dir" >"$log_file" 2>&1; then
            green "    PASS  $label / $scn_label"
            echo "PASS|$label|$scn_label|$cell_dir" >> "$cell_results_file"
            if [[ "$KEEP_ON_FAILURE" -eq 0 ]]; then
                find "$cell_dir" -mindepth 1 -maxdepth 1 \
                    ! -name 'run.log' \
                    ! -name 'result.json' \
                    ! -name 'scenario.yaml' \
                    -exec rm -rf {} + 2>/dev/null || true
            fi
        else
            red "    FAIL  $label / $scn_label  (see $log_file)"
            echo "FAIL|$label|$scn_label|$cell_dir" >> "$cell_results_file"
            tail -30 "$log_file" | sed 's/^/      /' >&2
        fi
    done
}

if [[ "$PARALLEL" -gt 1 ]]; then
    note "Running ${#CELLS[@]} cells with --parallel $PARALLEL (seed=$SEED)"
    declare -a JOB_PIDS=()
    in_flight=0
    for cell in "${CELLS[@]}"; do
        run_one_cell "$cell" &
        JOB_PIDS+=($!)
        in_flight=$((in_flight + 1))
        if [[ "$in_flight" -ge "$PARALLEL" ]]; then
            # Wait for the FIRST queued pid to free a slot.
            # Bash's `wait -n` would be cleaner but isn't on
            # macOS bash 3.2; using the explicit head-of-queue
            # wait stays portable.
            wait "${JOB_PIDS[0]}" || true
            JOB_PIDS=("${JOB_PIDS[@]:1}")
            in_flight=$((in_flight - 1))
        fi
    done
    for pid in "${JOB_PIDS[@]}"; do
        wait "$pid" || true
    done
else
    note "Running ${#CELLS[@]} cells serially (seed=$SEED)"
    for cell in "${CELLS[@]}"; do
        run_one_cell "$cell"
    done
fi

# ---------- Aggregate verdicts ------------------------------------
# Every cell wrote its per-scenario verdicts to
# $TMP_RESULTS_DIR/<label>.results during the run; collect them
# back into PASS / FAIL counts and a single CELL_RESULTS array
# for the markdown summary.
PASS=0
FAIL=0
declare -a CELL_RESULTS=()
for f in "$TMP_RESULTS_DIR"/*.results; do
    [[ -f "$f" ]] || continue
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        CELL_RESULTS+=("$line")
        case "$line" in
            PASS\|*) PASS=$((PASS + 1)) ;;
            FAIL\|*) FAIL=$((FAIL + 1)) ;;
        esac
    done < "$f"
done
rm -rf "$TMP_RESULTS_DIR"

# ---------- 6. Aggregate report -----------------------------------
SUMMARY="$REPORT_DIR/summary.md"
{
    echo "# Compat-shim multi-distro test run"
    echo ""
    echo "**Matrix:** $MATRIX (${#CELLS[@]} cells)"
    echo "**Shims:** $SHIM_LIST"
    echo "**Coverage:** $COVERAGE_LIST"
    echo "**Total:** $TOTAL run | **Passed:** $PASS | **Failed:** $FAIL"
    echo ""
    echo "| Verdict | Cell | Scenario | Artefacts |"
    echo "| --- | --- | --- | --- |"
    for r in "${CELL_RESULTS[@]}"; do
        IFS='|' read -r v c s d <<<"$r"
        echo "| $v | $c | $s | [$d](${d#$REPORT_DIR/}) |"
    done
    echo ""
    if [[ "$FAIL" -gt 0 ]]; then
        echo "## Failure logs"
        for r in "${CELL_RESULTS[@]}"; do
            IFS='|' read -r v c s d <<<"$r"
            if [[ "$v" == "FAIL" ]]; then
                echo "### $c / $s"
                echo '```'
                tail -50 "$d/run.log" 2>/dev/null || echo "(log missing)"
                echo '```'
            fi
        done
    fi
} > "$SUMMARY"

echo ""
green "summary at $SUMMARY"
echo "  $PASS / $TOTAL passed"

if [[ "$FAIL" -gt 0 ]]; then
    red "$FAIL cell(s) failed — see $SUMMARY"
    exit 1
fi
exit 0
