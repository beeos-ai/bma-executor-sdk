#!/usr/bin/env bash
# End-to-end check of the bma command line against a deployment: builds bma,
# runs read commands with BMA_API_KEY (an organization key of an acceptance
# organization) checking the output and the exit codes, then one real turn
# (environment, agent, session, `sessions send --wait`, event stream), and
# removes what it created.
#   BMA_API_KEY=... BMA_BASE_URL=https://api.staging.beeos.ai BMA_WORKSPACE_ID=... tools/cli_e2e.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${BMA_API_KEY:?}" "${BMA_BASE_URL:?}" "${BMA_WORKSPACE_ID:?}"
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
expect 0 bma agents list --query '.data | length'
expect 0 bma sessions list --format jsonl

# One turn. Everything created is named after this run and removed on exit.
stamp="cli-e2e-$(date +%s)-$$"
env_id="" agent_id="" session_id=""
cleanup() {
  [ -n "$session_id" ] && "$work/bma" sessions delete "$session_id" >/dev/null 2>&1 || true
  [ -n "$agent_id" ] && "$work/bma" agents archive "$agent_id" >/dev/null 2>&1 || true
  [ -n "$env_id" ] && "$work/bma" environments delete "$env_id" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
env_id="$(bma environments create --name "$stamp" --config '{type: cloud, networking: {type: unrestricted}}' --query .id)"
agent_id="$(bma agents create --name "$stamp" --model "${BMA_E2E_MODEL:-deepseek-flash}" --query .id)"
session_id="$(bma sessions create --agent "$agent_id" --environment-id "$env_id" --title "$stamp" --query .id)"
echo "ok: created $env_id $agent_id $session_id"
reply="$(timeout 300 "$work/bma" sessions send "$session_id" "Reply with exactly the word pong and nothing else." --wait)"
case "$reply" in
  *[Pp]ong*) echo "ok: sessions send --wait answered: $reply" ;;
  *) echo "FAIL: sessions send --wait answered: $reply" >&2; exit 1 ;;
esac
expect 0 bma sessions events list "$session_id" --query '[.data[] | select(.type == "agent.message")] | length'
[ "$(cat "$work/out")" -ge 1 ] || { echo "FAIL: no agent.message in the events" >&2; exit 1; }
echo "cli e2e: all checks passed against $BMA_BASE_URL"
