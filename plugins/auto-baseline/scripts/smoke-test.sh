#!/usr/bin/env bash
# End-to-end smoke test against CLIProxyAPI (CPA), in the pinned v8.0.4 image
# by default, in each config.yaml layout CPA v8 accepts.
#
# Usage:
#   scripts/smoke-test.sh [path-to-auto-baseline.so]
#   scripts/smoke-test.sh --cpa-bin <CLIProxyAPI binary> [path-to-auto-baseline.so] [--browser]
#   scripts/smoke-test.sh <CLIProxyAPI binary> [path-to-auto-baseline.so] [--browser]   (older form)
#
# Runners:
#   Docker (default): CPA_SMOKE_IMAGE (default: the pinned v8.0.4 digest below)
#     runs as a throwaway container published on 127.0.0.1 only. config.yaml is
#     a single-file bind mount, read-write, exactly like a Compose/Coolify
#     deployment, so the plugin's in-place write is exercised for real; the
#     plugins directory is a throwaway named volume.
#   Local binary (--cpa-bin, or a first argument that is an executable): runs
#     the given binary on 127.0.0.1. Required for --browser.
#
# Layouts (CPA_SMOKE_LAYOUT, space- or comma-separated; default "legacy v8"):
#   legacy   flat root keys, no claude-header-defaults: the compiled default
#            (2.1.280) is effective and the plugin creates the legacy block.
#   v8       config-version: 8, the layout CPA writes after a /v8/management
#            save, with the baseline at oauth.providers.claude.header-defaults
#            (2.1.283); the plugin must update that block and never create a
#            root claude-header-defaults (the 2026-09-29 loop).
#   interim  config-version: 8, but claude-header-defaults and codex kept at
#            their legacy root keys (2.1.283); the plugin keeps writing there.
# Each layout runs in its own CPA and prints its own PASS line.
#
# --browser (local binary only) adds a phase that proves the browser trust
# model over PLAIN HTTP on a NON-LOOPBACK address: CPA is bound to 0.0.0.0
# with management.allow-remote: true (legacy remote-management; key "smoke-mgmt"; the
# process is exposed on this host's interfaces for the duration of the run),
# the redacted sidebar page is fetched at http://<host-ip>:<port>, and the
# mutating routes are exercised with an Origin header and NO Sec-Fetch-Site,
# exactly the header shape a browser produces against a plain-HTTP server.
# If the agent-browser CLI is installed the phase reports it; driving it is
# not implemented here (no browser was available when this script was
# written, so no command sequence could be validated), and the phase always
# runs the curl checks.
#
# Environment:
#   CPA_SMOKE_IMAGE             Docker image (default pinned v8.0.4 digest)
#   CPA_SMOKE_LAYOUT            layouts to run (default "legacy v8")
#   CPA_SMOKE_PORT              fixed listen port, local binary only (default: a free port)
#   CPA_SMOKE_TIMEOUT_SECONDS   startup / reload wait budget (default 90)
#   CPA_SMOKE_ARTIFACT_DIR      where to keep evidence (default dist/smoke/<run-id>)
#
# Flow per layout:
#   1. write a config.yaml in that layout with the plugin enabled
#      (min-observations 3, min-distinct-sessions 2, promotion-cooldown 0s)
#      and start CPA;
#   2. POST /v1/messages three times with genuine-looking Claude Code 2.1.285
#      headers from two distinct session IDs. The config carries a placeholder
#      Claude API key whose base-url is a closed port on CPA's own loopback, so
#      the model is routable (interceptors only run for routable models), the
#      interceptor observes the headers before credential selection, and the
#      upstream call then fails locally with connection refused. No traffic
#      leaves the host;
#   3. assert the promotion landed in the block that layout requires, that
#      CPA logged the reload, and that the plugin confirmed it;
#   4. assert CPA's own runtime config (GET /v0/management/config) now
#      carries the promoted tuple: the promotion took effect;
#   5. send the same fingerprint again, wait, and assert the promotion
#      happened exactly once: one promotion log line, one history entry,
#      config.yaml unchanged and still in the plugin's 2-space encoding (a
#      conflict rewrite by CPA would re-indent it with 4 spaces).
#
# Management requests carry the placeholder key the script sets itself and
# are never retried after a 401 or 403: CPA bans a client IP for 30 minutes
# after 5 failed management-key attempts.
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_IMAGE="eceasy/cli-proxy-api:v8.0.4@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d"
IMAGE="${CPA_SMOKE_IMAGE:-${DEFAULT_IMAGE}}"
BROWSER_PHASE=false
CPA_BIN=""
POSITIONAL=()
while (( $# > 0 )); do
  case "$1" in
    --browser) BROWSER_PHASE=true ;;
    --cpa-bin)
      [[ $# -ge 2 ]] || { printf -- '--cpa-bin needs a path\n' >&2; exit 2; }
      CPA_BIN="$2"; shift ;;
    *) POSITIONAL+=("$1") ;;
  esac
  shift
done
# Older form: a first positional argument that is an executable, not a
# shared library, is the CPA binary.
if [[ -z "${CPA_BIN}" && ${#POSITIONAL[@]} -gt 0 && -x "${POSITIONAL[0]}" && "${POSITIONAL[0]}" != *.so ]]; then
  CPA_BIN="${POSITIONAL[0]}"
  POSITIONAL=("${POSITIONAL[@]:1}")
fi
PLUGIN_INPUT="${POSITIONAL[0]:-${ROOT_DIR}/auto-baseline.so}"
TIMEOUT_SECONDS="${CPA_SMOKE_TIMEOUT_SECONDS:-90}"
LAYOUTS="${CPA_SMOKE_LAYOUT:-legacy v8}"
LAYOUTS="${LAYOUTS//,/ }"
RUN_ID="${CPA_SMOKE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}}"
ARTIFACT_DIR="${CPA_SMOKE_ARTIFACT_DIR:-${ROOT_DIR}/dist/smoke/${RUN_ID}}"
MGMT_KEY="smoke-mgmt"
RUNNER=docker
[[ -n "${CPA_BIN}" ]] && RUNNER=binary

if ! [[ "${TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
  printf 'CPA_SMOKE_TIMEOUT_SECONDS must be a positive integer, got %s\n' "${TIMEOUT_SECONDS}" >&2
  exit 2
fi
read -r -a LAYOUT_LIST <<<"${LAYOUTS}"
if (( ${#LAYOUT_LIST[@]} == 0 )); then
  printf 'CPA_SMOKE_LAYOUT is empty\n' >&2
  exit 2
fi
for layout in "${LAYOUT_LIST[@]}"; do
  case "${layout}" in
    legacy|v8|interim) ;;
    *) printf 'CPA_SMOKE_LAYOUT entries must be legacy, v8, or interim; got %s\n' "${layout}" >&2; exit 2 ;;
  esac
done
if [[ "${BROWSER_PHASE}" == true && "${RUNNER}" != binary ]]; then
  printf -- '--browser exposes CPA on a non-loopback address and needs --cpa-bin; the Docker runner only publishes on 127.0.0.1\n' >&2
  exit 2
fi

# Several layouts: run each in its own process (own CPA, own cleanup trap)
# and report every result.
if (( ${#LAYOUT_LIST[@]} > 1 )); then
  args=()
  [[ -n "${CPA_BIN}" ]] && args+=(--cpa-bin "${CPA_BIN}")
  [[ "${BROWSER_PHASE}" == true ]] && args+=(--browser)
  failed=()
  for layout in "${LAYOUT_LIST[@]}"; do
    printf '=== layout %s ===\n' "${layout}"
    if ! CPA_SMOKE_LAYOUT="${layout}" CPA_SMOKE_RUN_ID="${RUN_ID}-${layout}" \
      CPA_SMOKE_ARTIFACT_DIR="${ARTIFACT_DIR}/${layout}" "$0" "${args[@]}" "${PLUGIN_INPUT}"; then
      failed+=("${layout}")
    fi
  done
  if (( ${#failed[@]} > 0 )); then
    printf 'smoke test FAILED for layout(s): %s\n' "${failed[*]}" >&2
    exit 1
  fi
  printf 'smoke test PASSED for layouts: %s\n' "${LAYOUT_LIST[*]}"
  exit 0
fi
LAYOUT="${LAYOUT_LIST[0]}"

required=(curl python3 realpath install sha256sum)
[[ "${RUNNER}" == docker ]] && required+=(docker)
for command_name in "${required[@]}"; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    printf 'required command not found: %s\n' "${command_name}" >&2
    exit 2
  fi
done
if [[ "${RUNNER}" == binary && ! -x "${CPA_BIN}" ]]; then
  printf 'CPA binary not executable: %s\n' "${CPA_BIN}" >&2
  exit 2
fi
if [[ ! -f "${PLUGIN_INPUT}" ]]; then
  printf 'plugin library not found: %s (build it with: make build)\n' "${PLUGIN_INPUT}" >&2
  exit 2
fi
[[ "${RUNNER}" == binary ]] && CPA_BIN="$(realpath "${CPA_BIN}")"
PLUGIN_INPUT="$(realpath "${PLUGIN_INPUT}")"

# Layout-specific expectations. FROM is the effective baseline before the
# promotion; SOURCE is where every promoted value must come from afterwards.
CANDIDATE_VERSION="2.1.285"
CANDIDATE_UA="claude-cli/${CANDIDATE_VERSION} (external, cli)"
case "${LAYOUT}" in
  legacy)  FROM="2.1.280"; FROM_SOURCE="default"; SOURCE="legacy"; TARGET="claude-header-defaults"; FILE_LAYOUT="legacy" ;;
  v8)      FROM="2.1.283"; FROM_SOURCE="v8";      SOURCE="v8";     TARGET="oauth.providers.claude.header-defaults"; FILE_LAYOUT="v8" ;;
  interim) FROM="2.1.283"; FROM_SOURCE="legacy";  SOURCE="legacy"; TARGET="claude-header-defaults"; FILE_LAYOUT="v8" ;;
esac

TMP_DIR=""
CPA_PID=""
CONTAINER_ID=""
PLUGIN_VOLUME=""
LOG_FILE="${ARTIFACT_DIR}/cpa.log"
BIND_HOST="127.0.0.1"
ALLOW_REMOTE=false
HOST_IP=""
if [[ "${RUNNER}" == docker ]]; then
  # Inside the container CPA listens on all interfaces; Docker publishes the
  # port on the host's loopback only. Requests reach CPA from the bridge
  # gateway, so management must allow non-loopback callers.
  BIND_HOST="0.0.0.0"
  ALLOW_REMOTE=true
fi
if [[ "${BROWSER_PHASE}" == true ]]; then
  # First non-loopback IPv4 (hostname -I may list IPv6 first), with an ip(8)
  # fallback for hosts whose hostname -I is empty.
  HOST_IP="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | grep -v '^127\.' | head -n1 || true)"
  if [[ -z "${HOST_IP}" ]] && command -v ip >/dev/null 2>&1; then
    HOST_IP="$(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -v '^127\.' | head -n1 || true)"
  fi
  if [[ -z "${HOST_IP}" ]]; then
    printf -- '--browser requires a non-loopback IPv4 address; neither hostname -I nor ip -4 addr reported one\n' >&2
    exit 2
  fi
  BIND_HOST="0.0.0.0"
  ALLOW_REMOTE=true
fi

# Paths as CPA sees them.
if [[ "${RUNNER}" == docker ]]; then
  CPA_ROOT="/CLIProxyAPI"
  AUTH_DIR="/root/.cli-proxy-api"
else
  CPA_ROOT=""  # set once TMP_DIR exists
  AUTH_DIR=""
fi

refresh_log() {
  if [[ "${RUNNER}" == docker && -n "${CONTAINER_ID}" ]]; then
    docker logs "${CONTAINER_ID}" >"${LOG_FILE}" 2>&1 || true
  fi
}

# fetch_plugin_file copies a file from the plugin state dir into dest.
fetch_plugin_file() {
  local name="$1" dest="$2"
  if [[ "${RUNNER}" == docker ]]; then
    docker cp "${CONTAINER_ID}:${CPA_ROOT}/plugins/auto-baseline/${name}" "${dest}" >/dev/null 2>&1
  else
    cp -f "${TMP_DIR}/plugins/auto-baseline/${name}" "${dest}" 2>/dev/null
  fi
}

# shellcheck disable=SC2329
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ -n "${TMP_DIR}" ]]; then
    fetch_plugin_file config.yaml.auto-baseline.bak "${ARTIFACT_DIR}/config.bak.yaml"
    fetch_plugin_file state.json "${ARTIFACT_DIR}/state.json"
  fi
  refresh_log
  if [[ -n "${CONTAINER_ID}" ]]; then
    docker rm -f "${CONTAINER_ID}" >/dev/null 2>&1
  fi
  if [[ -n "${PLUGIN_VOLUME}" ]]; then
    docker volume rm "${PLUGIN_VOLUME}" >/dev/null 2>&1
  fi
  if [[ -n "${CPA_PID}" ]] && kill -0 "${CPA_PID}" 2>/dev/null; then
    kill "${CPA_PID}" 2>/dev/null
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "${CPA_PID}" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 "${CPA_PID}" 2>/dev/null
  fi
  if [[ -n "${TMP_DIR}" ]]; then
    cp -f "${TMP_DIR}/config.yaml" "${ARTIFACT_DIR}/config.after.yaml" 2>/dev/null
    rm -rf -- "${TMP_DIR}"
  fi
  if (( status != 0 )); then
    printf 'smoke test FAILED (layout %s, %s runner); evidence: %s\n' "${LAYOUT}" "${RUNNER}" "${ARTIFACT_DIR}" >&2
    if [[ -s "${LOG_FILE}" ]]; then
      printf '%s\n' '--- CPA log (last 80 lines) ---' >&2
      tail -n 80 "${LOG_FILE}" >&2
    fi
  else
    printf 'smoke test PASSED (layout %s, %s runner); evidence retained at %s\n' "${LAYOUT}" "${RUNNER}" "${ARTIFACT_DIR}"
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "${ARTIFACT_DIR}"
TMP_DIR="$(mktemp -d)"
mkdir -p "${TMP_DIR}/plugins" "${TMP_DIR}/auth"
install -m 0755 -- "${PLUGIN_INPUT}" "${TMP_DIR}/plugins/auto-baseline.so"
if [[ "${RUNNER}" == binary ]]; then
  CPA_ROOT="${TMP_DIR}"
  AUTH_DIR="${TMP_DIR}/auth"
fi

# The placeholder Claude credential is REQUIRED for the interceptor to fire:
# CPA resolves the model's provider before it runs request interceptors; with
# no Claude credential no Claude model is routable and the request is
# rejected with HTTP 400 before interception. Its base-url is a closed port on
# CPA's own loopback, so the upstream call fails with connection refused
# after the observation.
write_config() {
  local port="$1" file="${TMP_DIR}/config.yaml"
  if [[ "${LAYOUT}" == legacy ]]; then
    cat >"${file}" <<YAML
# smoke-test config (auto-baseline)
host: "${BIND_HOST}"
port: ${port}
auth-dir: "${AUTH_DIR}"
api-keys:
  - "smoke-key"
debug: true
logging-to-file: false
remote-management:
  allow-remote: ${ALLOW_REMOTE}
  secret-key: "${MGMT_KEY}"
claude-api-key:
  - api-key: "sk-ant-smoke-placeholder"
    base-url: "http://127.0.0.1:9"
YAML
  else
    cat >"${file}" <<YAML
# smoke-test config (auto-baseline)
config-version: 8
server:
  host: "${BIND_HOST}"
  port: ${port}
access:
  api-keys:
    - "smoke-key"
management:
  allow-remote: ${ALLOW_REMOTE}
  secret-key: "${MGMT_KEY}"
observability:
  logs:
    debug: true
    logging-to-file: false
api-keys:
  claude:
    - name: "smoke"
      base-url: "http://127.0.0.1:9"
      keys:
        - api-key: "sk-ant-smoke-placeholder"
YAML
    if [[ "${LAYOUT}" == v8 ]]; then
      cat >>"${file}" <<YAML
oauth:
  auth-dir: "${AUTH_DIR}"
  providers:
    claude:
      header-defaults:
        user-agent: "claude-cli/${FROM} (external, cli)"
        package-version: "0.112.1"
        runtime-version: "v26.3.0"
    codex:
      disable-codex-cloaking: true
YAML
    else
      cat >>"${file}" <<YAML
oauth:
  auth-dir: "${AUTH_DIR}"
claude-header-defaults:
  user-agent: "claude-cli/${FROM} (external, cli)"
  package-version: "0.112.1"
  runtime-version: "v26.3.0"
codex:
  disable-codex-cloaking: true
YAML
    fi
  fi
  cat >>"${file}" <<YAML
plugins:
  enabled: true
  dir: "${CPA_ROOT}/plugins"
  configs:
    auto-baseline:
      enabled: true
      min-observations: 3
      # The smoke requests carry two distinct X-Claude-Code-Session-Id values,
      # so the stricter 2-session rule (default is 1) is exercised here.
      min-distinct-sessions: 2
      promotion-cooldown: 0s
      dry-run: false
      state-dir: "${CPA_ROOT}/plugins/auto-baseline"
YAML
  chmod 0644 "${file}"
}

pick_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# wait_for_resource_route polls the unauthenticated plugin resource route.
wait_for_resource_route() {
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  until curl --silent --fail --max-time 2 --output /dev/null "${BASE}/v0/resource/plugins/auto-baseline/status"; do
    if [[ "${RUNNER}" == docker ]]; then
      if [[ "$(docker container inspect --format '{{.State.Running}}' "${CONTAINER_ID}" 2>/dev/null || true)" != true ]]; then
        printf 'CPA container exited during startup\n' >&2
        return 1
      fi
    elif ! kill -0 "${CPA_PID}" 2>/dev/null; then
      CPA_PID=""
      if grep -Eq 'address already in use|EADDRINUSE|bind: ' "${LOG_FILE}"; then
        return 2
      fi
      printf 'CPA exited during startup\n' >&2
      return 1
    elif grep -Eq 'address already in use|EADDRINUSE' "${LOG_FILE}"; then
      kill "${CPA_PID}" 2>/dev/null || true
      wait "${CPA_PID}" 2>/dev/null || true
      CPA_PID=""
      return 2
    fi
    if (( SECONDS >= deadline )); then
      printf 'timed out waiting for the plugin resource route\n' >&2
      return 1
    fi
    sleep 0.5
  done
}

# start_binary writes the config for the given port and starts the local
# binary. It returns 0 when CPA is up, 2 when the port was taken (the caller
# retries with a new port: an ephemeral port picked and released can be
# reused by another process before CPA binds it), and 1 otherwise.
start_binary() {
  local port="$1"
  write_config "${port}"
  cp "${TMP_DIR}/config.yaml" "${ARTIFACT_DIR}/config.before.yaml"
  : >"${LOG_FILE}"
  printf 'starting %s on port %s (layout %s)\n' "${CPA_BIN}" "${port}" "${LAYOUT}"
  (
    cd "${TMP_DIR}"
    exec "${CPA_BIN}" -config "${TMP_DIR}/config.yaml" >"${LOG_FILE}" 2>&1
  ) &
  CPA_PID=$!
  BASE="http://127.0.0.1:${port}"
  wait_for_resource_route
}

start_docker() {
  if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    docker pull "${IMAGE}" >/dev/null
  fi
  printf 'image=%s\nimage_id=%s\n' "${IMAGE}" "$(docker image inspect --format '{{.Id}}' "${IMAGE}")" >"${ARTIFACT_DIR}/image.txt"
  write_config 8317
  cp "${TMP_DIR}/config.yaml" "${ARTIFACT_DIR}/config.before.yaml"
  # Let Docker name the volume so cleanup can never remove someone else's.
  PLUGIN_VOLUME="$(docker volume create --label "auto-baseline.smoke-run=${RUN_ID}")"
  CONTAINER_ID="$(docker create \
    --name "auto-baseline-smoke-${RUN_ID}" \
    --publish "127.0.0.1::8317" \
    --mount "type=volume,src=${PLUGIN_VOLUME},dst=${CPA_ROOT}/plugins" \
    --mount "type=bind,src=${TMP_DIR}/config.yaml,dst=${CPA_ROOT}/config.yaml" \
    "${IMAGE}")"
  # Copy the library into the stopped container: CPA never sees a partial write.
  docker cp "${TMP_DIR}/plugins/auto-baseline.so" "${CONTAINER_ID}:${CPA_ROOT}/plugins/auto-baseline.so" >/dev/null
  docker start "${CONTAINER_ID}" >/dev/null
  printf 'started container %s from %s (layout %s)\n' "${CONTAINER_ID:0:12}" "${IMAGE}" "${LAYOUT}"
  local mapping="" deadline=$((SECONDS + TIMEOUT_SECONDS))
  until [[ -n "${mapping}" ]]; do
    mapping="$(docker port "${CONTAINER_ID}" 8317/tcp 2>/dev/null | grep -m1 '^127\.0\.0\.1:' || true)"
    if (( SECONDS >= deadline )); then
      printf 'container port was never published\n' >&2
      return 1
    fi
    [[ -n "${mapping}" ]] || sleep 0.2
  done
  BASE="http://127.0.0.1:${mapping##*:}"
  wait_for_resource_route
}

BASE=""
PORT=""
if [[ "${RUNNER}" == docker ]]; then
  start_docker
elif [[ -n "${CPA_SMOKE_PORT:-}" ]]; then
  PORT="${CPA_SMOKE_PORT}"
  start_binary "${PORT}" || { printf 'CPA did not start on fixed port %s\n' "${PORT}" >&2; exit 1; }
else
  STARTED=false
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    PORT="$(pick_port)"
    rc=0
    start_binary "${PORT}" || rc=$?
    if (( rc == 0 )); then
      STARTED=true
      break
    elif (( rc == 2 )); then
      printf 'port %s was taken before CPA bound it (attempt %s/10); retrying\n' "${PORT}" "${attempt}"
      continue
    else
      exit 1
    fi
  done
  if [[ "${STARTED}" != true ]]; then
    printf 'could not find a free port after 10 attempts\n' >&2
    exit 1
  fi
fi
printf 'plugin resource route is up\n'
refresh_log
grep -m1 '^CLIProxyAPI Version:' "${LOG_FILE}" | tee "${ARTIFACT_DIR}/cpa-version.txt" || true

# mgmt_get fetches a management path into a file. It returns 0 on 200 and 1
# on other transient answers, and aborts the whole run on 401/403: an auth
# failure is never retried.
mgmt_get() {
  local path="$1" out="$2" base="${3:-${BASE}}" code
  code="$(curl --silent --show-error --max-time 5 --output "${out}" --write-out '%{http_code}' \
    -H "Authorization: Bearer ${MGMT_KEY}" "${base}/v0/management${path}" || true)"
  case "${code}" in
    200) return 0 ;;
    401|403) printf 'management API answered %s for %s; stopping without retrying\n' "${code}" "${path}" >&2; exit 1 ;;
    *) return 1 ;;
  esac
}

# cpa_effective prints the Claude header defaults CPA is actually running
# with, from its runtime config (never printing anything else).
cpa_effective() {
  local out="${TMP_DIR}/runtime-config.json"
  mgmt_get /config "${out}" || { printf 'GET /v0/management/config failed\n' >&2; exit 1; }
  python3 - "${out}" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
hd = cfg.get("claude-header-defaults") or {}
print(json.dumps({k: hd.get(k, "") for k in ("user-agent", "package-version", "runtime-version")}, sort_keys=True))
PY
  rm -f "${out}"
}

mgmt_get /plugins/auto-baseline/status "${ARTIFACT_DIR}/status.before.json" || { printf 'status route failed\n' >&2; exit 1; }
python3 - "${ARTIFACT_DIR}/status.before.json" "${FROM}" "${FROM_SOURCE}" "${TARGET}" "${FILE_LAYOUT}" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
want_from, want_source, want_target, want_layout = sys.argv[2:6]
claude = [p for p in s["providers"] if p["provider"] == "claude"][0]
eff = claude["effective_baseline"]
assert eff["version"] == want_from, eff
assert eff["sources"]["user-agent"] == want_source, eff
assert eff["write_target"] == want_target, eff
assert s["config_file"]["layout"] == want_layout, s["config_file"]
assert s["config_file"]["exists"] and s["config_file"]["writable"], s["config_file"]
print("status before: effective claude baseline %s from %s, writes to %s, %s layout, config writable" % (want_from, want_source, want_target, want_layout))
PY
BEFORE_RUNTIME="$(cpa_effective)"
printf 'CPA runtime before: %s\n' "${BEFORE_RUNTIME}" | tee "${ARTIFACT_DIR}/runtime.before.txt"

send_message() {
  local session="$1" tag="$2"
  curl --silent --show-error --max-time 15 --output "${ARTIFACT_DIR}/messages-${tag}-${session:0:8}.json" --write-out '%{http_code}' \
    -X POST "${BASE}/v1/messages" \
    -H 'Authorization: Bearer smoke-key' \
    -H 'Content-Type: application/json' \
    -H "User-Agent: claude-cli/${CANDIDATE_VERSION} (external, sdk-ts, agent-sdk/0.3.170)" \
    -H 'X-Stainless-Package-Version: 0.112.1' \
    -H 'X-Stainless-Runtime-Version: v26.3.0' \
    -H 'X-Stainless-Lang: js' \
    -H 'X-Stainless-Runtime: node' \
    -H 'X-Stainless-OS: Linux' \
    -H 'X-Stainless-Arch: x64' \
    -H 'X-Stainless-Timeout: 600' \
    -H 'x-app: cli' \
    -H 'anthropic-version: 2023-06-01' \
    -H 'anthropic-beta: claude-code-20250219,oauth-2025-04-20' \
    -H "X-Claude-Code-Session-Id: ${session}" \
    --data '{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}' || true
}

SESSION_A="3f6c1a1e-7b6d-4c1e-9a1f-2b3c4d5e6f70"
SESSION_B="9d2e4b6a-1c3f-4e5d-8a7b-6c5d4e3f2a10"
for session in "${SESSION_A}" "${SESSION_B}" "${SESSION_A}"; do
  code="$(send_message "${session}" first)"
  printf 'POST /v1/messages (session %s) -> HTTP %s (failure expected: placeholder credential, closed upstream port)\n' "${session:0:8}" "${code}"
done

PROMOTED="auto-baseline: promoted claude baseline ${FROM} -> ${CANDIDATE_VERSION}"
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until refresh_log; grep -Fq "${PROMOTED}" "${LOG_FILE}"; do
  if (( SECONDS >= DEADLINE )); then
    printf 'no promotion within %s seconds\n' "${TIMEOUT_SECONDS}" >&2
    cat "${TMP_DIR}/config.yaml" >&2
    exit 1
  fi
  sleep 0.5
done
PROMOTION_LINE="$(grep -nF "${PROMOTED}" "${LOG_FILE}" | head -n1 | cut -d: -f1)"
grep -F "${PROMOTED}" "${LOG_FILE}" | head -n1 | grep -Fq "at ${TARGET};" || {
  printf 'promotion was not written to %s:\n%s\n' "${TARGET}" "$(grep -F "${PROMOTED}" "${LOG_FILE}" | head -n1)" >&2
  exit 1
}
printf 'plugin promotion logged through host.log (log line %s), written to %s\n' "${PROMOTION_LINE}" "${TARGET}"

# CPA itself rewrites the plaintext management secret-key to a bcrypt hash at
# startup (internal/config/config_load.go:114-131), so a reload line may exist
# BEFORE the promotion. Require one AFTER the plugin's write.
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until refresh_log; tail -n "+${PROMOTION_LINE}" "${LOG_FILE}" | grep -Eq 'config file changed, reloading|CONFIG RELOAD'; do
  if (( SECONDS >= DEADLINE )); then
    printf 'CPA did not log a config reload after the promotion within %s seconds\n' "${TIMEOUT_SECONDS}" >&2
    exit 1
  fi
  sleep 0.5
done
printf 'CPA logged the config reload after the promotion\n'

# Wait for the plugin to confirm the reload (value-correlated).
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until mgmt_get /plugins/auto-baseline/status "${ARTIFACT_DIR}/status.after.json" \
  && python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); c=[p for p in s["providers"] if p["provider"]=="claude"][0]; sys.exit(0 if c["last_promotion"] and not c["awaiting_reload"] else 1)' "${ARTIFACT_DIR}/status.after.json"; do
  if (( SECONDS >= DEADLINE )); then
    printf 'the plugin never confirmed the reload\n' >&2
    exit 1
  fi
  sleep 0.5
done
python3 - "${ARTIFACT_DIR}/status.after.json" "${FROM}" "${CANDIDATE_VERSION}" "${SOURCE}" "${TARGET}" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
want_from, want_to, want_source, want_target = sys.argv[2:6]
claude = [p for p in s["providers"] if p["provider"] == "claude"][0]
eff, lp = claude["effective_baseline"], claude["last_promotion"]
assert eff["version"] == want_to and eff["explicit_in_config"] is True, eff
assert eff["sources"] == {"user-agent": want_source, "package-version": want_source, "runtime-version": want_source}, eff
assert lp["from"] == want_from and lp["target"] == want_target, lp
assert lp["observations"] == 3 and lp["distinct_sessions"] == 2, lp
unset = lambda v: not v or v.startswith("0001-01-01")
assert not unset(lp.get("confirmed_at")) and unset(lp.get("not_effective_at")), lp
assert "paused" not in claude, claude
assert s["counters"]["accepted"] >= 3, s["counters"]
assert s["backup"]["writable"] is True, s["backup"]
assert s["config_file"]["mode_unsupported"] is False, s["config_file"]
assert s["faulted"] is False, s
assert not s.get("last_error"), s.get("last_error")
print("status after: effective claude baseline %s from %s (%s), last promotion %s -> %s with 3 obs / 2 sessions, reload confirmed" % (want_to, want_source, want_target, want_from, want_to))
PY

# The promotion took effect: CPA's own runtime config carries the tuple.
AFTER_RUNTIME="$(cpa_effective)"
printf 'CPA runtime after:  %s\n' "${AFTER_RUNTIME}" | tee "${ARTIFACT_DIR}/runtime.after.txt"
python3 - "${AFTER_RUNTIME}" "${CANDIDATE_UA}" <<'PY'
import json, sys
got = json.loads(sys.argv[1])
want = {"user-agent": sys.argv[2], "package-version": "0.112.1", "runtime-version": "v26.3.0"}
assert got == want, (got, want)
print("CPA loaded the promoted tuple: the promotion took effect")
PY

# Exactly once: the same fingerprint keeps arriving (cooldown is 0s, so a
# loop would rewrite immediately), and nothing is written again.
SHA_AFTER="$(sha256sum "${TMP_DIR}/config.yaml" | cut -d' ' -f1)"
for session in "${SESSION_A}" "${SESSION_B}" "${SESSION_A}"; do
  send_message "${session}" again >/dev/null
done
sleep 5
refresh_log
PROMOTIONS="$(grep -cF 'auto-baseline: promoted claude baseline' "${LOG_FILE}" || true)"
[[ "${PROMOTIONS}" == 1 ]] || { printf 'expected exactly one promotion, saw %s\n' "${PROMOTIONS}" >&2; exit 1; }
if grep -Fq 'promotion_not_effective' "${LOG_FILE}"; then
  printf 'the plugin reported promotion_not_effective\n' >&2
  exit 1
fi
SHA_LATER="$(sha256sum "${TMP_DIR}/config.yaml" | cut -d' ' -f1)"
[[ "${SHA_AFTER}" == "${SHA_LATER}" ]] || { printf 'config.yaml changed after the promotion settled\n' >&2; exit 1; }
mgmt_get /plugins/auto-baseline/status "${ARTIFACT_DIR}/status.final.json" || { printf 'status route failed\n' >&2; exit 1; }
python3 - "${ARTIFACT_DIR}/status.final.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
assert len([h for h in s["history"] if h["provider"] == "claude"]) == 1, s["history"]
assert s["counters"]["decisions"].get("not_newer_than_baseline", 0) >= 3, s["counters"]
assert "promotion_not_effective" not in s["counters"]["decisions"], s["counters"]
print("exactly once: 3 more identical observations were not_newer_than_baseline, one promotion in history, config.yaml unchanged")
PY

python3 - "${TMP_DIR}/config.yaml" "${LAYOUT}" "${CANDIDATE_UA}" <<'PY'
import sys
text = open(sys.argv[1]).read()
layout, ua = sys.argv[2], sys.argv[3]
lines = text.splitlines()
assert "sdk-ts" not in text, text
assert "# smoke-test config (auto-baseline)" in text, "comment lost"
root_legacy = [l for l in lines if l.startswith("claude-header-defaults:")]
if layout == "v8":
    # Updated in place, 2-space indentation kept: a load-time conflict
    # rewrite by CPA re-indents the file with 4 spaces.
    block = "\n  providers:\n    claude:\n      header-defaults:\n        user-agent: \"%s\"\n        package-version: \"0.112.1\"\n        runtime-version: \"v26.3.0\"\n" % ua
    assert block in text, text
    assert not root_legacy, "a root claude-header-defaults was created beside its v8 counterpart:\n" + text
    assert 'access:\n  api-keys:\n    - "smoke-key"' in text, "unrelated keys changed"
else:
    assert len(root_legacy) == 1, text
    block = text.split("claude-header-defaults:", 1)[1]
    for needle in ['user-agent: "%s"' % ua, 'package-version: "0.112.1"', 'runtime-version: "v26.3.0"']:
        assert "\n  " + needle in block, (needle, text)
    assert "header-defaults:\n" not in text.replace("claude-header-defaults:\n", ""), "a v8 block was created:\n" + text
    if layout == "legacy":
        assert 'api-keys:\n  - "smoke-key"' in text, "unrelated keys changed"
print("config.yaml carries the promoted block where the %s layout requires it; comments, indentation, and other keys preserved" % layout)
PY
fetch_plugin_file config.yaml.auto-baseline.bak "${ARTIFACT_DIR}/config.bak.yaml" || { printf 'backup file missing\n' >&2; exit 1; }
if grep -Fq "${CANDIDATE_UA}" "${ARTIFACT_DIR}/config.bak.yaml"; then
  printf 'backup contains the promoted tuple; it should hold the previous file\n' >&2; exit 1
fi
printf 'backup file holds the pre-promotion config\n'

mgmt_get /plugins/auto-baseline/status/html "${ARTIFACT_DIR}/status.html" || { printf 'HTML status route failed\n' >&2; exit 1; }
grep -Fq '<title>Auto Baseline' "${ARTIFACT_DIR}/status.html" || { printf 'HTML status view malformed\n' >&2; exit 1; }
grep -Fq "writes <code>${TARGET}</code>" "${ARTIFACT_DIR}/status.html" || { printf 'HTML status view lacks the write target\n' >&2; exit 1; }
printf 'HTML status view rendered with sources and write target\n'

if [[ "${BROWSER_PHASE}" != true ]]; then
  exit 0
fi

########################################################################
# --browser phase: plain-HTTP, non-loopback, same-origin trust model.
########################################################################
REMOTE="http://${HOST_IP}:${PORT}"
BROWSER_LOG="${ARTIFACT_DIR}/browser-phase.txt"
: >"${BROWSER_LOG}"
say() { printf '%s\n' "$*" | tee -a "${BROWSER_LOG}"; }
say "browser phase: CPA reachable at ${REMOTE} (non-loopback, plain HTTP)"

if command -v agent-browser >/dev/null 2>&1; then
  say "agent-browser is installed but this script does not drive it (no browser was available to validate a command sequence when the phase was written); running the curl checks"
else
  say "agent-browser is not installed; running the curl checks (they reproduce the exact header shape a browser sends to a plain-HTTP non-loopback origin: Origin present, no Sec-Fetch-Site)"
fi

# (1) Redacted sidebar page: 200, CSP with frame-ancestors 'self', "redacted view".
HDRS="${ARTIFACT_DIR}/resource.headers"
curl --silent --show-error --max-time 10 --dump-header "${HDRS}" --output "${ARTIFACT_DIR}/resource.html" "${REMOTE}/v0/resource/plugins/auto-baseline/status"
grep -qiE '^HTTP/[0-9.]+ 200' "${HDRS}" || { say "resource route did not return 200"; cat "${HDRS}" >&2; exit 1; }
grep -qiE "^content-security-policy:.*frame-ancestors 'self'" "${HDRS}" || { say "CSP missing frame-ancestors 'self'"; cat "${HDRS}" >&2; exit 1; }
grep -qiE '^referrer-policy: *no-referrer' "${HDRS}" || { say "Referrer-Policy missing"; exit 1; }
grep -qiE '^x-content-type-options: *nosniff' "${HDRS}" || { say "X-Content-Type-Options missing"; exit 1; }
grep -Fq 'redacted view' "${ARTIFACT_DIR}/resource.html" || { say "resource page lacks the redacted pill"; exit 1; }
if grep -Fq "${TMP_DIR}" "${ARTIFACT_DIR}/resource.html"; then say "resource page leaks a filesystem path"; exit 1; fi
say "(1) GET ${REMOTE}/v0/resource/plugins/auto-baseline/status -> 200, CSP frame-ancestors 'self', redacted view, no paths"

# (2) The upgrade fetch the page's script performs: same-origin GET of the
# authenticated view with the recovered key -> 200 and "authenticated view".
code="$(curl --silent --show-error --max-time 10 -H "Authorization: Bearer ${MGMT_KEY}" -H "Origin: ${REMOTE}" \
  --output "${ARTIFACT_DIR}/upgraded.html" --write-out '%{http_code}' "${REMOTE}/v0/management/plugins/auto-baseline/status/html" || true)"
[[ "${code}" == 200 ]] || { say "upgrade fetch -> HTTP ${code}; stopping without retrying"; exit 1; }
grep -Fq 'authenticated view' "${ARTIFACT_DIR}/upgraded.html" || { say "upgrade fetch did not return the authenticated view"; exit 1; }
grep -Fq 'Switch to dry-run' "${ARTIFACT_DIR}/upgraded.html" || { say "authenticated view lacks the dry-run switch"; exit 1; }
say "(2) same-origin upgrade fetch with the recovered key -> authenticated view rendered over plain HTTP"

# (3) Dry-run switch through the CSRF gate with Origin only (no Sec-Fetch-Site).
post_action() { # route body extra-curl-args...
  local route="$1" body="$2"; shift 2
  curl --silent --show-error --max-time 10 --output "${ARTIFACT_DIR}/last-action.json" --write-out '%{http_code}' \
    -X POST -H "Authorization: Bearer ${MGMT_KEY}" -H 'X-Auto-Baseline-Action: 1' -H 'Content-Type: application/json' \
    "$@" --data "${body}" "${REMOTE}/v0/management/plugins/auto-baseline${route}" || true
}
wait_dry_run() { # want: true|false
  local want="$1" deadline=$((SECONDS + TIMEOUT_SECONDS))
  until mgmt_get /plugins/auto-baseline/status "${ARTIFACT_DIR}/status.browser.json" "${REMOTE}" \
    && python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["dry_run"] is (sys.argv[2]=="true") and s["dry_run_awaiting_reload"] is False else 1)' "${ARTIFACT_DIR}/status.browser.json" "${want}"; do
    if (( SECONDS >= deadline )); then say "status did not show dry_run ${want} after CPA reload"; exit 1; fi
    sleep 0.5
  done
}
code="$(post_action /dry-run '{"enabled":true}' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "dry-run toggle with plain-http Origin -> HTTP ${code}: $(cat "${ARTIFACT_DIR}/last-action.json")"; exit 1; }
say "(3a) POST /dry-run {enabled:true} with Origin ${REMOTE} and no Sec-Fetch-Site -> 200: $(cat "${ARTIFACT_DIR}/last-action.json")"
grep -Fq 'dry-run: true' "${TMP_DIR}/config.yaml" || { say "config.yaml does not carry dry-run: true"; exit 1; }
wait_dry_run true
say "(3b) CPA reloaded; status shows dry_run=true, awaiting_reload=false"
code="$(post_action /dry-run '{"enabled":false}' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "switch back to live writes -> HTTP ${code}"; exit 1; }
wait_dry_run false
say "(3c) POST /dry-run {enabled:false} -> 200; CPA reloaded; status shows dry_run=false (live writes)"

# (4) Clear pending with the same header shape -> 200; hostile shapes -> 403.
# The 403s below come from the plugin's CSRF gate (the key is valid), not
# from CPA's management-key check, and each is sent exactly once.
code="$(post_action /reset '' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "reset with plain-http Origin -> HTTP ${code}"; exit 1; }
say "(4) POST /reset with Origin ${REMOTE} and no Sec-Fetch-Site -> 200"
code="$(post_action /reset '' -H 'Origin: https://evil.example')"
[[ "${code}" == 403 ]] || { say "https Origin without fetch metadata -> HTTP ${code}, want 403"; exit 1; }
say "(4b) POST /reset with Origin https://evil.example -> 403"
code="$(post_action /reset '' -H "Origin: ${REMOTE}" -H 'Sec-Fetch-Site: cross-site')"
[[ "${code}" == 403 ]] || { say "Sec-Fetch-Site cross-site -> HTTP ${code}, want 403"; exit 1; }
say "(4c) POST /reset with Sec-Fetch-Site: cross-site -> 403"
code="$(curl --silent --max-time 10 --output /dev/null --write-out '%{http_code}' -X POST -H "Authorization: Bearer ${MGMT_KEY}" -H "Origin: ${REMOTE}" "${REMOTE}/v0/management/plugins/auto-baseline/reset" || true)"
[[ "${code}" == 403 ]] || { say "reset without the action header -> HTTP ${code}, want 403"; exit 1; }
say "(4d) POST /reset without X-Auto-Baseline-Action -> 403"
say "browser phase PASSED (curl checks)"
