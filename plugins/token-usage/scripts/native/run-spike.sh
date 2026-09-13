#!/usr/bin/env bash
# Preserve the Phase A entry point; the fixture now receives a private SQLite path.
set -euo pipefail
exec bash "$(dirname -- "${BASH_SOURCE[0]}")/run-smoke.sh" spike
