#!/bin/sh
# Install mem from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/introvert/memcli/main/install.sh | sh
#
# Options (environment variables):
#   MEM_VERSION=v0.1.0          install a specific release (default: latest)
#   MEM_INSTALL_DIR=/usr/local/bin   where to put the binary (default: ~/.local/bin)
set -eu

REPO="introvert/memcli"
VERSION="${MEM_VERSION:-latest}"
INSTALL_DIR="${MEM_INSTALL_DIR:-$HOME/.local/bin}"
BASE_URL="${MEM_DOWNLOAD_URL:-https://github.com/$REPO/releases}" # override for mirrors/testing

say() { printf '%s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported OS $(uname -s); on Windows download the .zip from https://github.com/$REPO/releases" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
  url="$BASE_URL/latest/download"
else
  url="$BASE_URL/download/$VERSION"
fi
asset="mem_${os}_${arch}.tar.gz"

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  fail "need curl or wget"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

say "Downloading mem ($VERSION, $os/$arch)..."
fetch "$url/$asset" "$tmp/$asset" || fail "download failed: $url/$asset"
fetch "$url/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $url/checksums.txt"

expected="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || fail "no checksum for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$asset" | awk '{ print $1 }')"
else
  actual="$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')"
fi
[ "$expected" = "$actual" ] || fail "checksum mismatch for $asset"

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$INSTALL_DIR"
install -m 755 "$tmp/mem" "$INSTALL_DIR/mem" 2>/dev/null ||
  { cp "$tmp/mem" "$INSTALL_DIR/mem" && chmod 755 "$INSTALL_DIR/mem"; } ||
  fail "cannot write to $INSTALL_DIR (try MEM_INSTALL_DIR=... or sudo)"

say "Installed $("$INSTALL_DIR/mem" --version) to $INSTALL_DIR/mem"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "Note: $INSTALL_DIR is not on your PATH. Add this to your shell profile:"
     say "  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say "Get started: mem \"my first note #hello\"   ·   mem --help"
