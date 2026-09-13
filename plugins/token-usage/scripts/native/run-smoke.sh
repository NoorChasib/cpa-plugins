#!/usr/bin/env bash
# Required, non-publishing acceptance. Never reads operator configuration.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
python3 "$ROOT/scripts/check-candidate.py"
MODE=${1:-all}
[[ "$MODE" == all || "$MODE" == spike ]] || { printf 'Expected all or spike mode.\n' >&2; exit 1; }
CPA_PIN=7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974
CPA_ARCHIVE_SHA256=3f829041f62175a546750520d1c160cba1a7618d30f17618731e250c048d77e5
PYTHON_IMAGE=python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285
for tool in go gcc curl tar docker sha256sum; do
  command -v "$tool" >/dev/null || { printf 'Required tool missing: %s\n' "$tool" >&2; exit 1; }
done
[[ $(go env GOOS)/$(go env GOARCH) == linux/amd64 ]] || { printf 'Native acceptance requires Linux amd64.\n' >&2; exit 1; }
docker info >/dev/null # Missing/unavailable Docker is a failure, never a skip.
WORK=$(mktemp -d /tmp/token-usage-smoke.XXXXXXXX)
printf 'Pinned CPA source/build/runtime artifacts: %s\n' "$WORK"
mkdir -p "$WORK/source" "$WORK/runtime"
# Reuse only an explicitly supplied archive; otherwise fetch the pinned source.
if [[ -n ${CPA_SOURCE_ARCHIVE:-} ]]; then
  [[ -f "$CPA_SOURCE_ARCHIVE" ]] || { printf 'Requested source archive is missing.\n' >&2; exit 1; }
  cp -- "$CPA_SOURCE_ARCHIVE" "$WORK/cpa.tar.gz"
else
  curl --fail --location --silent --show-error "https://codeload.github.com/router-for-me/CLIProxyAPI/tar.gz/$CPA_PIN" -o "$WORK/cpa.tar.gz"
fi
printf '%s  %s\n' "$CPA_ARCHIVE_SHA256" "$WORK/cpa.tar.gz" | sha256sum -c -
tar -xzf "$WORK/cpa.tar.gz" --strip-components=1 -C "$WORK/source"
CGO_ENABLED=1 go -C "$WORK/source" build -trimpath \
  -ldflags "-X main.Version=v7.2.155 -X main.Commit=$CPA_PIN" -o "$WORK/cpa" ./cmd/server
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -buildmode=c-shared \
  -ldflags '-s -w -X github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/plugin.Version=0.1.3' -o "$WORK/token-usage-production.so" .
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -tags nativefixture -buildmode=c-shared -o "$WORK/token-usage.so" .
# Runtime network isolation is mandatory. UID 0 avoids the host user's inotify
# quota exhaustion; CHOWN touches only the fresh synthetic /tmp bind mount.
docker run --rm --platform linux/amd64 --network none --read-only \
  --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges \
  --pids-limit 256 --memory 2g \
  -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" -e "TEST_MODE=$MODE" \
  -v "$WORK/cpa:/fixture/cpa:ro" \
  -v "$WORK/token-usage.so:/fixture/token-usage.so:ro" \
  -v "$WORK/token-usage-production.so:/fixture/token-usage-production.so:ro" \
  -v "$ROOT/scripts/native:/fixture/scripts:ro" -v "$WORK/runtime:/tmp" \
  "$PYTHON_IMAGE" sh -ec '
    trap '\''chown -R "$HOST_UID:$HOST_GID" /tmp'\'' EXIT
    chown 0:0 /tmp
    python /fixture/scripts/abi_probe.py /fixture/token-usage-production.so
    python /fixture/scripts/abi_probe.py /fixture/token-usage.so
    python /fixture/scripts/spike.py --cpa /fixture/cpa --library /fixture/token-usage.so
    if [ "$TEST_MODE" = all ]; then
      python /fixture/scripts/production.py --cpa /fixture/cpa --library /fixture/token-usage-production.so
    fi
  '
printf 'PASS. Retained synthetic logs and observations under %s/runtime.\n' "$WORK"
