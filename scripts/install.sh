#!/bin/sh
#
# pg_hardstorage installer — one-line setup.
#
# Usage:
#   curl -sSL https://get.pghardstorage.org | sh
#   curl -sSL https://get.pghardstorage.org | sh -s -- --version v1.0.0
#   curl -sSL https://get.pghardstorage.org | sh -s -- --bindir ~/.local/bin
#
# Flags (all optional):
#   --version <tag>   install a specific release tag (default: latest)
#   --bindir <dir>    install directory (default: /usr/local/bin, with a
#                     ~/.local/bin fallback when the former isn't writable)
#   --no-verify       skip checksum/signature verification (NOT advised)
#   --help            show this help and exit
#
# What it does: detects OS/arch, resolves the release, downloads the
# matching tarball + checksums.txt, verifies the SHA-256 (and the cosign
# signature when cosign is installed), then installs the binary.
#
# Portability: the canonical invocation is `... | sh`, which runs under
# whatever /bin/sh is (dash on Debian/Ubuntu, not bash).  A piped script
# has no file on disk to re-exec, so we stay strictly POSIX sh here
# rather than relying on bash — no [[ ]], no arrays, no `set -o pipefail`.

set -eu

REPO="cybertec-postgresql/pg_hardstorage"

# Identity the release artefacts are cosign-signed under (keyless /
# Sigstore via GitHub Actions OIDC).  Used only when cosign is present.
COSIGN_IDENTITY_REGEXP="https://github.com/${REPO}"
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { printf "${CYAN}→${NC} %s\n" "$*"; }
ok()    { printf "  ${GREEN}✓${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}!${NC} %s\n" "$*" >&2; }
err()   { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

# usage prints the flag help.  We emit a static heredoc rather than
# sed-ing this file's header, because under `curl | sh` there is no
# script file on disk to read ($0 is the shell, not a path).
usage() {
    cat <<'EOF'
pg_hardstorage installer — one-line setup.

Usage:
  curl -sSL https://get.pghardstorage.org | sh
  curl -sSL https://get.pghardstorage.org | sh -s -- --version v1.0.0
  curl -sSL https://get.pghardstorage.org | sh -s -- --bindir ~/.local/bin

Flags (all optional):
  --version <tag>   install a specific release tag (default: latest)
  --bindir <dir>    install directory (default: /usr/local/bin, with a
                    ~/.local/bin fallback when the former isn't writable)
  --no-verify       skip checksum/signature verification (NOT advised)
  --help            show this help and exit
EOF
    exit 0
}

# resolve_latest_tag prints the newest release tag name.  We follow the
# GitHub "latest release" redirect rather than the API to avoid the
# 60-req/hr unauthenticated API rate limit: /releases/latest
# 302-redirects to /releases/tag/<TAG>, and we read <TAG> off the final
# effective URL.  Works with both curl and wget.
resolve_latest_tag() {
    latest_url="https://github.com/${REPO}/releases/latest"
    tag=""
    if command -v curl >/dev/null 2>&1; then
        tag="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$latest_url" 2>/dev/null \
                | sed -n 's@.*/releases/tag/\(.*\)@\1@p' | tr -d '\r')"
    elif command -v wget >/dev/null 2>&1; then
        tag="$(wget -q -S --max-redirect=5 -O /dev/null "$latest_url" 2>&1 \
                | sed -n 's@.*/releases/tag/\(.*\)@\1@p' | tail -1 | tr -d '\r ')"
    fi
    printf '%s' "$tag"
}

# download URL DEST — fetch with curl or wget, hard-fail on error.
download() {
    url="$1"; dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$dest"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$dest"
    else
        err "Neither curl nor wget found. Install one and try again."
    fi
}

# sha256_of FILE — print the file's hex SHA-256, portable across the
# coreutils (sha256sum) and BSD/macOS (shasum -a 256) worlds.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        err "No sha256 tool (sha256sum / shasum) found; cannot verify download."
    fi
}

main() {
    version="latest"
    install_dir="${INSTALL_DIR:-/usr/local/bin}"
    verify=1

    # --- arg parsing (the old script read $1 directly, so `--version X`
    #     was taken as the version literally; parse flags properly). ---
    while [ $# -gt 0 ]; do
        case "$1" in
            --version) version="${2:-}"; shift 2 || err "--version needs a value" ;;
            --bindir)  install_dir="${2:-}"; shift 2 || err "--bindir needs a value" ;;
            --no-verify) verify=0; shift ;;
            --help|-h) usage ;;
            *) err "Unknown argument: $1 (try --help)" ;;
        esac
    done

    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *)      err "Unsupported OS: $(uname -s). pg_hardstorage supports Linux and macOS." ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *)  err "Unsupported architecture: $(uname -m). pg_hardstorage supports amd64 and arm64." ;;
    esac
    # macOS ships arm64 only (matches .goreleaser.yaml's darwin/amd64 ignore).
    if [ "$os" = "darwin" ] && [ "$arch" = "amd64" ]; then
        err "macOS builds are arm64 only. On an Intel Mac, install via Rosetta or build from source."
    fi

    # --- resolve the version tag ---
    if [ "$version" = "latest" ]; then
        info "Resolving the latest release"
        version="$(resolve_latest_tag)"
        [ -n "$version" ] || err "Could not resolve latest release. Pass --version <tag>, or see https://github.com/${REPO}/releases"
    fi
    ok "Release: ${version}"

    # goreleaser names archives <project>_<version>_<os>_<arch>.tar.gz with
    # the version stripped of its leading 'v' (.goreleaser.yaml uses
    # {{ .Version }}, which is the tag without the 'v').
    ver_noV="${version#v}"
    tarball="pg_hardstorage_${ver_noV}_${os}_${arch}.tar.gz"
    base="https://github.com/${REPO}/releases/download/${version}"

    info "Installing pg_hardstorage ${version} for ${os}/${arch}"

    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    info "Downloading ${tarball}"
    download "${base}/${tarball}" "$tmpdir/$tarball" \
        || err "Download failed. Check https://github.com/${REPO}/releases/tag/${version}"

    # --- verification ---
    if [ "$verify" -eq 1 ]; then
        info "Verifying checksum"
        download "${base}/checksums.txt" "$tmpdir/checksums.txt" \
            || err "Could not download checksums.txt — cannot verify. Re-run with --no-verify to override (not advised)."

        want="$(grep " ${tarball}\$" "$tmpdir/checksums.txt" | awk '{print $1}' | head -1)"
        [ -n "$want" ] || err "No checksum entry for ${tarball} in checksums.txt."
        got="$(sha256_of "$tmpdir/$tarball")"
        if [ "$want" != "$got" ]; then
            err "Checksum mismatch for ${tarball}! expected ${want}, got ${got}. Aborting."
        fi
        ok "SHA-256 verified"

        # cosign is optional: verify the signature when the tool is present,
        # otherwise note it was skipped.  The release signs every artefact
        # keylessly; the .sig + .pem sit next to the tarball.
        if command -v cosign >/dev/null 2>&1; then
            info "Verifying cosign signature"
            download "${base}/${tarball}.sig" "$tmpdir/$tarball.sig" \
                || err "Could not download ${tarball}.sig for cosign verification."
            download "${base}/${tarball}.pem" "$tmpdir/$tarball.pem" \
                || err "Could not download ${tarball}.pem for cosign verification."
            if cosign verify-blob \
                    --certificate-identity-regexp "$COSIGN_IDENTITY_REGEXP" \
                    --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" \
                    --certificate "$tmpdir/$tarball.pem" \
                    --signature "$tmpdir/$tarball.sig" \
                    "$tmpdir/$tarball" >/dev/null 2>&1; then
                ok "cosign signature verified"
            else
                err "cosign verification FAILED for ${tarball}. Aborting."
            fi
        else
            warn "cosign not installed — skipping signature check (checksum still verified)."
            warn "For supply-chain assurance, install cosign and re-run: https://docs.sigstore.dev/cosign/installation/"
        fi
    else
        warn "Verification skipped (--no-verify). You are trusting an unverified download."
    fi

    info "Extracting"
    tar xzf "$tmpdir/$tarball" -C "$tmpdir"

    binary="$tmpdir/pg_hardstorage"
    if [ ! -f "$binary" ]; then
        binary="$tmpdir/pg_hardstorage_${ver_noV}_${os}_${arch}/pg_hardstorage"
    fi
    [ -f "$binary" ] || err "Could not find pg_hardstorage binary in tarball."
    chmod +x "$binary"

    # --- install ---
    if [ -w "$install_dir" ]; then
        cp "$binary" "$install_dir/pg_hardstorage"
        ok "Installed to $install_dir/pg_hardstorage"
    elif [ ! -t 0 ]; then
        # No TTY (the canonical `curl | sh` case): we can't prompt for a
        # sudo decision, so fall back to the user-local bin rather than
        # blocking on a read that returns EOF.
        mkdir -p "$HOME/.local/bin"
        cp "$binary" "$HOME/.local/bin/pg_hardstorage"
        ok "Installed to $HOME/.local/bin/pg_hardstorage"
        printf "  Add to PATH: ${BOLD}export PATH=\"\$HOME/.local/bin:\$PATH\"${NC}\n"
    else
        printf "  Install to ${BOLD}%s${NC} (requires sudo)? [Y/n] " "$install_dir"
        read -r answer
        case "$answer" in
            [Nn]*)
                mkdir -p "$HOME/.local/bin"
                cp "$binary" "$HOME/.local/bin/pg_hardstorage"
                ok "Installed to $HOME/.local/bin/pg_hardstorage"
                printf "  Add to PATH: ${BOLD}export PATH=\"\$HOME/.local/bin:\$PATH\"${NC}\n"
                ;;
            *)
                sudo cp "$binary" "$install_dir/pg_hardstorage"
                ok "Installed to $install_dir/pg_hardstorage"
                ;;
        esac
    fi

    printf "\n"
    printf "${BOLD}Next steps:${NC}\n"
    printf "  pg_hardstorage version\n"
    printf "  pg_hardstorage demo\n"
    printf "  pg_hardstorage init --quick\n"
    printf "  https://docs.pghardstorage.org\n"
}

main "$@"
