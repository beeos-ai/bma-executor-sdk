#!/usr/bin/env bash
# Downloads the official claude binary of one release for this platform from
# Anthropic's release bucket and checks it against the release manifest (the
# checksum the official installer verifies). The SDK checks it again against
# its own pins (claudeCLIPins) when it starts it.
#   tools/fetch_claude_cli.sh <version> <destination file>
set -euo pipefail
version="$1" dest="$2"
base="https://downloads.claude.ai/claude-code-releases/$version"
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) platform=darwin-arm64 ;;
  Darwin-x86_64) platform=darwin-x64 ;;
  Linux-aarch64|Linux-arm64) platform=linux-arm64 ;;
  Linux-x86_64) platform=linux-x64 ;;
  *) echo "unsupported platform $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac
want="$(curl -fsSL "$base/manifest.json" | python3 -c "import json,sys; print(json.load(sys.stdin)['platforms']['$platform']['checksum'])")"
mkdir -p "$(dirname "$dest")"
curl -fsSL -o "$dest.part" "$base/$platform/claude"
got="$( (command -v sha256sum >/dev/null && sha256sum "$dest.part" || shasum -a 256 "$dest.part") | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
  rm -f "$dest.part"
  echo "claude $version $platform: sha256 $got, the manifest says $want" >&2
  exit 1
fi
chmod +x "$dest.part"
mv "$dest.part" "$dest"
echo "claude $version $platform -> $dest ($got)"
