#!/usr/bin/env bash
# End-to-end smoke test against a locally built CLIProxyAPI binary.
#
# Usage:
#   scripts/smoke-test.sh <path-to-CLIProxyAPI-binary> [path-to-auto-baseline.so] [--browser]
#
# --browser adds a phase that proves the browser trust model over PLAIN HTTP
# on a NON-LOOPBACK address: CPA is bound to 0.0.0.0 with
# remote-management.allow-remote: true (management key "smoke-mgmt"; the
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
#   CPA_SMOKE_PORT              fixed listen port (default: a free port)
#   CPA_SMOKE_TIMEOUT_SECONDS   startup / reload wait budget (default 60)
#   CPA_SMOKE_ARTIFACT_DIR      where to keep evidence (default dist/smoke/<run-id>)
#
# Flow:
#   1. write a temp config.yaml with the plugin enabled (min-observations 3,
#      min-distinct-sessions 2, promotion-cooldown 0s) and NO claude-header-defaults;
#   2. copy the plugin .so into <tmp>/plugins/ and start CPA with -config;
#   3. POST /v1/messages three times with genuine-looking Claude Code 2.1.258
#      headers from two distinct session IDs. The config carries a placeholder
#      claude-api-key whose base-url is a closed local port, so the model is
#      routable (interceptors only run for routable models), the interceptor
#      observes the headers before credential selection, and the upstream call
#      then fails locally with connection refused. No traffic leaves the host;
#   4. assert config.yaml now carries the promoted claude-header-defaults and
#      that CPA logged the config reload;
#   5. assert the status route reports the new effective baseline.
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BROWSER_PHASE=false
POSITIONAL=()
for arg in "$@"; do
  case "${arg}" in
    --browser) BROWSER_PHASE=true ;;
    *) POSITIONAL+=("${arg}") ;;
  esac
done
CPA_BIN="${POSITIONAL[0]:-}"
PLUGIN_INPUT="${POSITIONAL[1]:-${ROOT_DIR}/auto-baseline.so}"
TIMEOUT_SECONDS="${CPA_SMOKE_TIMEOUT_SECONDS:-60}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
ARTIFACT_DIR="${CPA_SMOKE_ARTIFACT_DIR:-${ROOT_DIR}/dist/smoke/${RUN_ID}}"

if [[ -z "${CPA_BIN}" ]]; then
  printf 'usage: %s <CLIProxyAPI binary> [auto-baseline.so] [--browser]\n' "$0" >&2
  exit 2
fi
for command_name in curl python3 realpath; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    printf 'required command not found: %s\n' "${command_name}" >&2
    exit 2
  fi
done
if [[ ! -x "${CPA_BIN}" ]]; then
  printf 'CPA binary not executable: %s\n' "${CPA_BIN}" >&2
  exit 2
fi
if [[ ! -f "${PLUGIN_INPUT}" ]]; then
  printf 'plugin library not found: %s (build it with: make build)\n' "${PLUGIN_INPUT}" >&2
  exit 2
fi
CPA_BIN="$(realpath "${CPA_BIN}")"
PLUGIN_INPUT="$(realpath "${PLUGIN_INPUT}")"

TMP_DIR=""
CPA_PID=""
LOG_FILE="${ARTIFACT_DIR}/cpa.log"
BIND_HOST="127.0.0.1"
ALLOW_REMOTE=false
HOST_IP=""
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

# shellcheck disable=SC2329
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
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
    cp -f "${TMP_DIR}/plugins/auto-baseline/config.yaml.auto-baseline.bak" "${ARTIFACT_DIR}/config.bak.yaml" 2>/dev/null
    cp -f "${TMP_DIR}/plugins/auto-baseline/state.json" "${ARTIFACT_DIR}/state.json" 2>/dev/null
    rm -rf -- "${TMP_DIR}"
  fi
  if (( status != 0 )); then
    printf 'smoke test FAILED; evidence: %s\n' "${ARTIFACT_DIR}" >&2
    if [[ -s "${LOG_FILE}" ]]; then
      printf '%s\n' '--- CPA log (last 80 lines) ---' >&2
      tail -n 80 "${LOG_FILE}" >&2
    fi
  else
    printf 'smoke test PASSED; evidence retained at %s\n' "${ARTIFACT_DIR}"
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

write_config() {
  local port="$1"
  cat >"${TMP_DIR}/config.yaml" <<YAML
# smoke-test config (auto-baseline)
host: "${BIND_HOST}"
port: ${port}
auth-dir: "${TMP_DIR}/auth"
api-keys:
  - "smoke-key"
debug: true
logging-to-file: false
remote-management:
  allow-remote: ${ALLOW_REMOTE}
  secret-key: "smoke-mgmt"
# A placeholder Claude credential is REQUIRED for the interceptor to fire:
# CPA resolves the model's provider (handlers_execution.go:54) before it runs
# request interceptors (:89); with no Claude credential no Claude model is
# routable and the request is rejected with HTTP 400 before interception.
# The base-url points at a closed local port so no traffic leaves the host;
# the upstream call fails with connection refused after the observation.
claude-api-key:
  - api-key: "sk-ant-smoke-placeholder"
    base-url: "http://127.0.0.1:9"
plugins:
  enabled: true
  dir: "${TMP_DIR}/plugins"
  configs:
    auto-baseline:
      enabled: true
      min-observations: 3
      # The smoke requests carry two distinct X-Claude-Code-Session-Id values,
      # so the stricter 2-session rule (default is 1) is exercised here.
      min-distinct-sessions: 2
      promotion-cooldown: 0s
      dry-run: false
      state-dir: "${TMP_DIR}/plugins/auto-baseline"
YAML
}

pick_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# start_cpa writes the config for the given port, starts CPA, and waits until
# the plugin resource route answers. It returns 0 when CPA is up, 2 when CPA
# exited because the port was already taken (the caller retries with a new
# port: an ephemeral port picked and released can be reused by another process
# before CPA binds it), and 1 for any other failure.
start_cpa() {
  local port="$1"
  write_config "${port}"
  cp "${TMP_DIR}/config.yaml" "${ARTIFACT_DIR}/config.before.yaml"
  : >"${LOG_FILE}"
  printf 'starting %s on port %s\n' "${CPA_BIN}" "${port}"
  (
    cd "${TMP_DIR}"
    exec "${CPA_BIN}" -config "${TMP_DIR}/config.yaml" >"${LOG_FILE}" 2>&1
  ) &
  CPA_PID=$!
  BASE="http://127.0.0.1:${port}"
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  until curl --silent --fail --max-time 2 --output /dev/null "${BASE}/v0/resource/plugins/auto-baseline/status"; do
    if ! kill -0 "${CPA_PID}" 2>/dev/null; then
      CPA_PID=""
      if grep -Eq 'address already in use|EADDRINUSE|bind: ' "${LOG_FILE}"; then
        return 2
      fi
      printf 'CPA exited during startup\n' >&2
      return 1
    fi
    if grep -Eq 'address already in use|EADDRINUSE' "${LOG_FILE}"; then
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
  return 0
}

BASE=""
if [[ -n "${CPA_SMOKE_PORT:-}" ]]; then
  PORT="${CPA_SMOKE_PORT}"
  start_cpa "${PORT}" || { printf 'CPA did not start on fixed port %s\n' "${PORT}" >&2; exit 1; }
else
  STARTED=false
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    PORT="$(pick_port)"
    rc=0
    start_cpa "${PORT}" || rc=$?
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

STATUS_BEFORE="$(curl --silent --fail --max-time 5 -H 'Authorization: Bearer smoke-mgmt' "${BASE}/v0/management/plugins/auto-baseline/status")"
printf '%s\n' "${STATUS_BEFORE}" >"${ARTIFACT_DIR}/status.before.json"
python3 - "${STATUS_BEFORE}" <<'PY'
import json, sys
s = json.loads(sys.argv[1])
claude = [p for p in s["providers"] if p["provider"] == "claude"][0]
assert claude["effective_baseline"]["version"] == "2.1.220", claude
assert claude["effective_baseline"]["explicit_in_config"] is False, claude
assert s["config_file"]["exists"] and s["config_file"]["writable"], s["config_file"]
print("status before: effective claude baseline 2.1.220 (compiled default), config writable")
PY

send_message() {
  local session="$1"
  curl --silent --show-error --max-time 15 --output "${ARTIFACT_DIR}/messages-${session}.json" --write-out '%{http_code}' \
    -X POST "${BASE}/v1/messages" \
    -H 'Authorization: Bearer smoke-key' \
    -H 'Content-Type: application/json' \
    -H 'User-Agent: claude-cli/2.1.258 (external, sdk-ts, agent-sdk/0.3.170)' \
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
  code="$(send_message "${session}")"
  printf 'POST /v1/messages (session %s) -> HTTP %s (failure expected: placeholder credential, closed upstream port)\n' "${session:0:8}" "${code}"
done

DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until grep -Fq 'claude-header-defaults:' "${TMP_DIR}/config.yaml"; do
  if (( SECONDS >= DEADLINE )); then
    printf 'config.yaml was not updated within %s seconds\n' "${TIMEOUT_SECONDS}" >&2
    cat "${TMP_DIR}/config.yaml" >&2
    exit 1
  fi
  sleep 0.5
done

python3 - "${TMP_DIR}/config.yaml" <<'PY'
import sys
text = open(sys.argv[1]).read()
block = text.split("claude-header-defaults:", 1)[1]
for needle in ['user-agent: "claude-cli/2.1.258 (external, cli)"', 'package-version: "0.112.1"', 'runtime-version: "v26.3.0"']:
    assert needle in block, (needle, text)
assert "sdk-ts" not in text, text
assert "# smoke-test config (auto-baseline)" in text, "comment lost"
assert 'api-keys:\n  - "smoke-key"' in text, "unrelated keys changed"
print("config.yaml promoted to claude-cli/2.1.258 with package 0.112.1 / runtime v26.3.0; comments and other keys preserved")
PY
BACKUP_FILE="${TMP_DIR}/plugins/auto-baseline/config.yaml.auto-baseline.bak"
[[ -f "${BACKUP_FILE}" ]] || { printf 'backup file missing at %s\n' "${BACKUP_FILE}" >&2; exit 1; }
if grep -Fq 'claude-header-defaults' "${BACKUP_FILE}"; then
  printf 'backup contains the promoted block; it should hold the previous file\n' >&2; exit 1
fi
printf 'backup file holds the pre-promotion config\n'

PROMOTION_LINE="$(grep -nF 'auto-baseline: promoted claude baseline 2.1.220 -> 2.1.258' "${LOG_FILE}" | head -n1 | cut -d: -f1 || true)"
[[ -n "${PROMOTION_LINE}" ]] || { printf 'plugin promotion log line missing\n' >&2; exit 1; }
printf 'plugin promotion logged through host.log (log line %s)\n' "${PROMOTION_LINE}"

# CPA itself rewrites remote-management.secret-key to a bcrypt hash at startup
# (internal/config/config_load.go:104-113), so a reload line may exist BEFORE the
# promotion. Require one AFTER the plugin's write.
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until tail -n "+${PROMOTION_LINE}" "${LOG_FILE}" | grep -Eq 'config file changed, reloading|CONFIG RELOAD'; do
  if (( SECONDS >= DEADLINE )); then
    printf 'CPA did not log a config reload after the promotion within %s seconds\n' "${TIMEOUT_SECONDS}" >&2
    exit 1
  fi
  sleep 0.5
done
printf 'CPA logged the config reload after the promotion\n'

sleep 1
STATUS_AFTER="$(curl --silent --fail --max-time 5 -H 'Authorization: Bearer smoke-mgmt' "${BASE}/v0/management/plugins/auto-baseline/status")"
printf '%s\n' "${STATUS_AFTER}" >"${ARTIFACT_DIR}/status.after.json"
python3 - "${STATUS_AFTER}" <<'PY'
import json, sys
s = json.loads(sys.argv[1])
claude = [p for p in s["providers"] if p["provider"] == "claude"][0]
assert claude["effective_baseline"]["version"] == "2.1.258", claude["effective_baseline"]
assert claude["effective_baseline"]["explicit_in_config"] is True, claude["effective_baseline"]
assert claude["last_promotion"]["from"] == "2.1.220", claude["last_promotion"]
assert claude["last_promotion"]["observations"] == 3 and claude["last_promotion"]["distinct_sessions"] == 2, claude["last_promotion"]
assert s["counters"]["accepted"] >= 3, s["counters"]
assert s["backup"]["writable"] is True, s["backup"]
assert s["config_file"]["mode_unsupported"] is False, s["config_file"]
assert s["faulted"] is False, s
assert not s.get("last_error"), s.get("last_error")
assert claude["awaiting_reload"] is False, "promotion still awaiting reload after CPA reconfigured the plugin: %r" % claude
print("status after: effective claude baseline 2.1.258 (explicit), last promotion 2.1.220 -> 2.1.258 with 3 obs / 2 sessions, reload confirmed")
PY

curl --silent --fail --max-time 5 -H 'Authorization: Bearer smoke-mgmt' \
  "${BASE}/v0/management/plugins/auto-baseline/status/html" >"${ARTIFACT_DIR}/status.html"
grep -Fq '<title>Auto Baseline' "${ARTIFACT_DIR}/status.html" || { printf 'HTML status view malformed\n' >&2; exit 1; }
printf 'HTML status view rendered\n'

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
curl --silent --show-error --fail --max-time 10 -H 'Authorization: Bearer smoke-mgmt' -H "Origin: ${REMOTE}" \
  --output "${ARTIFACT_DIR}/upgraded.html" "${REMOTE}/v0/management/plugins/auto-baseline/status/html"
grep -Fq 'authenticated view' "${ARTIFACT_DIR}/upgraded.html" || { say "upgrade fetch did not return the authenticated view"; exit 1; }
grep -Fq 'Switch to dry-run' "${ARTIFACT_DIR}/upgraded.html" || { say "authenticated view lacks the dry-run switch"; exit 1; }
say "(2) same-origin upgrade fetch with the recovered key -> authenticated view rendered over plain HTTP"

# (3) Dry-run switch through the CSRF gate with Origin only (no Sec-Fetch-Site).
post_action() { # route body extra-curl-args...
  local route="$1" body="$2"; shift 2
  curl --silent --show-error --max-time 10 --output "${ARTIFACT_DIR}/last-action.json" --write-out '%{http_code}' \
    -X POST -H 'Authorization: Bearer smoke-mgmt' -H 'X-Auto-Baseline-Action: 1' -H 'Content-Type: application/json' \
    "$@" --data "${body}" "${REMOTE}/v0/management/plugins/auto-baseline${route}" || true
}
code="$(post_action /dry-run '{"enabled":true}' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "dry-run toggle with plain-http Origin -> HTTP ${code}: $(cat "${ARTIFACT_DIR}/last-action.json")"; exit 1; }
say "(3a) POST /dry-run {enabled:true} with Origin ${REMOTE} and no Sec-Fetch-Site -> 200: $(cat "${ARTIFACT_DIR}/last-action.json")"
grep -Fq 'dry-run: true' "${TMP_DIR}/config.yaml" || { say "config.yaml does not carry dry-run: true"; exit 1; }
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until curl --silent --fail --max-time 5 -H 'Authorization: Bearer smoke-mgmt' "${REMOTE}/v0/management/plugins/auto-baseline/status" \
  | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if s["dry_run"] is True and s["dry_run_awaiting_reload"] is False else 1)'; do
  if (( SECONDS >= DEADLINE )); then say "status did not show dry_run true after CPA reload"; exit 1; fi
  sleep 0.5
done
say "(3b) CPA reloaded; status shows dry_run=true, awaiting_reload=false"
code="$(post_action /dry-run '{"enabled":false}' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "switch back to live writes -> HTTP ${code}"; exit 1; }
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
until curl --silent --fail --max-time 5 -H 'Authorization: Bearer smoke-mgmt' "${REMOTE}/v0/management/plugins/auto-baseline/status" \
  | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if s["dry_run"] is False and s["dry_run_awaiting_reload"] is False else 1)'; do
  if (( SECONDS >= DEADLINE )); then say "status did not show dry_run false after CPA reload"; exit 1; fi
  sleep 0.5
done
say "(3c) POST /dry-run {enabled:false} -> 200; CPA reloaded; status shows dry_run=false (live writes)"

# (4) Clear pending with the same header shape -> 200; hostile shapes -> 403.
code="$(post_action /reset '' -H "Origin: ${REMOTE}")"
[[ "${code}" == 200 ]] || { say "reset with plain-http Origin -> HTTP ${code}"; exit 1; }
say "(4) POST /reset with Origin ${REMOTE} and no Sec-Fetch-Site -> 200"
code="$(post_action /reset '' -H 'Origin: https://evil.example')"
[[ "${code}" == 403 ]] || { say "https Origin without fetch metadata -> HTTP ${code}, want 403"; exit 1; }
say "(4b) POST /reset with Origin https://evil.example -> 403"
code="$(post_action /reset '' -H "Origin: ${REMOTE}" -H 'Sec-Fetch-Site: cross-site')"
[[ "${code}" == 403 ]] || { say "Sec-Fetch-Site cross-site -> HTTP ${code}, want 403"; exit 1; }
say "(4c) POST /reset with Sec-Fetch-Site: cross-site -> 403"
code="$(curl --silent --max-time 10 --output /dev/null --write-out '%{http_code}' -X POST -H 'Authorization: Bearer smoke-mgmt' -H "Origin: ${REMOTE}" "${REMOTE}/v0/management/plugins/auto-baseline/reset" || true)"
[[ "${code}" == 403 ]] || { say "reset without the action header -> HTTP ${code}, want 403"; exit 1; }
say "(4d) POST /reset without X-Auto-Baseline-Action -> 403"
say "browser phase PASSED (curl checks)"
