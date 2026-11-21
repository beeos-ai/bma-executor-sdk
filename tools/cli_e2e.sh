#!/usr/bin/env bash
# End-to-end check of the bma command line against a deployment: builds bma
# and runs read-only commands with BMA_API_KEY (an organization key of an
# acceptance organization), checking the output and the exit codes.
#   BMA_API_KEY=... BMA_BASE_URL=https://api.staging.beeos.ai BMA_WORKSPACE_ID=... tools/cli_e2e.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${BMA_API_KEY:?}" "${BMA_BASE_URL:?}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export BMA_CONFIG_DIR="$work/cfg"
go build -o "$work/bma" ./cmd/bma
bma() { "$work/bma" "$@"; }

expect() { # expect <exit code> <command...>
  local want="$1"; shift
  set +e
  "$@" >"$work/out" 2>"$work/err"
  local got=$?
  set -e
  if [ "$got" != "$want" ]; then
    echo "FAIL: $* exited $got, want $want" >&2
    cat "$work/err" >&2
    exit 1
  fi
  echo "ok: $* (exit $got)"
}

expect 0 bma auth status --json
jq -e '.signed_in == true' "$work/out" >/dev/null
expect 0 bma machines list --json
jq -e 'has("data")' "$work/out" >/dev/null
expect 0 bma machine-pools list --all --query 'length'
expect 0 bma model-providers list --format jsonl
expect 4 bma machines get mch_00000000000000000000000000
expect 2 bma machines get
BMA_API_KEY=bma_org_v1_invalid expect 3 bma machines list
echo "cli e2e: all checks passed against $BMA_BASE_URL"
