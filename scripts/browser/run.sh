#!/usr/bin/env bash
# Mandatory when selected: missing tools or failed scenarios must fail the gate.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/scripts/browser/check-tools.sh"
export TOKEN_USAGE_BROWSER_TEST=1
export TOKEN_USAGE_BROWSER_ARTIFACTS=${TOKEN_USAGE_BROWSER_ARTIFACTS:-$(mktemp -d /tmp/token-usage-browser-artifacts.XXXXXXXX)}
mkdir -p "$TOKEN_USAGE_BROWSER_ARTIFACTS"
printf 'Browser acceptance artifacts: %s\n' "$TOKEN_USAGE_BROWSER_ARTIFACTS"
go -C "$ROOT" test ./internal/plugin -run TestSidebar -count=1 -json \
  | tee "$TOKEN_USAGE_BROWSER_ARTIFACTS/go-test.jsonl" \
  | python3 "$ROOT/scripts/browser/require-go-pass.py"
