#!/usr/bin/env bash
set -euo pipefail

# pg_hardstorage installer — one-line setup
#
# Usage:
#   curl -sSL https://get.pghardstorage.org | sh
#   curl -sSL https://get.pghardstorage.org | sh -s -- --version v0.2.0
#
# Detects OS/arch, downloads the right binary, and offers to install.

REPO="cybertec-postgresql/pg_hardstorage"
DEFAULT_VERSION="latest"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info()  { printf "${CYAN}→${NC} %s\n" "$*"; }
ok()    { printf "  ${GREEN}✓${NC} %s\n" "$*"; }
err()   { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

main() {
    local version="${1:-$DEFAULT_VERSION}"

    local os arch
    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin";;
        *)      err "Unsupported OS: $(uname -s). pg_hardstorage supports Linux and macOS." ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *)  err "Unsupported architecture: $(uname -m). pg_hardstorage supports amd64 and arm64." ;;
    esac

    info "Installing pg_hardstorage for ${os}/${arch}"

    local tarball="pg_hardstorage_${os}_${arch}.tar.gz"
    local url="https://github.com/${REPO}/releases/download/${version}/${tarball}"

    local tmpdir
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    info "Downloading ${url}"
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" -o "$tmpdir/$tarball" || err "Download failed. Check https://github.com/${REPO}/releases"
    elif command -v wget &>/dev/null; then
        wget -q "$url" -O "$tmpdir/$tarball" || err "Download failed. Check https://github.com/${REPO}/releases"
    else
        err "Neither curl nor wget found. Install one and try again."
    fi

    info "Extracting"
    tar xzf "$tmpdir/$tarball" -C "$tmpdir"

    local binary="$tmpdir/pg_hardstorage"
    if [[ ! -f "$binary" ]]; then
        binary="$tmpdir/pg_hardstorage_${os}_${arch}/pg_hardstorage"
    fi
    if [[ ! -f "$binary" ]]; then
        err "Could not find pg_hardstorage binary in tarball."
    fi

    chmod +x "$binary"

    local install_dir="/usr/local/bin"
    case "${INSTALL_DIR:-}" in
        "") ;;
        *) install_dir="$INSTALL_DIR" ;;
    esac

    if [[ -w "$install_dir" ]]; then
        cp "$binary" "$install_dir/pg_hardstorage"
        ok "Installed to $install_dir/pg_hardstorage"
    else
        printf "  Install to ${BOLD}%s${NC} (requires sudo)? [Y/n] " "$install_dir"
        read -r answer
        if [[ "$answer" =~ ^[Nn] ]]; then
            cp "$binary" "$HOME/.local/bin/pg_hardstorage" 2>/dev/null || {
                mkdir -p "$HOME/.local/bin"
                cp "$binary" "$HOME/.local/bin/pg_hardstorage"
            }
            ok "Installed to $HOME/.local/bin/pg_hardstorage"
            printf "  Add to PATH: ${BOLD}export PATH=\"\$HOME/.local/bin:\$PATH\"${NC}\n"
        else
            sudo cp "$binary" "$install_dir/pg_hardstorage"
            ok "Installed to $install_dir/pg_hardstorage"
        fi
    fi

    printf "\n"
    printf "${BOLD}Next steps:${NC}\n"
    printf "  %s version\n" "pg_hardstorage"
    printf "  %s demo\n" "pg_hardstorage"
    printf "  %s init --quick\n" "pg_hardstorage"
    printf "  %s\n" "https://docs.pghardstorage.org"
}

main "$@"