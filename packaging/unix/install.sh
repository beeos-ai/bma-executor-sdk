#!/bin/sh
# Installs the bma command for this user (no sudo): downloads the release
# archive for this platform from the GitHub release of bma-executor-sdk,
# checks it against the release manifest's sha256, and puts bma in
# ~/.bma/bin. Then run `bma up` to connect this computer.
#
#   curl -fsSL https://beeos.ai/bma/install.sh | sh
#   curl -fsSL https://beeos.ai/bma/install.sh | sh -s -- --version 1.7.0
#
# BMA_INSTALL_BASE overrides the release download base (tests).
set -eu

repo="https://github.com/beeos-ai/bma-executor-sdk/releases"
version=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) version="${2:?--version needs X.Y.Z}"; shift 2 ;;
    *) echo "bma install: unknown option $1" >&2; exit 2 ;;
  esac
done
if [ -n "${BMA_INSTALL_BASE:-}" ]; then
  base="$BMA_INSTALL_BASE"
elif [ -n "$version" ]; then
  base="$repo/download/v$version"
else
  base="$repo/latest/download"
fi

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "bma install: $(uname -s) is not supported (macOS and Linux are)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "bma install: $(uname -m) is not supported (arm64 and x86_64 are)" >&2; exit 1 ;;
esac

need() { command -v "$1" >/dev/null 2>&1 || { echo "bma install: $1 is required" >&2; exit 1; }; }
need curl
need tar
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
archive="bma-$os-$arch.tar.gz"
curl -fsSL -o "$tmp/manifest.json" "$base/bma-manifest.json"
# The manifest: {"version":"X.Y.Z","artifacts":{"bma-darwin-arm64.tar.gz":"<sha256>",...}}
want="$(sed -n "s/.*\"$archive\": *\"\([0-9a-f]\{64\}\)\".*/\1/p" "$tmp/manifest.json")"
got_version="$(sed -n 's/.*"version": *"\([0-9.]*\)".*/\1/p' "$tmp/manifest.json")"
[ -n "$want" ] || { echo "bma install: the release has no $archive" >&2; exit 1; }
curl -fsSL -o "$tmp/$archive" "$base/$archive"
got="$(sha256 "$tmp/$archive")"
if [ "$got" != "$want" ]; then
  echo "bma install: $archive sha256 $got, the manifest says $want: refused" >&2
  exit 1
fi
tar -xzf "$tmp/$archive" -C "$tmp"
dir="$HOME/.bma/bin"
mkdir -p "$dir"
mv -f "$tmp/bma" "$dir/bma.new"
chmod 755 "$dir/bma.new"
mv -f "$dir/bma.new" "$dir/bma"

line='export PATH="$HOME/.bma/bin:$PATH"'
case "${SHELL:-}" in
  */zsh) profile="$HOME/.zprofile" ;;
  */bash) if [ "$os" = darwin ]; then profile="$HOME/.bash_profile"; else profile="$HOME/.bashrc"; fi ;;
  *) profile="$HOME/.profile" ;;
esac
if ! grep -qs '.bma/bin' "$profile"; then
  printf '\n# bma\n%s\n' "$line" >> "$profile"
fi

echo "bma $got_version installed in $dir/bma"
case ":$PATH:" in
  *":$dir:"*) echo "Next: bma up" ;;
  *) echo "Next: open a new terminal (or run: $line), then: bma up" ;;
esac
