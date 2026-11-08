#!/usr/bin/env bash
# Required, non-publishing native acceptance for chat-archive.
# Builds CPA v8.0.60 from its pinned source, the production, reload and nativefixture
# libraries, the reject-fixture test plugin and the fake archive, then runs everything in a
# --network none container. Never reads operator configuration; every value is synthetic.
#
#   CPA_SOURCE_DIR=/path      reuse a local checkout of the pinned commit (copied, never modified)
#   CPA_SOURCE_ARCHIVE=/path  reuse a downloaded codeload archive (sha256 checked)
#   SMOKE_ONLY=main,duplex    run only some acceptance phases
#   SMOKE_CPUS / SMOKE_MEMORY container limits (default 2 CPUs, 3g)
#   SMOKE_LABEL               container name label: ca-test-<label>-<work dir> (default smoke)
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
python3 "$ROOT/scripts/check-candidate.py"
source "$ROOT/scripts/native/pins.sh"
for tool in go gcc curl tar docker sha256sum; do
  command -v "$tool" >/dev/null || { printf 'Required tool missing: %s\n' "$tool" >&2; exit 1; }
done
[[ $(go env GOOS)/$(go env GOARCH) == linux/amd64 ]] || { printf 'Native acceptance requires Linux amd64.\n' >&2; exit 1; }
docker info >/dev/null # Missing/unavailable Docker is a failure, never a skip.
WORK=$(mktemp -d /tmp/chat-archive-smoke.XXXXXXXX)
printf 'Pinned CPA source/build/runtime artifacts: %s\n' "$WORK"
mkdir -p "$WORK/runtime"
fetch_cpa_source "$WORK/source"
LIBFLAGS='-s -w -X github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin.Version'
CGO_ENABLED=1 go -C "$WORK/source" build -trimpath \
  -ldflags "-X main.Version=v8.0.60 -X main.Commit=$CPA_PIN" -o "$WORK/cpa" ./cmd/server
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -buildmode=c-shared \
  -ldflags "$LIBFLAGS=0.1.0" -o "$WORK/chat-archive-production.so" .
# A second production build with a higher version, for the reload case (a different file and
# different bytes, so CPA loads a new instance while the old one is quiesced).
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -buildmode=c-shared \
  -ldflags "$LIBFLAGS=9.9.9" -o "$WORK/chat-archive-reload.so" .
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -tags nativefixture -buildmode=c-shared -o "$WORK/chat-archive.so" .
CGO_ENABLED=0 go -C "$ROOT" build -trimpath -o "$WORK/fakearchive" ./scripts/native/fakearchive
gcc -shared -fPIC -O2 -Wall -o "$WORK/reject-fixture.so" "$ROOT/scripts/native/rejectplugin.c"
# Runtime network isolation is mandatory. UID 0 avoids the host user's inotify quota; CHOWN
# touches only the fresh synthetic /tmp bind mount.
docker run --rm --name "ca-test-${SMOKE_LABEL:-smoke}-$(basename "$WORK" | tr 'A-Z.' 'a-z-')" --platform linux/amd64 \
  --network none --read-only --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges \
  --pids-limit 512 --cpus "${SMOKE_CPUS:-2}" --memory "${SMOKE_MEMORY:-3g}" \
  -e PYTHONDONTWRITEBYTECODE=1 -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" \
  -e "SMOKE_ONLY=${SMOKE_ONLY:-main,duplex,fixture,memory,latency,blackhole}" \
  -v "$WORK/cpa:/fixture/cpa:ro" \
  -v "$WORK/chat-archive-production.so:/fixture/chat-archive-production.so:ro" \
  -v "$WORK/chat-archive-reload.so:/fixture/chat-archive-reload.so:ro" \
  -v "$WORK/chat-archive.so:/fixture/chat-archive.so:ro" \
  -v "$WORK/reject-fixture.so:/fixture/reject-fixture.so:ro" \
  -v "$WORK/fakearchive:/fixture/fakearchive:ro" \
  -v "$ROOT/scripts/native:/fixture/scripts:ro" -v "$WORK/runtime:/tmp" \
  "$PYTHON_IMAGE" sh -ec '
    trap '\''chown -R "$HOST_UID:$HOST_GID" /tmp'\'' EXIT
    chown 0:0 /tmp
    python /fixture/scripts/abi_probe.py /fixture/chat-archive-production.so
    python /fixture/scripts/abi_probe.py /fixture/chat-archive.so
    python /fixture/scripts/acceptance.py --cpa /fixture/cpa --library /fixture/chat-archive-production.so \
      --reload-library /fixture/chat-archive-reload.so --fixture-library /fixture/chat-archive.so \
      --reject-library /fixture/reject-fixture.so --fakearchive /fixture/fakearchive --only "$SMOKE_ONLY" \
      --results /tmp/results.json
  '
printf 'PASS. Retained synthetic logs and observations under %s/runtime.\n' "$WORK"
