#!/usr/bin/env bash
# Builds the installable Windows package: dist/bma-windows-hand-<sha>-windows-amd64.zip
# (windows-hand.exe, install.ps1, uninstall.ps1, README.txt, NOTICE, SHA256SUMS).
# Cross-compiles, so any host with Go and zip works.
#   packaging/windows/build.sh [outdir]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
out="${1:-dist}"
version="$(node -p "require('./typescript/package.json').version")"
commit="$(git rev-parse --short=12 HEAD)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
pkg="$stage/bma-windows-hand"
mkdir -p "$pkg" "$out"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
  -o "$pkg/windows-hand.exe" ./examples/windows-hand
cp packaging/windows/install.ps1 packaging/windows/uninstall.ps1 packaging/windows/README.txt \
   packaging/windows/NOTICE-cua-driver.txt LICENSE "$pkg/"
(cd "$pkg" && sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS)
zipfile="$(cd "$out" && pwd)/bma-windows-hand-$commit-windows-amd64.zip"
rm -f "$zipfile"
(cd "$stage" && zip -qr "$zipfile" bma-windows-hand)
echo "$zipfile"
