#!/usr/bin/env bash
#
# compile.sh — one-shot build script for pg_hardstorage.
#
# The Makefile is the canonical build interface; this script
# is a thin wrapper for users who want to clone-and-build
# without learning Make.  It does three things:
#
#   1. Verifies Go is installed and meets the minimum
#      version (1.26+).
#   2. Downloads module dependencies (go mod download).
#   3. Compiles the binary into ./bin/.
#
# Usage:
#
#   ./compile.sh                    # default static binary
#   ./compile.sh --testkit          # also build the testkit harness
#   ./compile.sh --fips             # FIPS variant (Linux/amd64 only)
#   ./compile.sh --pkcs11           # HSM variant (cgo + libpkcs11)
#   ./compile.sh --firecracker      # microVM verifier-sandbox variant
#   ./compile.sh --all              # default + testkit
#   ./compile.sh --help             # this help
#
# Environment overrides:
#
#   GO              path to the go binary (default: looked up on PATH)
#   GOOS, GOARCH    cross-compile target (default: native)
#   VERSION         version string stamped into the binary
#                   (default: `git describe`-derived)
#   BIN_DIR         output directory (default: ./bin)
#   CGO_ENABLED     0 (default) for static, 1 for cgo flavours
#
# The build produces:
#
#   bin/pg_hardstorage              (always)
#   bin/pg_hardstorage_testkit      (with --testkit / --all)
#   bin/pg_hardstorage-fips         (with --fips)
#   bin/pg_hardstorage-pkcs11       (with --pkcs11)
#   bin/pg_hardstorage-firecracker  (with --firecracker)
#
# Exit codes:
#   0  success
#   1  build failed
#   2  prerequisite check failed (Go missing / too old)
#   3  bad CLI arguments

set -euo pipefail

# -- pretty output ------------------------------------------------

if [ -t 1 ] && [ "${NO_COLOR:-}" != "1" ]; then
    ESC="$(printf '\033')"
    RESET="${ESC}[0m"
    BOLD="${ESC}[1m"
    RED="${ESC}[31m"
    GREEN="${ESC}[32m"
    YELLOW="${ESC}[33m"
    BLUE="${ESC}[34m"
else
    RESET="" BOLD="" RED="" GREEN="" YELLOW="" BLUE=""
fi

log()  { printf '%s>>%s %s\n' "${BOLD}${BLUE}" "${RESET}" "$*"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*"; }
warn() { printf '%s ⚠%s %s\n' "${YELLOW}" "${RESET}" "$*" >&2; }
err()  { printf '%s ✗%s %s\n' "${RED}" "${RESET}" "$*" >&2; }

# -- arg parsing --------------------------------------------------

print_help() {
    sed -n '3,32p' "$0" | sed -e 's|^# ||' -e 's|^#||'
}

BUILD_DEFAULT=1
BUILD_TESTKIT=0
BUILD_FIPS=0
BUILD_PKCS11=0
BUILD_FIRECRACKER=0

while [ $# -gt 0 ]; do
    case "$1" in
        --testkit)     BUILD_TESTKIT=1 ;;
        --fips)        BUILD_FIPS=1 ;;
        --pkcs11)      BUILD_PKCS11=1 ;;
        --firecracker) BUILD_FIRECRACKER=1 ;;
        --all)         BUILD_TESTKIT=1 ;;
        -h|--help)     print_help; exit 0 ;;
        *)
            err "unknown argument: $1"
            err "run with --help for usage"
            exit 3
            ;;
    esac
    shift
done

# -- prerequisites ------------------------------------------------

readonly GO_REQUIRED_MAJOR=1
readonly GO_REQUIRED_MINOR=26
GO_BIN="${GO:-go}"

if ! command -v "$GO_BIN" >/dev/null 2>&1; then
    err "go is not on PATH (or \$GO is unset)"
    err ""
    err "Install Go ${GO_REQUIRED_MAJOR}.${GO_REQUIRED_MINOR}+ from https://go.dev/dl/"
    err "or via your distro's package manager:"
    err "    Debian/Ubuntu:  apt install golang-${GO_REQUIRED_MAJOR}.${GO_REQUIRED_MINOR}"
    err "    Fedora/RHEL:    dnf install golang"
    err "    macOS Homebrew: brew install go"
    exit 2
fi

# Parse `go version` output: "go version go1.26.0 darwin/arm64".
GO_VERSION_RAW="$("$GO_BIN" version | awk '{print $3}')"
GO_VERSION="${GO_VERSION_RAW#go}"
GO_MAJOR="${GO_VERSION%%.*}"
GO_REST="${GO_VERSION#*.}"
GO_MINOR="${GO_REST%%.*}"

if [ "$GO_MAJOR" -lt "$GO_REQUIRED_MAJOR" ] \
   || { [ "$GO_MAJOR" -eq "$GO_REQUIRED_MAJOR" ] && [ "$GO_MINOR" -lt "$GO_REQUIRED_MINOR" ]; }; then
    err "Go ${GO_VERSION} is too old; ${GO_REQUIRED_MAJOR}.${GO_REQUIRED_MINOR}+ required"
    exit 2
fi

ok "go ${GO_VERSION} ($($GO_BIN env GOOS)/$($GO_BIN env GOARCH))"

# -- env defaults -------------------------------------------------

# We're at the repo root; this script can be run from anywhere.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
BIN_DIR="${BIN_DIR:-bin}"
CGO_ENABLED="${CGO_ENABLED:-0}"

readonly LDFLAGS_BASE="-s -w \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=${VERSION} \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=${COMMIT} \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=${DATE}"

readonly GOFLAGS_BASE="-trimpath"

mkdir -p "$BIN_DIR"

# -- 1. download deps --------------------------------------------

log "downloading module dependencies"
"$GO_BIN" mod download
ok "dependencies cached in $($GO_BIN env GOMODCACHE)"

# -- 2. build helpers --------------------------------------------

# build_one <out-name> <build-tags> <cgo-required> <package-path>
build_one() {
    local out="$1" tags="$2" needs_cgo="$3" pkg="$4"
    local cgo="$CGO_ENABLED"
    local extra_tags=""

    if [ "$needs_cgo" = "1" ]; then
        cgo=1
    fi
    if [ -n "$tags" ]; then
        extra_tags="-tags ${tags}"
    fi

    log "building ${out} (cgo=${cgo}${tags:+ tags=${tags}})"
    # shellcheck disable=SC2086
    CGO_ENABLED="$cgo" "$GO_BIN" build \
        ${GOFLAGS_BASE} \
        ${extra_tags} \
        -ldflags "${LDFLAGS_BASE}" \
        -o "${BIN_DIR}/${out}" \
        "${pkg}"
    ok "${BIN_DIR}/${out}"
}

# -- 3. build the requested flavours -----------------------------

if [ "$BUILD_DEFAULT" = "1" ]; then
    build_one pg_hardstorage "" 0 ./cmd/pg_hardstorage
fi

if [ "$BUILD_TESTKIT" = "1" ]; then
    build_one pg_hardstorage_testkit "" 0 ./cmd/pg_hardstorage_testkit
fi

if [ "$BUILD_FIPS" = "1" ]; then
    if [ "$($GO_BIN env GOOS)" != "linux" ] || [ "$($GO_BIN env GOARCH)" != "amd64" ]; then
        err "--fips requires linux/amd64 (Go's BoringCrypto experiment is platform-locked)"
        err "    current target: $($GO_BIN env GOOS)/$($GO_BIN env GOARCH)"
        exit 1
    fi
    log "FIPS: enabling GOEXPERIMENT=boringcrypto"
    GOEXPERIMENT=boringcrypto build_one pg_hardstorage-fips fips 1 ./cmd/pg_hardstorage
fi

if [ "$BUILD_PKCS11" = "1" ]; then
    # The pkcs11 build tag pulls in github.com/miekg/pkcs11
    # which needs the libpkcs11 / opensc / softhsm2 headers
    # at compile time.  We don't probe the headers here —
    # the go build step will surface a clear cgo error if
    # they're missing.
    warn "PKCS#11 build expects libpkcs11 headers (libltdl-dev on Debian, openssl-devel on RHEL)"
    build_one pg_hardstorage-pkcs11 pkcs11 1 ./cmd/pg_hardstorage
fi

if [ "$BUILD_FIRECRACKER" = "1" ]; then
    if [ "$($GO_BIN env GOOS)" != "linux" ]; then
        warn "--firecracker on non-Linux: the binary will compile but the microVM backend"
        warn "    only works at runtime on Linux + KVM"
    fi
    build_one pg_hardstorage-firecracker firecracker 0 ./cmd/pg_hardstorage
fi

# -- summary ------------------------------------------------------

echo
log "compile complete"
# Walk the bin dir without shellchecking-`ls` (SC2012).  BSD find
# (macOS) doesn't support `-printf`, so we use `-exec stat`
# instead: `stat -c '%n %s'` on GNU coreutils, `stat -f '%N %z'`
# on BSD.  Auto-detect by trying GNU first.  Sorted for
# deterministic build logs.  numfmt renders the byte count
# human-readable when present; otherwise raw bytes.
if stat -c '%n' /dev/null >/dev/null 2>&1; then
    stat_fmt='-c'; stat_args='%n %s'           # GNU coreutils
else
    stat_fmt='-f'; stat_args='%N %z'           # BSD (macOS)
fi
find "$BIN_DIR" -maxdepth 1 -type f -exec stat "$stat_fmt" "$stat_args" {} \; \
    | sort | while read -r path size; do
    name=$(basename "$path")
    if command -v numfmt >/dev/null 2>&1; then
        size=$(numfmt --to=iec --suffix=B "$size")
    fi
    printf '    %-30s %s\n' "$name" "$size"
done
echo
ok "version: ${VERSION}"
ok "commit:  ${COMMIT}"
ok "date:    ${DATE}"
echo
log "next steps:"
echo "    ${BIN_DIR}/pg_hardstorage doctor"
echo "    ${BIN_DIR}/pg_hardstorage init --help"
echo "    ${BIN_DIR}/pg_hardstorage --help"
