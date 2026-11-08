#!/usr/bin/env bash
# Mandatory when selected: missing tools or failed scenarios must fail the gate.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/scripts/browser/check-tools.sh"
export CHAT_ARCHIVE_BROWSER_TEST=1
export CHAT_ARCHIVE_BROWSER_ARTIFACTS=${CHAT_ARCHIVE_BROWSER_ARTIFACTS:-$(mktemp -d /tmp/chat-archive-browser-artifacts.XXXXXXXX)}
mkdir -p "$CHAT_ARCHIVE_BROWSER_ARTIFACTS"
printf 'Browser acceptance artifacts: %s\n' "$CHAT_ARCHIVE_BROWSER_ARTIFACTS"
go -C "$ROOT" test ./internal/plugin -run TestSidebar -count=1 -json \
  | tee "$CHAT_ARCHIVE_BROWSER_ARTIFACTS/go-test.jsonl" \
  | python3 "$ROOT/scripts/browser/require-go-pass.py"
