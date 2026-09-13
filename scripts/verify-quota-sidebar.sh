#!/usr/bin/env bash
# Reuse the repository's existing pinned test-only browser toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."
bash plugins/token-usage/scripts/browser/install-node.sh "${RUNNER_TEMP:?}/quota-cache-node"
export PATH="$RUNNER_TEMP/quota-cache-node/bin:$PATH"
make -C plugins/token-usage browser-tools
source plugins/token-usage/scripts/browser/check-tools.sh
export QUOTA_CACHE_AGENT_BROWSER="$TOKEN_USAGE_AGENT_BROWSER"
export AGENT_BROWSER_ARGS=--no-sandbox
python3 plugins/quota-cache/scripts/sidebar-smoke.py
