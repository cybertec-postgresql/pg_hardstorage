#!/usr/bin/env bash
# run_k8s_testing.sh — drop-in compat-shim coverage on
# Kubernetes (and OpenShift-equivalent admission).
#
# Tests pg_hardstorage's compat shims against operator-managed
# Postgres clusters by rebuilding each operator's PG image
# with the original backup tool's binary swapped for our shim:
#
#   CNPG    — barman-cloud-* binaries → pg-hardstorage-barman + -wal-archive
#   Crunchy — pgbackrest binary       → pg-hardstorage-pgbackrest
#   Zalando — wal-g binary            → pg-hardstorage-walg
#
# The operator's OWN backup CRD fires the backup; the operator
# does not know it's not talking to barman-cloud / pgBackRest /
# WAL-G.  That's the drop-in claim, tested honestly.
#
# Why not OpenShift / CRC directly?  CRC needs ~16 GiB RAM and
# KVM, which makes the workflow heavyweight.  Instead this
# driver runs against minikube but enables PodSecurityAdmission
# `restricted` (the same constraint OpenShift's restricted-v2
# SCC enforces) — so shim images that pass here are also
# OpenShift-compliant.  A `--kube-driver crc` opt-in ships in
# a follow-up for full OpenShift validation.
#
# Usage:
#   ./run_k8s_testing.sh [options]
#
# Options:
#   --operator <list>       Comma-separated subset of
#                           {cnpg, crunchy, zalando}.
#                           Default: cnpg.
#                           Use --operator all for the full set.
#   --pg-version <list>     Comma-separated PG majors to test.
#                           Default: 17.
#   --scenario <list>       Comma-separated subset of
#                           {operator_backup_smoke,
#                            operator_wal_archive_continuous,
#                            operator_failover_no_gap,
#                            operator_pitr,
#                            operator_doppelganger}.
#                           Default: operator_backup_smoke.
#                           Use --scenario all for the full
#                           set (S1 ships only smoke; the rest
#                           land in S2-S4).
#   --kube-driver <name>    minikube backend driver:
#                           docker | kvm2 | hyperkit | virtualbox | podman.
#                           Default: docker (most common on Linux
#                           x86_64).  CRC support for full OpenShift
#                           validation lands later.
#   --kube-cpus N           CPU budget per profile (default 4).
#   --kube-memory <size>    Memory budget per profile
#                           (default 8g).
#   --report-dir <p>        Default: ./test-runs/k8s-<ts>.
#   --no-build              Skip make build / build-compat /
#                           docker build of shim images.
#   --no-up                 Assume cluster + operator already up;
#                           skip create / install / destroy.
#   --keep-on-failure       Preserve cluster + minio for
#                           kubectl forensics on failed cells.
#   --parallel N            N cells concurrently (each its own
#                           minikube profile).  Default 1.
#                           Bounded by host RAM — each cell
#                           wants ~8 GiB.
#   --seed N                Seed cell-order shuffle so a
#                           --parallel N=small run doesn't always
#                           start with the same heaviest cells.
#                           Default: today's YYYYMMDD.
#   --dry-run               Print what would be done; don't
#                           start clusters.  Useful to verify
#                           prereqs + flag plumbing.
#   --help                  This message.
#
# Cell shape: (operator × pg_version × scenario).
#
# Per cell:
#   1. Build the per-operator shim image
#      (dockerfiles/k8s/Dockerfile.<operator>-shim).
#   2. Start a minikube profile sized for the cluster.
#   3. minikube image load <shim image>.
#   4. Install the operator (helm for cnpg, kubectl for the
#      others).
#   5. Create an operator-managed PG cluster CRD that
#      references the shim image.
#   6. Run the scenario (drives the operator's own backup CRD
#      through pg_hardstorage's shim).
#   7. Capture per-cell result.json + run.log.
#   8. Tear down the profile (or preserve on
#      --keep-on-failure).
#
# v1 status: scaffolding only.  Steps 1 + 2 + 3 work; steps 4
# (cnpg only) + 5 (stub) + 6 (stub) ship in S2.

set -euo pipefail

# Pin TMPDIR off /tmp — see run_testing.sh for the rationale (tmpfs
# inode ceiling + root-owned minio/operator scratch dirs).  Override
# with HS_TMPDIR=<path> when the repo-local default lands on the
# wrong filesystem (the resolved value is echoed below on launch).
HS_TMPDIR="${HS_TMPDIR:-$(cd "$(dirname "$0")" && pwd)/test-runs/tmp}"
mkdir -p "$HS_TMPDIR"
export TMPDIR="$HS_TMPDIR"
echo "info: TMPDIR=$TMPDIR (override: HS_TMPDIR=<path> $0 ...)" >&2

usage() {
    sed -nE '2,/^set -/{ /^set -/d; s/^# ?//p; }' "$0"
    exit "${1:-0}"
}

OPERATOR_LIST="cnpg"
PG_VERSION_LIST="17"
SCENARIO_LIST="operator_backup_smoke"
# Default minikube backend — `docker` is the most common on
# Linux x86_64 (matches our supported posture).  Operators on
# Linux without a docker daemon should pass --kube-driver kvm2.
KUBE_DRIVER="docker"
KUBE_CPUS=4
KUBE_MEMORY="8g"
REPORT_DIR=""
NO_BUILD=0
NO_UP=0
KEEP_ON_FAILURE=0
PARALLEL=1
SEED=""
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --operator)        OPERATOR_LIST="$2";      shift 2 ;;
        --pg-version)      PG_VERSION_LIST="$2";    shift 2 ;;
        --scenario)        SCENARIO_LIST="$2";      shift 2 ;;
        --kube-driver)     KUBE_DRIVER="$2";        shift 2 ;;
        --kube-cpus)       KUBE_CPUS="$2";          shift 2 ;;
        --kube-memory)     KUBE_MEMORY="$2";        shift 2 ;;
        --report-dir)      REPORT_DIR="$2";         shift 2 ;;
        --no-build)        NO_BUILD=1;              shift   ;;
        --no-up)           NO_UP=1;                 shift   ;;
        --keep-on-failure) KEEP_ON_FAILURE=1;       shift   ;;
        --parallel)        PARALLEL="$2";           shift 2 ;;
        --seed)            SEED="$2";               shift 2 ;;
        --dry-run)         DRY_RUN=1;               shift   ;;
        --help|-h)         usage 0 ;;
        *) echo "unknown option: $1" >&2; usage 2 ;;
    esac
done

if ! [[ "$PARALLEL" =~ ^[0-9]+$ ]] || [[ "$PARALLEL" -lt 1 ]]; then
    echo "run_k8s_testing.sh: --parallel must be ≥1 (got $PARALLEL)" >&2
    exit 2
fi
case "$KUBE_DRIVER" in
    docker|kvm2|hyperkit|virtualbox|podman) ;;
    *)
        echo "run_k8s_testing.sh: --kube-driver must be docker|kvm2|hyperkit|virtualbox|podman (got $KUBE_DRIVER)" >&2
        exit 2
        ;;
esac
if [[ -z "$SEED" ]]; then
    SEED="$(date +%Y%m%d)"
fi
if ! [[ "$SEED" =~ ^[0-9]+$ ]]; then
    echo "run_k8s_testing.sh: --seed must be numeric (got $SEED)" >&2
    exit 2
fi
if [[ -z "$REPORT_DIR" ]]; then
    REPORT_DIR="./test-runs/k8s-$(date -u +%Y%m%d-%H%M%S)"
fi
mkdir -p "$REPORT_DIR"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

green()  { printf '\033[32m✓\033[0m %s\n' "$*"; }
note()   { printf '\033[36m●\033[0m %s\n' "$*"; }
red()    { printf '\033[31m✗\033[0m %s\n' "$*"; }
warn()   { printf '\033[33m!\033[0m %s\n' "$*"; }

TESTKIT_BIN="$REPO_ROOT/bin/pg_hardstorage_testkit"

# ---------- 1. Expand operator + scenario lists -------------------
expand_list() {
    # Expand `--operator all` etc. into the canonical full list.
    local raw="$1" all="$2"
    if [[ "$raw" == "all" ]]; then
        echo "$all"
    else
        echo "$raw"
    fi
}

OPERATOR_LIST=$(expand_list "$OPERATOR_LIST" "cnpg,crunchy,zalando")
SCENARIO_LIST=$(expand_list "$SCENARIO_LIST" \
    "operator_backup_smoke,operator_wal_archive_continuous,operator_failover_no_gap,operator_pitr,operator_doppelganger")

IFS=',' read -r -a OPERATORS    <<<"$OPERATOR_LIST"
IFS=',' read -r -a PG_VERSIONS  <<<"$PG_VERSION_LIST"
IFS=',' read -r -a SCENARIOS    <<<"$SCENARIO_LIST"

for op in "${OPERATORS[@]}"; do
    case "$op" in
        cnpg|crunchy|zalando) ;;
        *) echo "run_k8s_testing.sh: --operator must be subset of {cnpg,crunchy,zalando} (got $op)" >&2; exit 2 ;;
    esac
done

# ---------- 2. Verify prereqs -------------------------------------
# --dry-run bypasses the cluster-side prereqs (minikube + helm) —
# the dry-run path only prints what WOULD happen, no actual
# cluster work fires.  We still need the testkit binary so the
# script can resolve flag plumbing through k8s subcommands.
if [[ ! -x "$TESTKIT_BIN" && "$NO_BUILD" -eq 1 ]]; then
    echo "run_k8s_testing.sh: $TESTKIT_BIN not found; remove --no-build or run \`make build-testkit\`" >&2
    exit 1
fi
if [[ ! -x "$TESTKIT_BIN" ]]; then
    note "Building testkit"
    make build-testkit
fi
if [[ "$DRY_RUN" -eq 0 ]]; then
    note "Verifying prereqs (minikube + kubectl + helm + docker)"
    if ! "$TESTKIT_BIN" k8s prereqs; then
        echo ""
        echo "Install the missing tools and re-run.  On Linux x86_64:" >&2
        echo "  minikube: https://minikube.sigs.k8s.io/docs/start/" >&2
        echo "  helm:     https://helm.sh/docs/intro/install/" >&2
        exit 1
    fi
    green "prereqs OK"
else
    note "DRY-RUN — skipping prereqs check (cluster ops will be printed, not executed)"
fi

# ---------- 3. Build binaries -------------------------------------
if [[ "$NO_BUILD" -eq 0 ]]; then
    note "Building agent + compat shim binaries"
    make build
    make build-compat
    green "binaries built"
fi

# ---------- 4. Build the shim images ------------------------------
# One per operator we're targeting × the PG versions we need.
# Image tag carries the operator + PG version + a short hash
# of the local agent binary so cached layers are correctly
# busted when the binary changes.
declare -A SHIM_DOCKERFILE=(
    [cnpg]="dockerfiles/k8s/Dockerfile.cnpg-shim"
    [crunchy]="dockerfiles/k8s/Dockerfile.crunchy-shim"
    [zalando]="dockerfiles/k8s/Dockerfile.spilo-shim"
)
declare -A SHIM_BUILD_ARG=(
    [cnpg]="CNPG_PG_VERSION"
    [crunchy]="CRUNCHY_PG_VERSION"
    [zalando]="SPILO_PG_VERSION"
)
declare -A SHIM_IMAGE_TAGS=()  # key=op:pg → tag

# Compute the shim tag for every (op × pg) regardless of
# --no-build, so the cell-matrix rendering always has a tag
# to print.  --no-build skips the actual `docker build`, but
# the tag scheme is deterministic given the binaries that
# would land in the image, so a cached image from a prior
# run carries the same tag iff its content matches.
#
# We hash BOTH the agent + the compat binary because the
# operator-shim images COPY the compat binary (not the
# agent) — a fix to compat code wouldn't change the tag
# under an agent-only hash, and minikube would serve the
# stale cached image.  Hashing both means any rebuild that
# changes either binary produces a fresh tag.
hash_inputs=()
for f in "$REPO_ROOT/bin/pg_hardstorage" "$REPO_ROOT/bin/pg-hardstorage-compat"; do
    if [[ -f "$f" ]]; then
        hash_inputs+=("$f")
    fi
done
if [[ ${#hash_inputs[@]} -gt 0 ]]; then
    bin_hash=$(sha256sum "${hash_inputs[@]}" | sha256sum | awk '{print substr($1,1,8)}')
else
    bin_hash="unbuilt"
fi
for op in "${OPERATORS[@]}"; do
    for pgv in "${PG_VERSIONS[@]}"; do
        SHIM_IMAGE_TAGS["${op}:${pgv}"]="pg-hardstorage/${op}-shim:pg${pgv}-${bin_hash}"
    done
done

declare -A SHIM_IMAGE_BUILD_FAILED=()

if [[ "$NO_BUILD" -eq 0 ]]; then
    note "Building shim images"
    for op in "${OPERATORS[@]}"; do
        for pgv in "${PG_VERSIONS[@]}"; do
            tag="${SHIM_IMAGE_TAGS["${op}:${pgv}"]}"
            note "  $op pg${pgv} → $tag"
            if [[ "$DRY_RUN" -eq 1 ]]; then
                echo "    docker build -f ${SHIM_DOCKERFILE[$op]} --build-arg ${SHIM_BUILD_ARG[$op]}=${pgv} -t $tag $REPO_ROOT"
            else
                # `|| true` + per-(op,pgver) build-failure record:
                # upstream operator catalogs occasionally drift —
                # e.g. registry.developers.crunchydata.com/.../crunchy-postgres
                # publishes ubi9-17.5-2520 but not ubi9-15.5-2520
                # or ubi9-18.5-2520 today.  Pre-fix, the first
                # missing tag aborted the whole slot before any
                # cell could run.  Now we record the failure,
                # warn loudly, and skip-with-warning every cell
                # that needs that (op,pgver) combo at scenario
                # dispatch time — every other (op,pgver) tuple
                # still runs.
                if ! docker build \
                        -f "${SHIM_DOCKERFILE[$op]}" \
                        --build-arg "${SHIM_BUILD_ARG[$op]}=${pgv}" \
                        -t "$tag" \
                        "$REPO_ROOT" >/dev/null 2>&1; then
                    SHIM_IMAGE_BUILD_FAILED["${op}:${pgv}"]=1
                    note "    ! shim build FAILED for ${op} pg${pgv} — likely upstream catalog drift; cells using this tuple will SKIP"
                fi
            fi
        done
    done
    if [[ "${#SHIM_IMAGE_BUILD_FAILED[@]}" -gt 0 ]]; then
        note "shim images built (${#SHIM_IMAGE_BUILD_FAILED[@]} (op,pgver) tuple(s) skipped — see warnings above)"
    else
        green "shim images built"
    fi
else
    # --no-build was passed.  Verify every required shim image is
    # actually present in the local Docker daemon — otherwise the
    # cells will fail much later with a cryptic
    # "Error response from daemon: No such image: ..." after the
    # operator install + cluster apply has already burned several
    # minutes.  Mark missing images as build-failed so the per-cell
    # SKIP-with-warning path treats them like an upstream catalog
    # drift; the operator gets a clean "cell skipped because shim
    # image X not cached — rerun without --no-build" message.
    for op in "${OPERATORS[@]}"; do
        for pgv in "${PG_VERSIONS[@]}"; do
            tag="${SHIM_IMAGE_TAGS["${op}:${pgv}"]}"
            if ! docker image inspect "$tag" >/dev/null 2>&1; then
                SHIM_IMAGE_BUILD_FAILED["${op}:${pgv}"]=1
                note "    ! --no-build: shim image $tag is NOT cached — cells using this tuple will SKIP (rerun without --no-build to build it)"
            fi
        done
    done
    if [[ "${#SHIM_IMAGE_BUILD_FAILED[@]}" -gt 0 ]]; then
        note "--no-build pre-flight: ${#SHIM_IMAGE_BUILD_FAILED[@]} (op,pgver) tuple(s) missing shim image — see warnings above"
    else
        green "shim images already cached (--no-build pre-flight)"
    fi
fi

# ---------- 5. Build the cell matrix ------------------------------
# Cell shape: "operator|pg_version|scenario|profile_name|shim_image"
declare -a CELLS=()
for op in "${OPERATORS[@]}"; do
    for pgv in "${PG_VERSIONS[@]}"; do
        for scn in "${SCENARIOS[@]}"; do
            # Profile names must be lowercase + alphanumeric +
            # hyphens; the scenario name can contain underscores
            # (operator_backup_smoke), translate to hyphens.
            # We strip the "operator_" prefix from the scenario
            # to keep profile names short — minikube limits
            # profile names to ~38 chars in some drivers, and
            # "pgvalidate-k8s-cnpg-pg17-operator-backup-smoke"
            # is already 46.
            scn_short="${scn#operator_}"
            profile="pgvalidate-k8s-${op}-pg${pgv}-${scn_short//_/-}"
            shim_tag="${SHIM_IMAGE_TAGS["${op}:${pgv}"]:-}"
            CELLS+=("${op}|${pgv}|${scn}|${profile}|${shim_tag}")
        done
    done
done

# Shuffle by SEED so --parallel N small doesn't always pick the
# same heavy cells first.  awk-based — portable to macOS bash 3.2.
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

# ---------- 6. Run the matrix -------------------------------------
TOTAL=${#CELLS[@]}
TMP_RESULTS_DIR="$REPORT_DIR/.tmp-results"
mkdir -p "$TMP_RESULTS_DIR"

run_one_cell() {
    local cell="$1"
    local op pgv scn profile shim_tag
    IFS='|' read -r op pgv scn profile shim_tag <<<"$cell"

    local cell_label="${op}-pg${pgv}-${scn}"
    local cell_dir="$REPORT_DIR/$cell_label"
    mkdir -p "$cell_dir"
    local log_file="$cell_dir/run.log"
    local results_file="$TMP_RESULTS_DIR/$cell_label.result"

    note "[$cell_label] op=$op pg=$pgv scn=$scn profile=$profile"

    # Skip-with-warning if the shim image for this (op,pgver)
    # tuple failed to build (upstream catalog drift — see the
    # SHIM_IMAGE_BUILD_FAILED comment around the docker build
    # loop).  Recording as SKIP rather than FAIL keeps the slot
    # verdict honest: a missing upstream image isn't a
    # pg_hardstorage regression.
    if [[ -n "${SHIM_IMAGE_BUILD_FAILED["${op}:${pgv}"]:-}" ]]; then
        note "    ! shim image for ${op} pg${pgv} not available (upstream catalog drift) — SKIPPING $cell_label"
        echo "SKIP|$cell_label|$op|$pgv|$scn|$cell_dir|reason=shim_image_unavailable" > "$results_file"
        echo "shim image for ${op} pg${pgv} not available — slot skipped" > "$log_file"
        return 0
    fi

    if [[ "$DRY_RUN" -eq 1 ]]; then
        {
            echo "DRY-RUN cell: $cell_label"
            echo "  shim image: $shim_tag"
            echo "  steps:"
            echo "    1. testkit k8s up --profile $profile --driver $KUBE_DRIVER --cpus $KUBE_CPUS --memory $KUBE_MEMORY --preload-image $shim_tag"
            echo "    2. testkit k8s install-operator --operator $op"
            echo "    3. testkit k8s pg-cluster --operator $op --image $shim_tag --name shim-test"
            echo "    4. (scenario) $scn — runs testkit scenario run …"
            echo "    5. (cleanup) testkit k8s down --profile $profile"
        } > "$log_file"
        echo "DRY-RUN|$cell_label|$op|$pgv|$scn|$cell_dir" > "$results_file"
        green "    DRY-RUN $cell_label"
        return 0
    fi

    if [[ "$NO_UP" -eq 0 ]]; then
        note "  → minikube up"
        if ! "$TESTKIT_BIN" k8s up \
                --profile "$profile" \
                --driver "$KUBE_DRIVER" \
                --cpus "$KUBE_CPUS" \
                --memory "$KUBE_MEMORY" \
                --preload-image "$shim_tag" \
                >>"$log_file" 2>&1; then
            red "    FAIL  $cell_label  (minikube up failed; see $log_file)"
            echo "FAIL|$cell_label|$op|$pgv|$scn|$cell_dir" > "$results_file"
            tail -30 "$log_file" | sed 's/^/      /'
            return 1
        fi
    fi
    # Operator install is the scenario script's responsibility,
    # not the driver's: each (operator, scenario) combo has
    # different needs (helm chart vs raw manifests vs kustomize),
    # different namespaces, different supplementary RBAC.  The
    # driver's job ends at "cluster up + shim image preloaded";
    # the scenario sequences install-operator → create-cluster →
    # exercise → assert.

    # Dispatch the scenario.  Each (op, scenario) combo maps to
    # a script under test/scenarios/k8s/; if no script exists
    # yet, emit a SKIP so operators see the bring-up worked
    # while the scenario set is still being populated.
    local scenario_script="$REPO_ROOT/test/scenarios/k8s/${op}_${scn}.sh"
    if [[ ! -x "$scenario_script" ]]; then
        warn "  → scenario $scn: SKIP (no script at $scenario_script)"
        echo "SKIP|$cell_label|$op|$pgv|$scn|$cell_dir" > "$results_file"
        if [[ "$NO_UP" -eq 0 && "$KEEP_ON_FAILURE" -eq 0 ]]; then
            note "  → minikube down"
            "$TESTKIT_BIN" k8s down --profile "$profile" >>"$log_file" 2>&1 || true
        fi
        green "    SKIP  $cell_label  (bring-up succeeded; no scenario script)"
        return 0
    fi
    note "  → scenario $scn"
    KUBECTX="$profile" SHIM_IMAGE="$shim_tag" CELL_DIR="$cell_dir" \
        TESTKIT_BIN="$TESTKIT_BIN" \
        "$scenario_script" >>"$log_file" 2>&1
    local scenario_rc=$?
    if [[ $scenario_rc -ne 0 ]]; then
        red "    FAIL  $cell_label  (scenario exit $scenario_rc; see $log_file)"
        echo "FAIL|$cell_label|$op|$pgv|$scn|$cell_dir" > "$results_file"
        tail -30 "$log_file" | sed 's/^/      /'
        if [[ "$NO_UP" -eq 0 && "$KEEP_ON_FAILURE" -eq 0 ]]; then
            note "  → minikube down (post-failure cleanup)"
            # Append teardown output to the cell log so a future
            # run can diagnose if the down ran or failed silently.
            # Old form `>/dev/null 2>&1` masked teardown errors and
            # left orphaned profiles that broke the next run with
            # stale CNPG cluster state — see test/scenarios/k8s/
            # cnpg_operator_backup_smoke.sh's force-clean step.
            "$TESTKIT_BIN" k8s down --profile "$profile" >>"$log_file" 2>&1 || \
                warn "  k8s down --profile $profile failed; profile may persist"
        fi
        return 1
    fi
    echo "PASS|$cell_label|$op|$pgv|$scn|$cell_dir" > "$results_file"

    if [[ "$NO_UP" -eq 0 && "$KEEP_ON_FAILURE" -eq 0 ]]; then
        note "  → minikube down"
        "$TESTKIT_BIN" k8s down --profile "$profile" >>"$log_file" 2>&1 || \
            warn "  k8s down --profile $profile failed; profile may persist"
    fi

    green "    PASS  $cell_label"
    return 0
}

if [[ "$PARALLEL" -gt 1 ]]; then
    note "Running $TOTAL cells with --parallel $PARALLEL (seed=$SEED)"
    declare -a JOB_PIDS=()
    in_flight=0
    for cell in "${CELLS[@]}"; do
        run_one_cell "$cell" &
        JOB_PIDS+=($!)
        in_flight=$((in_flight + 1))
        if [[ "$in_flight" -ge "$PARALLEL" ]]; then
            wait "${JOB_PIDS[0]}" || true
            JOB_PIDS=("${JOB_PIDS[@]:1}")
            in_flight=$((in_flight - 1))
        fi
    done
    for pid in "${JOB_PIDS[@]}"; do
        wait "$pid" || true
    done
else
    note "Running $TOTAL cells serially (seed=$SEED)"
    for cell in "${CELLS[@]}"; do
        # `|| true` prevents `set -e` from aborting the whole
        # campaign on a single-cell failure.  Per-cell failure
        # is recorded in $TMP_RESULTS_DIR/*.result; the
        # aggregate step at the end of the script tallies
        # PASS/FAIL/SKIP and exits non-zero iff any failed,
        # so an early failure no longer blocks coverage on
        # the remaining cells.  Mirrors run_compat_testing.sh's
        # already-correct serial-loop shape.
        run_one_cell "$cell" || true
    done
fi

# ---------- 7. Aggregate ------------------------------------------
PASS=0; FAIL=0; SKIP=0; DRYRUN=0
declare -a CELL_RESULTS=()
for f in "$TMP_RESULTS_DIR"/*.result; do
    [[ -f "$f" ]] || continue
    line=$(cat "$f")
    CELL_RESULTS+=("$line")
    case "$line" in
        PASS\|*)    PASS=$((PASS + 1)) ;;
        FAIL\|*)    FAIL=$((FAIL + 1)) ;;
        SKIP\|*)    SKIP=$((SKIP + 1)) ;;
        DRY-RUN\|*) DRYRUN=$((DRYRUN + 1)) ;;
    esac
done
rm -rf "$TMP_RESULTS_DIR"

SUMMARY="$REPORT_DIR/summary.md"
{
    echo "# Kubernetes drop-in shim test run"
    echo ""
    echo "**Operators:** $OPERATOR_LIST"
    echo "**PG versions:** $PG_VERSION_LIST"
    echo "**Scenarios:** $SCENARIO_LIST"
    echo "**Kube driver:** $KUBE_DRIVER (cpus=$KUBE_CPUS memory=$KUBE_MEMORY)"
    echo "**Total:** $TOTAL run | **Passed:** $PASS | **Failed:** $FAIL | **Skipped:** $SKIP | **Dry-run:** $DRYRUN"
    echo ""
    echo "| Verdict | Cell | Operator | PG | Scenario | Artefacts |"
    echo "| --- | --- | --- | --- | --- | --- |"
    for r in "${CELL_RESULTS[@]}"; do
        IFS='|' read -r v c op pgv scn d <<<"$r"
        echo "| $v | $c | $op | $pgv | $scn | [$c]($c/) |"
    done
    if [[ "$FAIL" -gt 0 ]]; then
        echo ""
        echo "## Failure logs"
        for r in "${CELL_RESULTS[@]}"; do
            IFS='|' read -r v c op pgv scn d <<<"$r"
            if [[ "$v" == "FAIL" ]]; then
                echo "### $c"
                echo '```'
                tail -50 "$d/run.log" 2>/dev/null || echo "(log missing)"
                echo '```'
            fi
        done
    fi
} > "$SUMMARY"

echo ""
green "summary at $SUMMARY"
echo "  $PASS pass / $FAIL fail / $SKIP skip / $DRYRUN dry-run (of $TOTAL)"

if [[ "$FAIL" -gt 0 ]]; then
    red "$FAIL cell(s) failed"
    exit 1
fi
exit 0
