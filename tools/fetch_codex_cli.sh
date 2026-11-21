#!/usr/bin/env bash
# Downloads the official codex binary of one release for this platform from
# OpenAI's GitHub release and checks the release asset against the sha256
# digest GitHub publishes for it. The SDK checks the binary again against its
# own pins (codexPins) when it starts it.
#   tools/fetch_codex_cli.sh <version> <destination file>
set -euo pipefail
version="$1" dest="$2"
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) target=aarch64-apple-darwin ;;
  Darwin-x86_64) target=x86_64-apple-darwin ;;
  Linux-aarch64|Linux-arm64) target=aarch64-unknown-linux-musl ;;
  Linux-x86_64) target=x86_64-unknown-linux-musl ;;
  *) echo "unsupported platform $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac
asset="codex-$target.tar.gz"
auth=()
[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
want="$(curl -fsSL ${auth[@]+"${auth[@]}"} "https://api.github.com/repos/openai/codex/releases/tags/rust-v$version" |
  python3 -c "import json,sys; print(next(a['digest'] for a in json.load(sys.stdin)['assets'] if a['name']=='$asset').removeprefix('sha256:'))")"
mkdir -p "$(dirname "$dest")"
curl -fsSL -o "$dest.tar.gz" "https://github.com/openai/codex/releases/download/rust-v$version/$asset"
got="$( (command -v sha256sum >/dev/null && sha256sum "$dest.tar.gz" || shasum -a 256 "$dest.tar.gz") | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
  rm -f "$dest.tar.gz"
  echo "codex $version $target: sha256 $got, GitHub says $want" >&2
  exit 1
fi
tar -xzf "$dest.tar.gz" -C "$(dirname "$dest")" "codex-$target"
rm -f "$dest.tar.gz"
mv "$(dirname "$dest")/codex-$target" "$dest"
chmod +x "$dest"
echo "codex $version $target -> $dest (asset $got)"
