#!/usr/bin/env bash
# make fuse-smoke: CPA v8.0.60 with a host-side fault injected by
# scripts/native/fuse-v8.0.60.patch keeps serving after it fuses chat-archive, and the plugin keeps
# heartbeating with frozen hook counters. If the patch no longer applies, this fails loudly.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/scripts/native/pins.sh"
for tool in go curl tar docker sha256sum patch; do
  command -v "$tool" >/dev/null || { printf 'Required tool missing: %s\n' "$tool" >&2; exit 1; }
done
docker info >/dev/null
WORK=$(mktemp -d /tmp/chat-archive-fuse.XXXXXXXX)
printf 'Fuse smoke artifacts: %s\n' "$WORK"
mkdir -p "$WORK/runtime"
fetch_cpa_source "$WORK/source"
patch -d "$WORK/source" -p1 --forward --batch < "$ROOT/scripts/native/fuse-v8.0.60.patch" || {
  printf 'scripts/native/fuse-v8.0.60.patch no longer applies to the pinned CPA source.\n' >&2; exit 1; }
CGO_ENABLED=1 go -C "$WORK/source" build -trimpath \
  -ldflags "-X main.Version=v8.0.60-fuse -X main.Commit=$CPA_PIN" -o "$WORK/cpa" ./cmd/server
CGO_ENABLED=1 go -C "$ROOT" build -trimpath -buildmode=c-shared \
  -ldflags '-s -w -X github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin.Version=0.1.0' \
  -o "$WORK/chat-archive-production.so" .
CGO_ENABLED=0 go -C "$ROOT" build -trimpath -o "$WORK/fakearchive" ./scripts/native/fakearchive
docker run --rm --name "ca-test-fuse-$(basename "$WORK" | tr 'A-Z.' 'a-z-')" --platform linux/amd64 \
  --network none --read-only --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges \
  --pids-limit 256 --cpus "${SMOKE_CPUS:-2}" --memory 2g \
  -e PYTHONDONTWRITEBYTECODE=1 -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" \
  -v "$WORK/cpa:/fixture/cpa:ro" -v "$WORK/chat-archive-production.so:/fixture/chat-archive.so:ro" \
  -v "$WORK/fakearchive:/fixture/fakearchive:ro" -v "$ROOT/scripts/native:/fixture/scripts:ro" \
  -v "$WORK/runtime:/tmp" "$PYTHON_IMAGE" sh -ec '
    trap '\''chown -R "$HOST_UID:$HOST_GID" /tmp'\'' EXIT
    chown 0:0 /tmp
    python /fixture/scripts/fuse.py --cpa /fixture/cpa --library /fixture/chat-archive.so --fakearchive /fixture/fakearchive
  '
printf 'PASS. Retained synthetic logs under %s/runtime.\n' "$WORK"
