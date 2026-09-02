# Auto Baseline for CLIProxyAPI

`auto-baseline` is a native CLIProxyAPI (CPA) plugin that keeps CPA's measured Claude Code and Codex CLI client-fingerprint baselines current automatically. It watches the genuine headers of every inbound request, learns the newest authentic client version (together with the package-version and runtime-version that arrived on the same request), and, once a quorum of observations agrees, writes that tuple into `claude-header-defaults` / `codex-header-defaults` in CPA's `config.yaml`. CPA hot-reloads the file, so outbound requests stop being rewritten down to a stale compiled-in version such as `claude-cli/2.1.220`.

The plugin never changes outbound headers directly. For the flows this plugin targets (Claude OAuth tokens, credentials with the `claude-code-cli` fingerprint profile, and every cloaked request) CPA normalizes the software tuple against the configured baseline *after* request interceptors run, so an interceptor cannot raise the outbound version; caller-owned API-key flows already forward the client's real headers and need no help (see [docs/architecture.md](docs/architecture.md), section 2.2). Editing the config file is therefore the right lever, and it is the same file CPA's own management console edits.

## How it works

1. **Observe.** The plugin registers the `request_interceptor` capability and receives the inbound client headers before credential selection. It classifies each request from headers only, never bodies, and always answers "no changes".
2. **Validate.** A Claude request counts only if it looks like Claude Code end to end: `User-Agent: claude-cli/<M.m.p> (external, <entrypoint>[, agent-sdk/x.y.z])`, `x-app: cli`, `anthropic-version: 2023-06-01`, `X-Stainless-Lang: js`, `X-Stainless-Runtime: node`, well-formed `X-Stainless-Package-Version` and `X-Stainless-Runtime-Version`, non-empty OS/Arch, and (by default) the `claude-code-20250219` beta. A Codex request counts if the UA is `codex_cli_rs/x.y.z` or `codex-tui/x.y.z` and an `Originator` header is present.
3. **Quorum.** The same fingerprint tuple must be seen `min-observations` times from `min-distinct-sessions` distinct client session IDs inside `observation-window`. Requests without a session header are anonymous: they count toward observations but never as a session. Only tuples strictly newer than the baseline currently on disk are tracked; nothing below `claude-min-version` / `codex-min-version` is ever written, and a provider whose explicit on-disk `user-agent` does not parse is never touched.
4. **Promote.** A background worker re-reads `config.yaml`, re-checks "strictly newer than what is on disk right now", edits only the baseline keys with the yaml.v3 node API (comments, order and every other key are preserved; aliases, merge keys, sequences and multi-document files are refused rather than rewritten), copies the previous file to `<backup-dir>/config.yaml.auto-baseline.bak`, re-hashes the file, and rewrites the existing inode in place (write, then truncate, then fsync). `dry-run: true` logs what would be written instead. After a real write the promotion is shown as "awaiting reload" until CPA's hot reload is observed.
5. **Persist.** Pending evidence, promotion history and counters live in `<state-dir>/state.json` and survive container restarts.

The promoted Claude tuple is always written in canonical CLI form (`claude-cli/2.1.258 (external, cli)` even when learned from an `sdk-ts` Agent SDK host), with `package-version` and `runtime-version` copied from the same request. `os`, `arch`, `timeout`, `timezone` and `stabilize-device-profile` are never touched. For Codex the full observed UA string is written, because CPA injects it verbatim.

## Requirements and platform support

- A CLIProxyAPI build with native plugin ABI v1 / RPC schema 4 (audited against `v7.2.146-3-g81e1b53`).
- CPA must be able to write its own `config.yaml` (it already does so for the management console and for hashing `remote-management.secret-key`).
- Persistent access to the plugin directory (the default `state-dir` lives inside it).
- Go 1.26.0 and a native C toolchain only when building from source.

Release targets are: `linux_amd64`, `linux_arm64` (manylinux2014, GLIBC <= 2.17), `darwin_amd64`, `darwin_arm64`, `windows_amd64`. The library is `auto-baseline.so` / `.dylib` / `.dll`.

## Installation

- [Docker Compose and manual installation](docs/install-docker-compose.md)
- [Custom Plugin Store install, update and uninstall](docs/custom-plugin-store.md)
- [Architecture, safety rules, threat model and limitations](docs/architecture.md)
- [Troubleshooting](docs/troubleshooting.md)

Start with `dry-run: true`, watch the status page until the expected candidate reaches quorum, then switch to `dry-run: false`. A complete example is in [`config.example.yaml`](config.example.yaml).

**Codex caveat.** A learned `codex-header-defaults.user-agent` has no effect until you set `codex.disable-codex-cloaking: true`; CPA otherwise forces its compiled Codex UA on every outbound request. The status page warns while the flag is off.

**Compiled-default caveat.** When `config.yaml` omits a baseline field the plugin assumes the compiled default of the audited CPA build (shown as "Assumed CPA build" in status). After upgrading CPA to a build with a newer compiled default, raise `claude-min-version` / `codex-min-version` to that build's default (or set an explicit baseline) so the plugin cannot treat the older value as the effective baseline and implicitly write something CPA already exceeds. Setting `require-explicit-baseline: true` removes the guesswork entirely: the plugin then only ever raises a baseline you wrote down.

**Deployment-mode caveat.** When CPA loads its configuration from a Home control plane, Postgres, an object store, or a git store, the local `config.yaml` is not the effective configuration. The plugin detects those modes (`-home-jwt`/`HOME_JWT`, `PGSTORE_DSN`, `OBJECTSTORE_ENDPOINT`, `GITSTORE_GIT_URL`), marks `config_file.mode_unsupported` in status, and disables automatic writes unless `config-path` is set explicitly. See [troubleshooting](docs/troubleshooting.md) for the host-side alternative.

## Configuration reference

All keys live under `plugins.configs.auto-baseline`.

| Field | Default | Meaning |
| --- | ---: | --- |
| `enabled` | `false` | Enables the plugin. |
| `priority` | host-owned | CPA plugin load/order priority; unrelated to learning. |
| `dry-run` | `false` | Compute, log and report promotions without writing `config.yaml`. Recommended `true` for first install. |
| `config-path` | discovered | Path of CPA's `config.yaml`. Default: the `-config` flag of the running CPA process (Linux, via `/proc/self/cmdline`), else `<cwd>/config.yaml` (`/CLIProxyAPI/config.yaml` in the official image). |
| `state-dir` | `plugins/auto-baseline` | Directory for `state.json`; relative paths resolve against the CPA working directory. |
| `backup-dir` | = `state-dir` | Directory that receives `config.yaml.auto-baseline.bak` before each write. Must be writable or nothing is promoted. |
| `manage-claude` | `true` | Learn and promote `claude-header-defaults`. |
| `manage-codex` | `true` | Learn and promote `codex-header-defaults.user-agent`. |
| `claude-entrypoints` | `[cli, sdk-cli, claude-vscode, sdk-ts, sdk-py]` | Entrypoints whose fingerprints may be learned. |
| `require-claude-code-beta` | `true` | Require `claude-code-20250219` in `anthropic-beta`. The plugin sees the *inbound* header: a count_tokens or helper request that arrives without the beta is rejected under `claude_code_beta_missing` and does not count (CPA itself adds the beta on the *outbound* count_tokens request, `claude_executor_request.go:209-217,796`, which the plugin never sees). Requests that carry it count normally. |
| `min-observations` | `3` | Observations of one identical tuple needed inside the window. |
| `min-distinct-sessions` | `1` | Distinct named session IDs those observations must span. Anonymous requests (no session header) never count as a session, so `1` means "observation count only". Set `2` when clients send `X-Claude-Code-Session-Id` (Claude Code does) or a Codex `Session_id`/`Thread-Id`. |
| `observation-window` | `24h` | Age limit for an observation to count. |
| `promotion-cooldown` | `60s` | Minimum spacing between config writes. |
| `claude-min-version` | `2.1.220` | Never write a Claude baseline below this (compiled default of the audited CPA). |
| `codex-min-version` | `0.146.0` | Never write a Codex baseline below this. |
| `require-explicit-baseline` | `false` | Never promote a provider whose on-disk baseline is implicit (compiled default assumed); refusals are counted under `baseline_implicit`. The safe choice after a CPA upgrade whose compiled defaults the plugin does not know. |
| `display-timezone` | `UTC` | IANA zone or `local` for timestamps on the HTML status view only. |

Durations accept Go strings (`24h`, `60s`) or bare integers (seconds). Unknown or misspelled keys (for example `dry_run`) are rejected at registration so a typo cannot silently leave a default in place.

## Status and management routes

| Route | Access | Behavior |
| --- | --- | --- |
| `GET /v0/management/plugins/auto-baseline/status` | Authenticated Management API | JSON snapshot: effective baselines, pending candidates with observation and session counts, last promotion, history, counters, config/state file status, warnings. |
| `GET /v0/management/plugins/auto-baseline/status/html` | Authenticated Management API | Browser view of the same snapshot with a "Clear pending candidates" action. |
| `POST /v0/management/plugins/auto-baseline/observe` | Authenticated + `X-Auto-Baseline-Request: 1` | Host reporting: `{provider, user_agent, package_version, runtime_version, os, arch, session_id, force}` goes through the same validation and quorum as an inbound request; `force: true` queues an immediate promotion after validation (never below the on-disk baseline or the floor; one queued entry per provider, executed by the promotion worker). Returns `202` when accepted (`queued: true` for a force), `422` with a reason bucket when rejected. |
| `POST /v0/management/plugins/auto-baseline/reset` | Authenticated + `X-Auto-Baseline-Request: 1` | Clears pending candidates; baselines and history are kept. |
| `GET /v0/resource/plugins/auto-baseline/status` | Unauthenticated resource route (sidebar menu "Auto Baseline") | Static read-only shell; upgrades itself client-side to the authenticated view when the browser already holds a same-origin management session. Returns 404 when CPA runs in Home mode (`internal/api/server_management.go:281` refuses every plugin resource route there); the authenticated routes still work. |

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'

curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/status | jq .

# Report a fingerprint captured on the host (e.g. from a header dump)
curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H "X-Auto-Baseline-Request: 1" -H "Content-Type: application/json" \
  --data '{"provider":"claude","user_agent":"claude-cli/2.1.258 (external, cli)","package_version":"0.112.1","runtime_version":"v26.3.0","os":"Linux","arch":"x64","session_id":"host-report"}' \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/observe
```

Mutating routes require the custom header. When a browser sends fetch metadata, only `Sec-Fetch-Site: same-origin` or `none` is accepted. Over plain HTTP on a non-loopback host browsers send no fetch metadata, so a well-formed `http://` `Origin` is accepted there (mixed-content blocking means only a plain-HTTP page can post to a plain-HTTP server) while `https://`, `null`, or malformed origins are rejected. Non-browser clients may omit both. No route or log ever includes API keys, tokens, request bodies, session IDs or the contents of `config.yaml` beyond the managed baseline values.

## Write safety

- Only `claude-header-defaults.{user-agent,package-version,runtime-version}` and `codex-header-defaults.user-agent` are ever written. A file with a duplicate mapping key is refused (`duplicate_key`): CPA's own decoder rejects such a file, so it could never be hot-reloaded. A target that is a non-empty scalar, a sequence, an alias, reached through a YAML merge key (`<<`, recognized by its `!!merge` tag; a quoted `"<<"` is an ordinary key), part of an alias/merge cycle, or part of a multi-document file is refused (`unsupported_config_shape` / `multi_document_config`) rather than rewritten. Reads resolve aliases and merge keys with a cycle guard, so a malformed file cannot crash CPA.
- Every write is preceded by a fresh read and a "strictly newer than on disk" check; the file hash is compared again after the backup and immediately before the write, and the read-modify-write is retried (up to 3 times) if another writer got there first. This narrows but cannot eliminate the race: other writers do not take a lock.
- Writes rewrite the existing inode in place (write the new bytes, truncate to the new length, fsync, close), so a Docker single-file bind mount works and the file is never empty on disk between steps. If a write fails midway the previous bytes are written back best-effort. Crash atomicity still cannot be guaranteed on a single-file bind mount (no rename is possible); the backup exists for manual recovery.
- The previous file is copied to `<backup-dir>/config.yaml.auto-baseline.bak` (mode 0600) first; an unwritable backup dir blocks promotion.
- `Stop`/quiesce is a write barrier: in-flight workers and timers are waited for, and no write can start or finish afterwards. A reconfigure that changes any write-affecting field drains the same way before the new config takes effect, and every attempt re-checks its config generation immediately before writing.
- The fresh on-disk read before every write must also show `plugins.enabled: true` and `plugins.configs.auto-baseline.enabled: true` (`plugin_disabled_on_disk` otherwise). At the audited revision the host drops a disabled plugin on reload without calling quiesce, so the file, not the last lifecycle message, is the authority.
- Nothing is written in dry-run, when disabled, faulted, or in an unsupported deployment mode, when the config file or backup dir is unusable, when the on-disk provider block is malformed or structurally unsupported (evidence is retained and evaluation resumes once it is fixed), when `require-explicit-baseline` is set and the baseline is implicit, or when the candidate is not newer than the on-disk baseline or is below the floor.

## Fail-safe posture

If the plugin errors, panics (fused by the host), fails to register, or cannot write, CPA keeps serving requests with the static baseline that is already in `config.yaml` or compiled in. Requests never depend on the plugin. A panic inside a plugin-owned goroutine is recovered, recorded as `last_error`, and marks the plugin `faulted` (writes suspended until the next reconfigure) rather than crashing CPA. See [docs/architecture.md](docs/architecture.md) section 7.

## Build and validation

```bash
export PATH=/path/to/go1.26.0/bin:$PATH
make fmt-check vet lint test race build
make package GLIBC_ENFORCE=0   # on a non-manylinux host
make check-release
bash -n scripts/smoke-test.sh
```

`make build` produces `auto-baseline.so` on Linux. The end-to-end smoke test needs a locally built CPA binary:

```bash
(cd /path/to/CLIProxyAPI && CGO_ENABLED=1 go build -o /tmp/cpa-bin/CLIProxyAPI ./cmd/server)
./scripts/smoke-test.sh /tmp/cpa-bin/CLIProxyAPI ./auto-baseline.so
```

It starts CPA on a free port with a temporary config (placeholder Claude credential pointing at a closed local port, so no traffic leaves the host), sends three Claude Code 2.1.258-shaped requests from two session IDs, and asserts that `config.yaml` was promoted, that CPA logged the reload, and that the status route reports the new baseline. Evidence is kept under `dist/smoke/<run-id>/`.

## Operator acceptance checklist

- [ ] `/CLIProxyAPI/plugins` is backed by a persistent volume and `auto-baseline.so` loads after a restart.
- [ ] Status shows `config_file.exists` and `config_file.writable` as `true` and the expected path.
- [ ] With `dry-run: true`, a few real requests produce a pending candidate with the real client version, package-version and runtime-version.
- [ ] After quorum, history shows a dry-run promotion with the expected tuple.
- [ ] With `dry-run: false`, `config.yaml` gains the promoted `claude-header-defaults` block, CPA logs `config file changed, reloading`, and status no longer shows `awaiting_reload`.
- [ ] After any CPA upgrade, `claude-min-version` / `codex-min-version` are at least the new build's compiled defaults.
- [ ] Requests for version-gated models now succeed.
- [ ] For Codex, `codex.disable-codex-cloaking: true` is set before expecting any effect.

## License

MIT. See [`LICENSE`](LICENSE).
