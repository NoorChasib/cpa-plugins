#!/usr/bin/env bash
# Short native loop: the ABI probes and the nativefixture phase only.
set -euo pipefail
SMOKE_ONLY=fixture exec bash "$(dirname -- "${BASH_SOURCE[0]}")/run-smoke.sh"
