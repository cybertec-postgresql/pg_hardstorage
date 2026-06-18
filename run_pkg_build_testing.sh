#!/usr/bin/env bash
# run_pkg_build_testing.sh — multi-distro driver that builds
# pg_hardstorage's native packages (.deb / .rpm / Arch
# PKGBUILD / FreeBSD port) and smoke-tests that they install +
# run cleanly on a fresh container of each target distro.
#
# Why a separate driver from run_compat_testing.sh?
# run_compat_testing exercises the COMPAT shim binaries against
# pgBackRest / Barman scenarios.  This driver exercises the
# PACKAGING surface — does `dpkg-buildpackage` on Debian 12
# produce a .deb?  Does `rpmbuild -bb` on Rocky 9 produce an
# .rpm?  Different question, different cells, different
# Dockerfiles.
#
# Each cell is one (family, distro_image) pair.  Per cell:
#   1. Build the per-family builder image
#      (dockerfiles/pkg-build/Dockerfile.<family>-builder).
#   2. `docker run` the builder with the project source bind-
#      mounted; the build produces .deb / .rpm / .pkg.tar.zst /
#      (FreeBSD: validates Makefile only).
#   3. Smoke-install the resulting package in a fresh,
#      otherwise-empty container of the SAME distro_image
#      (NOT the builder image — we want to exercise the package
#      against a clean rootfs the way an end user would).
#   4. Inside the consumer container, run `pg_hardstorage version`
#      and `pg_hardstorage doctor` (no-config doctor exits
#      non-zero today; we just assert it doesn't crash with
#      "command not found" / SIGSEGV).
#   5. Capture per-cell result.json + run.log under
#      <report-dir>/<cell>/.
#
# Usage:
#   ./run_pkg_build_testing.sh [options]
#
# Options:
#   --family <list>       Comma-separated subset of
#                         {deb,rpm-rhel,rpm-suse,arch,freebsd}.
#                         Default: deb,rpm-rhel,rpm-suse,arch
#                         (FreeBSD validates Makefile only and
#                         is opt-in via --family freebsd).
#   --matrix <preset>     "default" (one cell per family) |
#                         "wide" (multiple distro images per
#                         family).  Default: default.
#   --report-dir <p>      Output dir.  Default:
#                         ./test-runs/pkg-<ts>.
#   --no-build            Skip building the builder images
#                         (assume they're cached).
#   --keep-on-failure     Preserve build artefacts of failing
#                         cells.
#   --seed N              Reserved for future use; currently
#                         unused (cell order is deterministic).
#   --parallel N          Run N cells concurrently. Default 1.
#                         Each cell runs disjoint container
#                         names + per-cell artefact dirs, so
#                         parallelism is safe.
#   --help                This message.

set -euo pipefail

# Pin TMPDIR off /tmp — see run_testing.sh for the rationale (tmpfs
# inode ceiling + root-owned package-build scratch dirs).  Override
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

FAMILY_LIST="deb,rpm-rhel,rpm-suse,arch"
MATRIX="default"
REPORT_DIR=""
NO_BUILD=0
KEEP_ON_FAILURE=0
SEED=""
PARALLEL=1

while [[ $# -gt 0 ]]; do
    case "$1" in
        --family) FAMILY_LIST="$2"; shift 2 ;;
        --matrix) MATRIX="$2"; shift 2 ;;
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
    echo "run_pkg_build_testing.sh: --parallel must be ≥1 (got $PARALLEL)" >&2
    exit 2
fi

# Discard SEED for now — keeps the flag accepted (parity with
# the other drivers) without pretending we use it.
[[ -n "$SEED" ]] || true

if [[ -z "$REPORT_DIR" ]]; then
    REPORT_DIR="./test-runs/pkg-$(date -u +%Y%m%d-%H%M%S)"
fi
mkdir -p "$REPORT_DIR"
# Absolutise REPORT_DIR.  Why: every cell passes
# "$out_dir:/path" to `docker run -v`, and docker treats a
# relative path here as a NAMED VOLUME, not a bind mount —
# failing with "includes invalid characters for a local
# volume name" the moment a `/` shows up in the path.
# Resolve once, here, so every downstream `-v` gets an
# absolute host path.
REPORT_DIR="$(cd "$REPORT_DIR" && pwd)"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

green()  { printf '\033[32m✓\033[0m %s\n' "$*"; }
note()   { printf '\033[36m●\033[0m %s\n' "$*"; }
red()    { printf '\033[31m✗\033[0m %s\n' "$*"; }

# ---------- Define the cell matrix ---------------------------------
# Cell shape: "family|consumer_image|label"
#   family         — selects builder Dockerfile + per-family build cmd
#   consumer_image — fresh image used to install + smoke-test
#   label          — cell ID, becomes the report dir name
declare -a ALL_CELLS=()
case "$MATRIX" in
    default)
        ALL_CELLS+=(
            "deb|debian:12|deb-debian-12"
            "rpm-rhel|rockylinux:9|rpm-rocky-9"
            "rpm-suse|opensuse/leap:15|rpm-leap-15"
            "arch|archlinux:latest|arch-rolling"
            "freebsd|alpine:3.20|freebsd-validate"
        )
        ;;
    wide)
        ALL_CELLS+=(
            "deb|debian:12|deb-debian-12"
            "deb|ubuntu:24.04|deb-ubuntu-2404"
            "rpm-rhel|rockylinux:9|rpm-rocky-9"
            "rpm-rhel|fedora:40|rpm-fedora-40"
            "rpm-suse|opensuse/leap:15|rpm-leap-15"
            "arch|archlinux:latest|arch-rolling"
            "freebsd|alpine:3.20|freebsd-validate"
        )
        ;;
    *)
        echo "run_pkg_build_testing.sh: --matrix must be one of {default|wide} (got $MATRIX)" >&2
        exit 2
        ;;
esac

# Filter ALL_CELLS by the requested --family list.
IFS=',' read -r -a SELECTED_FAMILIES <<<"$FAMILY_LIST"
declare -a CELLS=()
for cell in "${ALL_CELLS[@]}"; do
    IFS='|' read -r fam _img _label <<<"$cell"
    for sf in "${SELECTED_FAMILIES[@]}"; do
        if [[ "$fam" == "$sf" ]]; then
            CELLS+=("$cell")
            break
        fi
    done
done
if [[ ${#CELLS[@]} -eq 0 ]]; then
    echo "run_pkg_build_testing.sh: --family=$FAMILY_LIST produced no cells (matrix=$MATRIX)" >&2
    exit 2
fi

# ---------- Build the agent binary once ---------------------------
# Every family Dockerfile expects bin/pg_hardstorage to exist
# in the bind-mounted source root.  Build it here so all cells
# share the same binary.
note "Building binaries (set --no-build to skip)"
if [[ "$NO_BUILD" -eq 0 ]]; then
    make build
    make build-compat || true
    green "binaries built"
fi

# ---------- Build the per-family builder images -------------------
# Cached aggressively: if the Dockerfile + base image hashes
# match, docker reuses layers.  --no-build does NOT skip this
# step — there's no way to install a package without the
# builder image.  --no-build skips ONLY the agent-binary build.
declare -A BUILDER_TAG=(
    [deb]="pg-hardstorage-pkg-build-deb:latest"
    [rpm-rhel]="pg-hardstorage-pkg-build-rpm-rhel:latest"
    [rpm-suse]="pg-hardstorage-pkg-build-rpm-suse:latest"
    [arch]="pg-hardstorage-pkg-build-arch:latest"
)
declare -A BUILDER_DOCKERFILE=(
    [deb]="dockerfiles/pkg-build/Dockerfile.deb-builder"
    [rpm-rhel]="dockerfiles/pkg-build/Dockerfile.rpm-rhel-builder"
    [rpm-suse]="dockerfiles/pkg-build/Dockerfile.rpm-suse-builder"
    [arch]="dockerfiles/pkg-build/Dockerfile.arch-builder"
)
declare -A NEEDED_FAMILIES=()
for cell in "${CELLS[@]}"; do
    IFS='|' read -r fam _ _ <<<"$cell"
    NEEDED_FAMILIES["$fam"]=1
done
for fam in "${!NEEDED_FAMILIES[@]}"; do
    [[ "$fam" == "freebsd" ]] && continue   # validation-only, no builder needed
    note "Building builder image for $fam"
    docker build \
        -f "${BUILDER_DOCKERFILE[$fam]}" \
        -t "${BUILDER_TAG[$fam]}" \
        "$REPO_ROOT" >/dev/null
    green "  ${BUILDER_TAG[$fam]}"
done

# ---------- Per-family build + smoke-install steps ----------------
# Each function takes (consumer_image, cell_dir) and exits 0 on
# success.  The driver loops over CELLS and dispatches by family.

build_and_install_deb() {
    local consumer_image="$1"
    local cell_dir="$2"
    local out_dir="$cell_dir/out"
    mkdir -p "$out_dir"
    # Build the .deb inside the deb-builder.  --build invokes
    # `dpkg-buildpackage -us -uc -b` inside the bind-mounted
    # source.  The resulting .deb files land in the parent
    # directory (per debian convention); we move them to /out.
    # `:z` on the bind-mounts: SELinux-labelled hosts (Fedora,
    # RHEL, CentOS) refuse cross-context reads/writes from inside
    # the container without a relabel.  `z` does a shared relabel,
    # which is fine here since these dirs are scoped to one cell.
    # Without it the build fails with `cp: cannot access '/src':
    # Permission denied`.
    docker run --rm \
        -v "$REPO_ROOT:/src:rw,z" \
        -v "$out_dir:/out:rw,z" \
        -w /src \
        "${BUILDER_TAG[deb]}" \
        bash -c '
            set -euo pipefail
            # dpkg-buildpackage writes ../*.deb relative to /src,
            # which lands at /, breaking the bind-mount.  Build
            # in a copy so the artefacts can be moved cleanly.
            #
            # Stage with `rsync --exclude` instead of `cp -r` so we
            # do NOT drag test-runs/ (gigabytes of soak artefacts),
            # .git/, bin/, /tmp scratch, or stray vendor caches into
            # the build copy.  The cp -r form took 5+ minutes and
            # turned a 50 MB source tree into a 30+ GB working copy
            # on hosts with active soak history.
            rsync -a \
                --exclude=test-runs \
                --exclude=.git \
                --exclude=bin \
                --exclude=node_modules \
                --exclude=tmp \
                --exclude=*.log \
                --exclude=coverage.out \
                --exclude=coverage.html \
                /src/ /work/
            cd /work
            # `-d` bypasses the apt-level Build-Depends check.
            # The deb-builder image installs Go 1.26 manually from
            # go.dev/dl/ (Debian 12 only ships Go 1.19), so the
            # apt package `golang-go (>= 2:1.26~)` is reported as
            # missing even though /usr/local/go/bin/go is present
            # and on PATH.  -d trusts the manual install and lets
            # the build proceed.
            dpkg-buildpackage -us -uc -b -d
            mv ../*.deb /out/ 2>/dev/null || true
            ls /out
        ' >/dev/null
    # Install in a fresh consumer container.  We run apt update
    # then install the .debs with `apt install ./` so missing
    # runtime deps get pulled.
    docker run --rm \
        -v "$out_dir:/pkgs:ro,z" \
        "$consumer_image" \
        bash -c '
            set -euo pipefail
            export DEBIAN_FRONTEND=noninteractive
            apt-get update >/dev/null
            apt-get install -y --no-install-recommends ca-certificates >/dev/null
            apt-get install -y --no-install-recommends /pkgs/*.deb
            pg_hardstorage version
            pg_hardstorage doctor || true   # no-config doctor exits non-zero by design
        '
}

build_and_install_rpm() {
    local family="$1"          # rpm-rhel | rpm-suse
    local consumer_image="$2"
    local cell_dir="$3"
    local out_dir="$cell_dir/out"
    mkdir -p "$out_dir"
    # Build the .rpm.  We tar the source into a tarball whose
    # name matches Source0 in the spec (pg_hardstorage-VERSION.tar.gz),
    # drop the spec into ~/rpmbuild/SPECS, and run rpmbuild -bb.
    docker run --rm \
        -v "$REPO_ROOT:/src:ro,z" \
        -v "$out_dir:/out:rw,z" \
        "${BUILDER_TAG[$family]}" \
        bash -c '
            set -euo pipefail
            # Match Version: in the spec.
            ver=$(awk "/^Version:/{print \$2}" /src/packaging/rpm/pg_hardstorage.spec)
            name=pg_hardstorage
            tarball="${name}-${ver}.tar.gz"
            # Stage the source in a tree shaped like %autosetup wants.
            # rsync with excludes (NOT cp -r) keeps test-runs/, .git/,
            # bin/, etc. out of the tarball — see the deb path for
            # the equivalent block and full rationale.  Without these
            # excludes the tar step turned a 50 MB source into a
            # multi-GB rpmbuild SOURCE on hosts with soak history.
            staging=$(mktemp -d)
            rsync -a \
                --exclude=test-runs \
                --exclude=.git \
                --exclude=bin \
                --exclude=node_modules \
                --exclude=tmp \
                --exclude=*.log \
                --exclude=coverage.out \
                --exclude=coverage.html \
                /src/ "${staging}/${name}-${ver}/"
            (cd "${staging}" && tar -czf "/root/rpmbuild/SOURCES/${tarball}" "${name}-${ver}")
            cp /src/packaging/rpm/pg_hardstorage.spec /root/rpmbuild/SPECS/
            # `--nodeps` skips BuildRequires checking.  Same rationale
            # as the deb path: Go 1.26 is installed manually from
            # go.dev/dl/ (RHEL/SUSE ship older Go), so the spec
            # `BuildRequires: golang >= 1.26` shows as missing even
            # though /usr/local/go/bin/go is on PATH.
            #
            # `--define _topdir /root/rpmbuild` is load-bearing on
            # openSUSE: the default _topdir there is
            # /usr/src/packages, but the staging block above writes
            # the source tarball + spec into /root/rpmbuild/{SOURCES,
            # SPECS}.  Without the override SUSE rpmbuild aborts with
            # "Bad source: /usr/src/packages/SOURCES/..: No such
            # file or directory".  RHEL is unaffected (default
            # _topdir is already ~/rpmbuild), so passing it
            # unconditionally is harmless on RHEL and required on
            # SUSE.
            rpmbuild -bb --nodeps \
                --define "_topdir /root/rpmbuild" \
                /root/rpmbuild/SPECS/pg_hardstorage.spec
            cp /root/rpmbuild/RPMS/*/*.rpm /out/
            ls /out
        ' >/dev/null
    # Install in a fresh consumer container.  RHEL uses dnf;
    # SUSE uses zypper.  Pick by family.
    if [[ "$family" == "rpm-rhel" ]]; then
        docker run --rm \
            -v "$out_dir:/pkgs:ro,z" \
            "$consumer_image" \
            bash -c '
                set -euo pipefail
                # --setopt=install_weak_deps=False: the spec uses
                # Recommends:postgresql which DNF would otherwise pull
                # from the AppStream modular repo.  On rockylinux:9 the
                # postgresql:18 module ships without complete modular
                # metadata, so the implicit weak-dep install fails with
                # "No available modular metadata for modular package".
                # Skipping weak deps here installs ONLY the .rpm we just
                # built — which is what the consumer-side smoke test
                # really wants to validate.
                dnf -y install --allowerasing \
                    --setopt=install_weak_deps=False \
                    /pkgs/*.rpm
                pg_hardstorage version
                pg_hardstorage doctor || true
            '
    else
        # The opensuse/leap consumer image ships without shadow-utils
        # (group/user-management binaries — useradd / groupadd), but
        # the spec lists Requires(pre): shadow-utils because we need
        # `groupadd pgbackup` in the %pre scriptlet.  zypper has the
        # package under the SUSE-canonical name `shadow`, so install
        # that first; the subsequent install of /pkgs/*.rpm then
        # satisfies its own Requires(pre) and runs %pre cleanly.
        docker run --rm \
            -v "$out_dir:/pkgs:ro,z" \
            "$consumer_image" \
            bash -c '
                set -euo pipefail
                zypper --non-interactive --no-gpg-checks install shadow
                zypper --non-interactive --no-gpg-checks install /pkgs/*.rpm
                pg_hardstorage version
                pg_hardstorage doctor || true
            '
    fi
}

build_and_install_arch() {
    local consumer_image="$1"
    local cell_dir="$2"
    local out_dir="$cell_dir/out"
    mkdir -p "$out_dir"
    # makepkg writes the .pkg.tar.zst alongside PKGBUILD.  We
    # copy the relevant files into a builder workspace, run
    # makepkg, and extract the artefact.
    docker run --rm \
        -v "$REPO_ROOT:/src:ro,z" \
        -v "$out_dir:/out:rw,z" \
        --user builder \
        "${BUILDER_TAG[arch]}" \
        bash -c '
            set -euo pipefail
            # rsync with excludes (NOT cp -r) — see the deb path
            # for the rationale (test-runs/, .git/, bin/ would
            # otherwise drag in gigabytes from soak history).
            sudo mkdir -p /home/builder/repo
            sudo rsync -a \
                --exclude=test-runs \
                --exclude=.git \
                --exclude=bin \
                --exclude=node_modules \
                --exclude=tmp \
                --exclude=*.log \
                --exclude=coverage.out \
                --exclude=coverage.html \
                /src/ /home/builder/repo/
            sudo chown -R builder:builder /home/builder/repo
            cd /home/builder/repo/packaging/arch
            # `--nodeps` skips dep check; Go 1.26 is from manual
            # install not pacman.  Same pattern as deb/rpm.
            makepkg -f --noconfirm --skipchecksums --nodeps
            sudo cp *.pkg.tar.zst /out/
            ls /out
        ' >/dev/null
    # Install in a fresh consumer container.
    docker run --rm \
        -v "$out_dir:/pkgs:ro,z" \
        "$consumer_image" \
        bash -c '
            set -euo pipefail
            pacman-key --init
            pacman-key --populate archlinux
            pacman -Syu --noconfirm --needed glibc
            pacman -U --noconfirm /pkgs/*.pkg.tar.zst
            pg_hardstorage version
            pg_hardstorage doctor || true
        '
}

validate_freebsd_port() {
    local cell_dir="$1"
    # FreeBSD ports cannot be built on Linux (bsd.port.mk
    # requires bmake + /usr/ports/Mk).  Validation-only: assert
    # that the port skeleton exists, the Makefile parses with
    # `awk` (sanity check on basic syntax), and the required
    # variables are set.
    local mk="$REPO_ROOT/packaging/freebsd/Makefile"
    local descr="$REPO_ROOT/packaging/freebsd/pkg-descr"
    {
        for f in "$mk" "$descr"; do
            [[ -f "$f" ]] || { echo "missing: $f" >&2; return 1; }
        done
        for var in PORTNAME DISTVERSION CATEGORIES MAINTAINER COMMENT WWW LICENSE; do
            if ! grep -qE "^${var}=" "$mk"; then
                echo "Makefile missing required variable: $var" >&2
                return 1
            fi
        done
        echo "ok"
    } > "$cell_dir/freebsd-validation.log"
}

# ---------- Run the matrix ----------------------------------------
TOTAL=${#CELLS[@]}
TMP_RESULTS_DIR="$REPORT_DIR/.tmp-results"
mkdir -p "$TMP_RESULTS_DIR"

run_cell() {
    local cell="$1"
    IFS='|' read -r family consumer_image label <<<"$cell"
    local cell_dir="$REPORT_DIR/$label"
    mkdir -p "$cell_dir"
    local log_file="$cell_dir/run.log"
    local rc=0
    note "  → $label  (family=$family consumer=$consumer_image)"
    {
        case "$family" in
            deb)        build_and_install_deb     "$consumer_image" "$cell_dir" ;;
            rpm-rhel)   build_and_install_rpm     "$family" "$consumer_image" "$cell_dir" ;;
            rpm-suse)   build_and_install_rpm     "$family" "$consumer_image" "$cell_dir" ;;
            arch)       build_and_install_arch    "$consumer_image" "$cell_dir" ;;
            freebsd)    validate_freebsd_port     "$cell_dir" ;;
            *) echo "unknown family: $family" >&2; rc=1 ;;
        esac
    } >"$log_file" 2>&1 || rc=$?
    if [[ $rc -eq 0 ]]; then
        green "    PASS  $label"
        echo "PASS|$label|$family|$consumer_image|$cell_dir" \
            > "$TMP_RESULTS_DIR/$label.result"
        if [[ "$KEEP_ON_FAILURE" -eq 0 && "$family" != "freebsd" ]]; then
            rm -rf "$cell_dir/out"  # keep run.log; drop the bulky package files
        fi
    else
        red   "    FAIL  $label  (rc=$rc; see $log_file)"
        echo "FAIL|$label|$family|$consumer_image|$cell_dir" \
            > "$TMP_RESULTS_DIR/$label.result"
        tail -30 "$log_file" | sed 's/^/      /'
    fi
}

if [[ "$PARALLEL" -gt 1 ]]; then
    note "Running $TOTAL cells with --parallel $PARALLEL"
    declare -a JOB_PIDS=()
    declare -a JOB_CELLS=()
    in_flight=0
    for cell in "${CELLS[@]}"; do
        run_cell "$cell" &
        JOB_PIDS+=($!)
        JOB_CELLS+=("$cell")
        in_flight=$((in_flight + 1))
        if [[ "$in_flight" -ge "$PARALLEL" ]]; then
            wait "${JOB_PIDS[0]}" || true
            JOB_PIDS=("${JOB_PIDS[@]:1}")
            JOB_CELLS=("${JOB_CELLS[@]:1}")
            in_flight=$((in_flight - 1))
        fi
    done
    for pid in "${JOB_PIDS[@]}"; do
        wait "$pid" || true
    done
else
    note "Running $TOTAL cells serially"
    for cell in "${CELLS[@]}"; do
        run_cell "$cell"
    done
fi

# ---------- Aggregate report --------------------------------------
PASS=0; FAIL=0
declare -a CELL_RESULTS=()
for f in "$TMP_RESULTS_DIR"/*.result; do
    [[ -f "$f" ]] || continue
    line=$(cat "$f")
    CELL_RESULTS+=("$line")
    case "$line" in
        PASS\|*) PASS=$((PASS + 1)) ;;
        FAIL\|*) FAIL=$((FAIL + 1)) ;;
    esac
done
rm -rf "$TMP_RESULTS_DIR"

SUMMARY="$REPORT_DIR/summary.md"
{
    echo "# Package-build multi-distro test run"
    echo ""
    echo "**Matrix:** $MATRIX (${#CELLS[@]} cells)"
    echo "**Families:** $FAMILY_LIST"
    echo "**Total:** $TOTAL run | **Passed:** $PASS | **Failed:** $FAIL"
    echo ""
    echo "| Verdict | Cell | Family | Consumer image | Artefacts |"
    echo "| --- | --- | --- | --- | --- |"
    for r in "${CELL_RESULTS[@]}"; do
        IFS='|' read -r v c f i d <<<"$r"
        echo "| $v | $c | $f | $i | [$c]($c/) |"
    done
    echo ""
    if [[ "$FAIL" -gt 0 ]]; then
        echo "## Failure logs"
        for r in "${CELL_RESULTS[@]}"; do
            IFS='|' read -r v c f i d <<<"$r"
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
echo "  $PASS / $TOTAL passed"

if [[ "$FAIL" -gt 0 ]]; then
    red "$FAIL cell(s) failed — see $SUMMARY"
    exit 1
fi
exit 0
