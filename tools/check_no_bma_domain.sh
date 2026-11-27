#!/usr/bin/env bash
# No tracked file may name a retired bma.beeos.ai host (L790) except the old-host rewrite tables that migrate saved
# configs (executor_origin / MigrateExecutorURL), their tests, the CHANGELOG, and the contract snapshot that is
# regenerated from BMA main (tools/check_contract.py).
set -euo pipefail
cd "$(dirname "$0")/.."
allow=(
  ':!client.go' ':!python/src/bma_executor_sdk/client.py' ':!typescript/src/client.ts'
  ':!base_url_test.go' ':!cmd/bma/cli_test.go' ':!python/tests/test_base_url.py' ':!typescript/test/base-url.test.ts'
  ':!CHANGELOG.md' ':!contracts/' ':!tools/check_no_bma_domain.sh'
)
if hits=$(git grep -nIE 'bma\.beeos\.ai' -- . "${allow[@]}"); then
  printf '%s\n\nretired bma.beeos.ai host outside the allowlist in tools/check_no_bma_domain.sh\n' "$hits" >&2
  exit 1
fi
