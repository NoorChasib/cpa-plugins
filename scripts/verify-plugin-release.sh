#!/usr/bin/env bash
set -euo pipefail
plugin=${1:?plugin required}
case "$plugin" in
  quota-cache|account-health-pushover|reset-priority|auto-baseline|token-usage) ;;
  *) echo 'Unknown plugin' >&2; exit 1 ;;
esac
cd "$(dirname "$0")/../plugins/$plugin"
case "$plugin" in
  quota-cache)
    make ci
    python3 scripts/native-probe.py dist/quota-cache.so
    ;;
  account-health-pushover)
    make ci c-shared package-current checksums verify-release
    CPA_SMOKE_REQUIRE_DOCKER=1 make smoke
    ;;
  reset-priority|auto-baseline)
    make fmt-check vet lint test race build
    make package GLIBC_ENFORCE=0
    make check-release
    bash -n scripts/smoke-test.sh
    ;;
  token-usage)
    test "$(go env GOVERSION)" = go1.27.1
    bash scripts/browser/install-node.sh "${RUNNER_TEMP:?}/token-usage-node"
    export PATH="$RUNNER_TEMP/token-usage-node/bin:$PATH"
    export AGENT_BROWSER_ARGS=--no-sandbox
    make browser-tools ci browser
    make smoke | tee "$RUNNER_TEMP/native-smoke.log"
    CPA_SMOKE_WORK=$(python3 - "$RUNNER_TEMP/native-smoke.log" <<'PY'
import re
import sys
from pathlib import Path
matches = re.findall(r'^Pinned CPA source/build/runtime artifacts: (/tmp/token-usage-smoke\.[A-Za-z0-9]+)$', Path(sys.argv[1]).read_text(), re.M)
if len(matches) != 1:
    raise SystemExit('expected exactly one native smoke work directory')
print(matches[0])
PY
    )
    cp "$CPA_SMOKE_WORK/token-usage-production.so" dist/token-usage.so
    docker pull --platform linux/amd64 python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285
    make store-smoke CPA_SMOKE_WORK="$CPA_SMOKE_WORK"
    make package-tested checksums verify-release
    cmp "$CPA_SMOKE_WORK/token-usage-production.so" dist/token-usage.so
    (cd dist && sha256sum -c checksums.txt)
    python3 scripts/verify-cpa-package.py --cpa-source "$CPA_SMOKE_WORK/source"
    ;;
esac
