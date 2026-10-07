# Auto Baseline for CLIProxyAPI

`auto-baseline` is a native CLIProxyAPI (CPA) plugin that keeps CPA's measured Claude Code and Codex CLI client-fingerprint baselines current automatically. It watches the genuine headers of every inbound request, learns the newest authentic client version (together with the package-version and runtime-version that arrived on the same request), and, once a quorum of observations agrees, writes that tuple into CPA's Claude and Codex header defaults in `config.yaml`: `oauth.providers.claude.header-defaults` / `oauth.providers.codex.header-defaults` in CPA v8's layout, or their legacy names `claude-header-defaults` / `codex-header-defaults` (see [Config layouts](#config-layouts-cpa-v8)). CPA hot-reloads the file, so outbound requests stop being rewritten down to a stale compiled-in version.

The plugin never changes outbound headers directly. For the flows this plugin targets (Claude OAuth tokens, credentials with the `claude-code-cli` fingerprint profile, and every cloaked request) CPA normalizes the software tuple against the configured baseline *after* request interceptors run, so an interceptor cannot raise the outbound version; caller-owned API-key flows already forward the client's real headers and need no help (see [docs/architecture.md](docs/architecture.md), section 2.2). Editing the config file is therefore the right lever, and it is the same file CPA's own management console edits.

## How it works

1. **Observe.** The plugin registers the `request_interceptor` capability and receives the inbound client headers before credential selection. It classifies each request from headers only, never bodies, and always answers "no changes".
2. **Validate.** A Claude request counts only if it looks like Claude Code end to end: `User-Agent: claude-cli/<M.m.p> (external, <entrypoint>[, agent-sdk/x.y.z])`, `x-app: cli`, `anthropic-version: 2023-06-01`, `X-Stainless-Lang: js`, `X-Stainless-Runtime: node`, well-formed `X-Stainless-Package-Version` and `X-Stainless-Runtime-Version`, non-empty OS/Arch, and (by default) the `claude-code-20250219` beta. A Codex request counts if the UA is `codex_cli_rs/x.y.z` or `codex-tui/x.y.z` and an `Originator` header is present.
3. **Quorum.** The same fingerprint tuple must be seen `min-observations` times from `min-distinct-sessions` distinct client session IDs inside `observation-window`. Requests without a session header are anonymous: they count toward observations but never as a session. Only tuples strictly newer than the baseline currently on disk are tracked; nothing below `claude-min-version` / `codex-min-version` is ever written, and a provider whose explicit on-disk `user-agent` does not parse is never touched.
4. **Promote.** A background worker re-reads `config.yaml`, re-checks "strictly newer than what is on disk right now", picks the block CPA will actually read (see [Config layouts](#config-layouts-cpa-v8)), edits only the baseline keys with the yaml.v3 node API (comments, order and every other key are preserved; aliases, merge keys, sequences and multi-document files are refused rather than rewritten), copies the previous file to `<backup-dir>/config.yaml.auto-baseline.bak`, re-hashes the file, and rewrites the existing inode in place (write, then truncate, then fsync). Before writing, it parses the rendered file again with CPA's own layout rules and refuses (`promotion_not_effective`) unless CPA would load exactly the promoted tuple. `dry-run: true` runs all of this without writing and logs what would be written, and where. After a real write the promotion is shown as "awaiting reload" until CPA's hot reload is observed; if the file is rewritten without it instead, the provider pauses rather than rewriting the file again.
5. **Persist.** Pending evidence, promotion history and counters live in `<state-dir>/state.json` and survive container restarts.

The promoted Claude tuple is always written in canonical CLI form (`claude-cli/2.1.285 (external, cli)` even when learned from an `sdk-ts` Agent SDK host), with `package-version` and `runtime-version` copied from the same request. `os`, `arch`, `timeout`, `timezone` and `stabilize-device-profile` are never touched. For Codex the full observed UA string is written, because CPA injects it verbatim.

## Requirements and platform support

- A CLIProxyAPI build with native plugin ABI v1 (the plugin declares RPC schema 4). Verified on **CPA v8.0.4** (`d33f63f`) in both config layouts; its compiled defaults and layout rules were audited against that build. The `upstream` paths CPA v8.0.11 made canonical were audited against **v8.0.13** (`d7914af`); the compiled defaults are unchanged through v8.0.16. The ABI was first audited against `v7.2.146-3-g81e1b53`.
- CPA must be able to write its own `config.yaml` (it already does so for the management console and for hashing `management.secret-key`, legacy name `remote-management.secret-key`).
- Persistent access to the plugin directory (the default `state-dir` lives inside it).
- Go 1.26.0 and a native C toolchain only when building from source.

Release targets are: `linux_amd64`, `linux_arm64` (manylinux2014, GLIBC <= 2.17), `darwin_amd64`, `darwin_arm64`, `windows_amd64`. The library is `auto-baseline.so` / `.dylib` / `.dll`.

## Installation

- [Docker Compose and manual installation](docs/install-docker-compose.md)
- [Custom Plugin Store install, update and uninstall](docs/custom-plugin-store.md)
- [Architecture, safety rules, threat model and limitations](docs/architecture.md)
- [Troubleshooting](docs/troubleshooting.md)

Start with `dry-run: true`, watch the status page until the expected candidate reaches quorum, then switch to `dry-run: false`. A complete example is in [`config.example.yaml`](config.example.yaml).

**Codex caveat.** A learned Codex user-agent has no effect until you set `disable-codex-cloaking: true`: `upstream.codex.disable-codex-cloaking` on CPA v8.0.11+, `oauth.providers.codex.disable-codex-cloaking` before that (legacy name `codex.disable-codex-cloaking`); CPA otherwise forces its compiled Codex UA on every outbound request. The status page warns while the flag is off and reports where the flag was read from.

**Compiled defaults and CPA upgrades.** Only before the first promotion, while `config.yaml` has no baseline block, does the plugin have to assume what CPA is currently using; it assumes the compiled default of the audited CPA build, v8.0.4: `claude-cli/2.1.280 (external, cli)` / `0.112.1` / `v26.3.0` and `codex-tui/0.154.0 (...)` (shown as "Assumed CPA build" in status). After the first promotion the block exists, CPA reads it instead of its compiled constant, and the plugin compares against that on-disk value from then on. Upgrading the CPA image therefore never resets or lowers an already-promoted baseline, and no setting needs adjusting. In the rare case that a newer CPA build compiles in a default above your client's version while the block is still absent, the plugin writes your client's real tuple: that is a correction to what you actually run, not a downgrade. `claude-min-version` / `codex-min-version` are floors against spoofed ancient versions and can stay at their defaults; `require-explicit-baseline: true` is an optional stricter mode for operators who prefer to write the first baseline by hand.

## Config layouts (CPA v8)

CPA v8 reads two `config.yaml` layouts, leaf by leaf, and CPA v8.0.11 moved some v8 keys under a new `upstream` section. The plugin reads and writes these keys:

| CPA v8.0.11+ canonical | v8 layout (`config-version: 8`; an alias from v8.0.11) | Legacy name (still read) |
| --- | --- | --- |
| `upstream.claude.header-defaults.{user-agent,package-version,runtime-version}` | `oauth.providers.claude.header-defaults.{...}` | `claude-header-defaults.{...}` |
| not moved | `oauth.providers.codex.header-defaults.user-agent` | `codex-header-defaults.user-agent` |
| `upstream.codex.disable-codex-cloaking` (read only) | `oauth.providers.codex.disable-codex-cloaking` (read only) | `codex.disable-codex-cloaking` |
| not moved | `plugins.configs.auto-baseline` | same: plugin settings stay at the root in every layout |

- **Precedence is per leaf.** A present leaf wins over every lower-precedence name of the same setting, even when it is `false`, `0`, blank, or null (a blank or null value then means CPA's compiled default): `upstream` first, then the v8 name, then the legacy name. A leaf without a higher-precedence counterpart still applies. The plugin resolves each value the same way and reports where each one came from (`upstream`, `v8`, `legacy`, or `default`).
- **Where a promotion is written**, per provider: for Claude, `upstream.claude.header-defaults` whenever the file has an `upstream` section (only CPA v8.0.11+ writes one; the plugin cannot ask CPA for its version, so the file is the evidence). Otherwise the existing v8 block; otherwise the existing legacy block; otherwise the v8 path if the file declares `config-version: 8`; otherwise the legacy path. Codex is never written under `upstream`. The plugin never creates a key beside a higher-precedence counterpart, because CPA deletes such a key on its next load. When it writes a block it also removes the lower-precedence keys it superseded, and any mapping that leaves empty, which CPA would otherwise delete by rewriting the whole file. **After a CPA downgrade** to v8.0.10 or earlier, which ignore `upstream` silently, remove the `upstream` section (moving its Claude block back to `oauth.providers.claude.header-defaults`); otherwise the plugin keeps writing values that CPA does not apply.
- **Loading never migrates**, and neither does `config-version: 8` on its own. On every load CPA only deletes legacy keys that conflict with a present v8 key, and then rewrites the file (4-space indentation, anchors and merge keys expanded). Saves through `/v0/management` keep the file's layout.
- **Any write through `/v8/management` migrates the whole file** to the v8 layout, and CPA's Management Center panel only uses `/v8/management`. Saving anything in the panel, including installing, updating or uninstalling a plugin from the Plugin Store, moves `claude-header-defaults` to `oauth.providers.claude.header-defaults` and so on; on CPA v8.0.11+ it moves the Claude block on to `upstream.claude.header-defaults` and Codex cloaking to `upstream.codex.disable-codex-cloaking`. auto-baseline 0.1.6 follows both moves. 0.1.5 follows only the first: on CPA v8.0.11+ its Claude promotions pause as `promotion_not_effective` (see [troubleshooting](docs/troubleshooting.md#promotion_not_effective-right-after-a-panel-save-on-cpa-v8011)); 0.1.4 follows neither (see [upgrading from 0.1.4](docs/install-docker-compose.md#upgrading-from-014-to-015-cpa-v8)).
- **Scope changes with the layout.** In the v8 layout, values under `oauth.providers.*` apply to OAuth credentials only. The header defaults and `disable-codex-cloaking` no longer reach API-key credentials (`api-keys.claude`, legacy `claude-api-key`), while the legacy root keys apply to both. This is CPA's behavior; the plugin does not work around it.

Status reports, per provider, `effective_baseline.sources` (`upstream`, `v8`, `legacy`, or `default` for each value), `effective_baseline.write_target` (the dotted path a promotion writes), and for Codex `disable_codex_cloaking.{value,source}`. `config_file.layout` is `v8` or `legacy`, and each history entry records its `target`.

**Loop guard.** Before writing, the plugin re-reads the rendered bytes with these rules; unless CPA would load exactly the promoted tuple, and nothing else the plugin reads changes, nothing is written (`promotion_not_effective`) and the provider pauses until `config.yaml` changes. After a write, a later read that shows a different tuple means the file was rewritten without the promotion (for example a panel save restoring an older value). The promotion is then recorded as not effective (`not_effective_at`), the provider shows `paused`, and status warns. The plugin does not retry: **Clear pending** (`POST .../reset`), a change to a write-affecting plugin setting such as `dry-run`, or restoring the promoted tuple by hand resumes it. The pause survives restarts.

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
| `manage-claude` | `true` | Learn and promote the Claude header defaults (`upstream.claude.header-defaults` on CPA v8.0.11+, `oauth.providers.claude.header-defaults`, legacy `claude-header-defaults`). |
| `manage-codex` | `true` | Learn and promote the Codex header-defaults `user-agent` (`oauth.providers.codex.header-defaults`, legacy `codex-header-defaults`). |
| `claude-entrypoints` | `[cli, sdk-cli, claude-vscode, sdk-ts, sdk-py]` | Entrypoints whose fingerprints may be learned. |
| `require-claude-code-beta` | `true` | Require `claude-code-20250219` in `anthropic-beta`. The plugin sees the *inbound* header: a count_tokens or helper request that arrives without the beta is rejected under `claude_code_beta_missing` and does not count (CPA itself adds the beta on the *outbound* count_tokens request, `claude_executor_request.go:209-217,796`, which the plugin never sees). Requests that carry it count normally. |
| `min-observations` | `3` | Observations of one identical tuple needed inside the window. |
| `min-distinct-sessions` | `1` | Distinct named session IDs those observations must span. Anonymous requests (no session header) never count as a session, so `1` means "observation count only". Set `2` when clients send `X-Claude-Code-Session-Id` (Claude Code does) or a Codex `Session_id`/`Thread-Id`. |
| `observation-window` | `24h` | Age limit for an observation to count. |
| `promotion-cooldown` | `60s` | Minimum spacing between config writes. |
| `claude-min-version` | `2.1.280` | Never write a Claude baseline below this (compiled default of the audited CPA, v8.0.4; was `2.1.220` for v7.2.146 up to plugin 0.1.4). |
| `codex-min-version` | `0.154.0` | Never write a Codex baseline below this (was `0.146.0` up to plugin 0.1.4). |
| `require-explicit-baseline` | `false` | Never promote a provider whose on-disk baseline is implicit (compiled default assumed); refusals are counted under `baseline_implicit`. The safe choice after a CPA upgrade whose compiled defaults the plugin does not know. |
| `display-timezone` | `UTC` | IANA zone or `local` for timestamps on the HTML status view only. |

Durations accept Go strings (`24h`, `60s`) or bare integers (seconds). Unknown or misspelled keys (for example `dry_run`) are rejected at registration so a typo cannot silently leave a default in place.

## Management routes

Authenticated by CPA's normal management-key boundary:

```text
GET  /v0/management/plugins/auto-baseline/status
GET  /v0/management/plugins/auto-baseline/status/html
POST /v0/management/plugins/auto-baseline/observe
POST /v0/management/plugins/auto-baseline/reset
POST /v0/management/plugins/auto-baseline/dry-run
```

Browser resource (sidebar entry):

```text
GET /v0/resource/plugins/auto-baseline/status
```

| Route | Behavior |
| --- | --- |
| `GET .../status` | JSON snapshot: effective baselines with per-value `sources` and `write_target`, config layout, pending candidates with observation and session counts, last promotion (with `target`, `confirmed_at` or `not_effective_at`), `paused` providers, history, counters, config/backup/state file status, dry-run state (`dry_run`, `dry_run_awaiting_reload`), warnings. |
| `GET .../status/html` | Authenticated browser view of the same snapshot with actions (below). |
| `POST .../observe` | Host reporting: `{provider, user_agent, package_version, runtime_version, os, arch, session_id, force}` goes through the same validation and quorum as an inbound request; `force: true` queues an immediate promotion after validation (never below the on-disk baseline or the floor; one queued entry per provider). `202` when accepted (`queued: true` for a force), `422` with a reason bucket when rejected. |
| `POST .../reset` | Clears pending candidates and resumes paused providers; baselines and history are kept. |
| `POST .../dry-run` | Body `{"enabled": true|false}`. Edits only `plugins.configs.auto-baseline.dry-run` in CPA's `config.yaml` with the same node surgery, backup, re-hash, in-place write and verification as a promotion. CPA hot-reloads the file and the runtime flag flips when `plugin.reconfigure` arrives; until then status reports `dry_run_awaiting_reload`. `200` on success, `409` while a promotion write is in flight or when the plugin is disabled on disk, `422` when the subtree is missing or the file shape is unsupported (the plugin never creates `plugins.configs.auto-baseline`), `503` in an unsupported deployment mode or when the config/backup location is unusable. |

### Sidebar and browser views

CPA's Management Center lists the resource route as **Auto Baseline** in its sidebar and iframes it from the CPA origin. Resource routes are unauthenticated in current CPA, so the server response is always the **redacted** view: it hides the config, state and backup paths, error and warning text, and the deployment-mode reason; it shows the effective baselines, pending candidate tuples, quorum rules, promotion history, counters, and the assumed CPA build (none of which is secret). Session identifiers are never rendered on any view, only their counts.

The redacted page carries a small inline script that upgrades the view purely client-side when the browser already holds a same-origin management session. It recovers the management key that the official management console (Cli-Proxy-API-Management-Center) persists in same-origin localStorage (the console's documented `enc::v1::` reversible obfuscation under `cli-proxy-auth`, or the legacy `managementKey` entry), fetches the authenticated `GET .../status/html` view over the same origin with `Authorization: Bearer`, and swaps it in. This is the same trust model used by the [reset-priority](https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/reset-priority) and [account-health-pushover](https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/account-health-pushover) plugins: the upgrade happens entirely in the operator's browser with credentials that browser already holds (a remembered console session, ambient reverse-proxy auth, or cookies), and the key is only ever sent to same-origin CPA management routes. When the console runs on a different origin, or no key is remembered (`Remember password` off) and no ambient auth exists, the fetch fails closed and the redacted view stays up with a short note.

The authenticated HTML view at `GET .../status/html` requires the same management authentication as the JSON route. It shows exact paths, sanitized errors and warnings, per-provider baseline cards (effective version, explicit or implicit, where each value comes from, the block a promotion writes, floor, malformed/unsupported/paused flags, awaiting-reload state), the pending-candidates table with observation and session progress, the promotion history with reload confirmation, counters, and four actions:

- **Promote now** on each pending row queues an immediate promotion of that exact tuple (`POST .../observe` with `force: true`, body built from the row).
- **Switch to live writes** / **Switch to dry-run** flips `dry-run` in `config.yaml` through `POST .../dry-run`; the label reflects the current runtime value and a pill shows while the change awaits CPA's reload.
- **Clear pending** discards collected evidence and resumes a paused provider (`POST .../reset`).
- **Refresh** reloads the page.

Timestamps are rendered in the configured `display-timezone` as `Tue Sep 1 2026 - 6:25:36 PM PDT`; hover any timestamp for the exact RFC3339 UTC instant. The JSON route always stays RFC3339 UTC.

The hands-off workflow this enables: install with `dry-run: true`, open the sidebar, use Claude Code or Codex through the proxy until the pending row shows quorum met (or click Promote now), click **Switch to live writes** once, and never touch the YAML again. Every later version your client ships is learned and promoted automatically; the sidebar shows what was written and whether CPA reloaded it.

At the audited CPA revision, management authentication is header-only (`Authorization: Bearer ...` or `X-Management-Key`); a query parameter is not a management credential, and ordinary address-bar navigation cannot add that header. Open the HTML route only through the sidebar upgrade, a browser/profile, or an authenticated reverse proxy that supplies the management header to both the page GET and its same-origin action POSTs. CPA refuses every plugin resource route in Home mode (`internal/api/server_management.go:281`, HTTP 404); the authenticated routes still work.

### CSRF

The three mutating routes are CSRF-gated. Browser requests carrying `Sec-Fetch-Site` are accepted only when the value is `same-origin` or `none` **and** the request includes `X-Auto-Baseline-Action: 1`; `same-site`, `cross-site`, and empty metadata are rejected with HTTP 403. Browsers omit fetch metadata entirely for requests to non-secure URLs (plain `http://` on a non-loopback host, which is how many private CPA deployments are reached). In that case the gate accepts a single well-formed `http://` `Origin` plus the action header, because mixed-content blocking means that is the only origin a legitimate browser page can produce against a plain-HTTP server; `https://`, `null`, multi-valued, and malformed origins without metadata are rejected. Over plain HTTP the plugin therefore cannot prove same-origin, only "came from some plain-HTTP page"; serving CPA over HTTPS (for example with `tailscale serve`) restores the strict check automatically. Non-browser clients such as `curl` send neither header and authenticate with the management key plus the action header alone. A fronting proxy that injects ambient management authentication must still enforce its own CSRF/origin policy for the entire Management API and must pass `Sec-Fetch-Site` unchanged.

`scripts/smoke-test.sh ... --browser` exercises exactly this shape against a real CPA bound to a non-loopback address over plain HTTP (see [Build and validation](#build-and-validation)).

Example API actions:

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'

curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/status | jq .

# Switch to live writes (CPA hot-reloads config.yaml)
curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" -H "X-Auto-Baseline-Action: 1" \
  -H "Content-Type: application/json" --data '{"enabled":false}' \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/dry-run

# Report a fingerprint captured on the host (e.g. from a header dump)
curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" -H "X-Auto-Baseline-Action: 1" \
  -H "Content-Type: application/json" \
  --data '{"provider":"claude","user_agent":"claude-cli/2.1.285 (external, cli)","package_version":"0.112.1","runtime_version":"v26.3.0","os":"Linux","arch":"x64","session_id":"host-report"}' \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/observe
```

No route or log ever includes API keys, tokens, request bodies, session IDs, or the contents of `config.yaml` beyond the managed baseline values. Do not paste the management key into shell history on shared systems; use an environment variable or secure prompt.

## Write safety

- Only the Claude header-defaults `user-agent`, `package-version`, and `runtime-version`, the Codex header-defaults `user-agent` (each in the block described in [Config layouts](#config-layouts-cpa-v8)), and (through the operator-driven dry-run route only) `plugins.configs.auto-baseline.dry-run` are ever written. Writing a v8 block also removes the superseded legacy keys, and an emptied legacy block, that CPA would delete on its next load. A file with a duplicate mapping key is refused (`duplicate_key`): CPA's own decoder rejects such a file, so it could never be hot-reloaded. A target that is a non-empty scalar, a sequence, an alias, reached through a YAML merge key (`<<`, recognized by its `!!merge` tag; a quoted `"<<"` is an ordinary key), part of an alias/merge cycle, or part of a multi-document file is refused (`unsupported_config_shape` / `multi_document_config`) rather than rewritten. Reads resolve aliases and merge keys with a cycle guard, so a malformed file cannot crash CPA.
- Every write is preceded by a fresh read and a "strictly newer than on disk" check, and by the loop guard (the rendered file must make CPA load exactly the candidate); the file hash is compared again after the backup and immediately before the write, and the read-modify-write is retried (up to 3 times) if another writer got there first. This narrows but cannot eliminate the race: other writers do not take a lock.
- Writes rewrite the existing inode in place (write the new bytes, truncate to the new length, fsync, close), so a Docker single-file bind mount works and the file is never empty on disk between steps. If a write fails midway the previous bytes are written back best-effort. Crash atomicity still cannot be guaranteed on a single-file bind mount (no rename is possible); the backup exists for manual recovery.
- The previous file is copied to `<backup-dir>/config.yaml.auto-baseline.bak` (mode 0600) first; an unwritable backup dir blocks promotion.
- `Stop`/quiesce is a write barrier: in-flight workers and timers are waited for, and no write can start or finish afterwards. A reconfigure that changes any write-affecting field drains the same way before the new config takes effect, and every attempt re-checks its config generation immediately before writing.
- The fresh on-disk read before every write must also show `plugins.enabled: true` and `plugins.configs.auto-baseline.enabled: true` (`plugin_disabled_on_disk` otherwise). At the audited revision the host drops a disabled plugin on reload without calling quiesce, so the file, not the last lifecycle message, is the authority.
- Nothing is written in dry-run, when disabled, faulted, or in an unsupported deployment mode, when the config file or backup dir is unusable, when the on-disk provider block is malformed or structurally unsupported (evidence is retained and evaluation resumes once it is fixed), when `require-explicit-baseline` is set and the baseline is implicit, when the candidate is not newer than the on-disk baseline or is below the floor, or while the provider is paused after a promotion that did not take effect.

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

`make build` produces `auto-baseline.so` on Linux. The end-to-end smoke test runs it in the pinned CPA v8.0.4 image (`eceasy/cli-proxy-api:v8.0.4@sha256:72205ea2...`; override with `CPA_SMOKE_IMAGE`), once per config layout:

```bash
make build
./scripts/smoke-test.sh                                       # legacy and v8 layouts
CPA_SMOKE_LAYOUT="legacy v8 interim" ./scripts/smoke-test.sh  # plus the interim layout
```

Each layout gets a throwaway container published on `127.0.0.1` only, with `config.yaml` as a writable single-file bind mount (as in Compose/Coolify) and a placeholder Claude credential pointing at a closed port, so no traffic leaves the host. It sends three Claude Code 2.1.285-shaped requests from two session IDs and asserts that the promotion landed in the block that layout requires, that CPA logged the reload and the plugin confirmed it, and that CPA's own runtime config (`GET /v0/management/config`) carries the promoted tuple. It then sends the same fingerprint again and asserts the promotion happened exactly once, with `config.yaml` unchanged. `legacy` starts with no baseline (compiled default 2.1.280); `v8` starts with `oauth.providers.claude.header-defaults` at 2.1.283, the layout of the 2026-09-29 loop; `interim` is a v8 file with the header keys kept at their legacy names. Evidence is kept under `dist/smoke/<run-id>/<layout>/`. Management requests carry the placeholder key and are never retried after a 401 or 403.

`--cpa-bin <CLIProxyAPI>` (or the older `./scripts/smoke-test.sh <binary> [auto-baseline.so]` form) runs a local CPA binary instead. Add `--browser` to it to also prove the browser trust model over plain HTTP on a non-loopback address: CPA is bound to `0.0.0.0` with `remote-management.allow-remote: true` (v8: `management.allow-remote`) for the duration of the run (it is reachable on this host's interfaces while it runs), the redacted sidebar page and its same-origin upgrade are fetched at `http://<host-ip>:<port>`, the dry-run switch is flipped both ways through the CSRF gate with an `Origin` header and no `Sec-Fetch-Site` (the header shape a browser sends to a plain-HTTP origin) and confirmed through CPA's reload, and hostile shapes (`https://` origin, `cross-site`, missing action header) are asserted to return 403. No headless browser is driven: none was available when the phase was written, so the checks are `curl` reproductions of the browser's requests and the output says so.

## Operator acceptance checklist

- [ ] `/CLIProxyAPI/plugins` is backed by a persistent volume and `auto-baseline.so` loads after a restart.
- [ ] Status shows `config_file.exists` and `config_file.writable` as `true` and the expected path.
- [ ] With `dry-run: true`, a few real requests produce a pending candidate with the real client version, package-version and runtime-version.
- [ ] After quorum, history shows a dry-run promotion with the expected tuple.
- [ ] Status `effective_baseline.sources` and `write_target` match the layout of `config.yaml` after any panel save: `upstream` and `upstream.claude.header-defaults` on CPA v8.0.11+, `v8` and `oauth.providers.claude.header-defaults` before that.
- [ ] With `dry-run: false`, the promoted values appear in that block, CPA logs `config file changed, reloading`, and status shows the promotion confirmed (no `awaiting_reload`, no `paused`).
- [ ] After any CPA upgrade, `claude-min-version` / `codex-min-version` are at least the new build's compiled defaults.
- [ ] Requests for version-gated models now succeed.
- [ ] For Codex, `disable-codex-cloaking: true` is set before expecting any effect (`upstream.codex.` on CPA v8.0.11+, `oauth.providers.codex.` before that, legacy `codex.`).
- [ ] If Claude API-key credentials relied on the header defaults, they still receive them (v8-layout values apply to OAuth credentials only).

## License

MIT. See [`LICENSE`](LICENSE).
