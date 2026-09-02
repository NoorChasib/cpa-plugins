# Architecture: auto-baseline

This document records the feasibility findings, the design decision, the safety
rules and how each one is enforced, the failure analysis, and the known
limitations of the `auto-baseline` plugin. All CLIProxyAPI (CPA) references are
to commit `81e1b5374f99c212f196f34956eeed964a46b8fa` (`v7.2.146-3-g81e1b53`).
Revalidate every file:line after a CPA upgrade.

## 1. The problem in CPA terms

CPA normalizes the outbound Claude fingerprint to a "measured baseline":

- Compiled defaults: `claude-cli/2.1.220 (external, cli)`, package `0.94.0`,
  runtime `v26.3.0`, OS `MacOS`, arch `arm64`
  (`internal/runtime/executor/helps/claude_device_profile.go:22-27`).
- `defaultClaudeDeviceProfile(cfg)` overrides each field with the corresponding
  `claude-header-defaults.*` YAML key when it is non-blank
  (`claude_device_profile.go:129-151`; struct at
  `internal/config/config_types.go:115-124`).
- `plausibleClaudeCodeUserAgent` requires the client's Claude Code version to
  equal the baseline version exactly (`claude_client_detection.go:462-470`,
  `plausibleClaudeCLIVersion` is `Compare(...) == 0`).
  `meetsClaudeDeviceProfileBaseline` additionally requires package-version and
  runtime-version equality (`claude_device_profile.go:219-229`), and
  `normalizeClaudeDeviceProfile` replaces any non-matching software tuple with
  the baseline (`:239-250`).
- Only entrypoints in `nativeClaudeEntrypoints` (`cli`, `sdk-cli`,
  `claude-vscode`; `claude_client_detection.go:66-70`) can be "confirmed";
  `sdk-ts` and `sdk-py` are cloaked to the baseline, going out with the baseline
  UA verbatim and `cc_entrypoint=cli`
  (`claude_client_detection.go:123-127`,
  `claude_executor_request.go:1011-1019`,
  `claude_device_profile.go:566-597`,
  `claude_executor_cloaking.go:1027-1029`).
- `DefaultClaudeVersion(cfg)` feeds the `cc_version` billing header
  (`claude_device_profile.go:588-594`, used at
  `claude_executor_cloaking.go:196,1027`), so raising the baseline UA also fixes
  the billing header version.

Anthropic gates new models server-side on the Claude Code version. When the
proxy stamps 2.1.220 on a request for a gated model the API refuses it even
though the real client is 2.1.258.

## 2. Feasibility: what the plugin API can and cannot hook

### 2.1 Interfaces available (`sdk/pluginapi/types.go`, `sdk/pluginabi/types.go`)

ABI v1, RPC schema 4. The plugin declares capabilities in
`plugin.register`; the relevant ones here are:

- `request_interceptor` (`internal/pluginhost/rpc_schema.go:35`, wired at
  `rpc_client.go:115-116`): the host calls `request.intercept_before` for every
  model request before credential selection and supplies `request.intercept_after`
  as its after-auth callback (`sdk/api/handlers/handlers_execution.go:84,89,154,186,233`
  and `handlers_stream.go:341,346`, including count_tokens at `:233`; callback
  implementation at `handlers_interceptors.go:475-485,529-545`). Note the
  provider is resolved first (`handlers_execution.go:54`); a request for an
  unroutable model is rejected before any interceptor runs.
- `management_api`: authenticated routes anywhere under `/v0/management/`
  (the host normalizes and registers them at
  `internal/pluginhost/management.go:37-150`; the `/plugins/<id>/...` prefix
  this plugin uses is its own convention, not host-enforced) and
  unauthenticated GET resources under `/v0/resource/plugins/<id>/`
  (`management.go:186`, served at `:301-306`).

Host callbacks a plugin may invoke (`sdk/pluginabi/types.go:78-92`):
`host.http.do(+stream)`, `host.model.execute(+stream)`, `host.stream.emit/close`,
`host.log`, `host.auth.list/get/get_runtime/save`. **There is no callback to
read or write CPA's configuration.** The plugin only receives its own
`plugins.configs.<id>` subtree as `config_yaml` in `plugin.register` /
`plugin.reconfigure` (`internal/config/config_types.go:40-47`, `Raw` node;
the same subtree also carries the host-owned `priority` and `store` keys,
`internal/pluginhost/config.go:109`).

### 2.2 Why header mutation cannot raise the baseline (for the flows that matter)

`request.intercept_before` may rewrite request headers
(`RequestInterceptResponse.Headers`, merged at
`internal/pluginhost/adapters_interceptors.go:421-436`), and the headers it
sees are the inbound client's real headers (`handlers_execution.go:82` ->
`modelExecutionHeaders`, `model_execution.go:200-205` -> `headersFromContext`,
`handlers_context.go:137-145`, a gin request header clone).

What happens to those headers afterwards depends on the credential's wire
policy (`internal/runtime/executor/claude_fingerprint_policy.go:34-41,96-112`):

- **Claude OAuth tokens, credentials with `fingerprint-profile:
  claude-code-cli`, and any cloaked request** (`applyCLIFingerprint` at
  `claude_executor_request.go:754`) get the strict CLI fingerprint. The
  executor normalizes the software tuple against
  `defaultClaudeDeviceProfile(cfg)` *after* the interceptor: any UA whose
  version is not exactly the baseline version is rewritten down
  (`normalizeClaudeDeviceProfile`). A plugin can only make such a client look
  *like* the baseline, never *newer* than it.
- **Caller-owned API-key flows** (first-party `api.anthropic.com` API keys
  without a fingerprint profile, and unconfirmed clients on other gateways;
  `preserveCallerFingerprint` at `:755`, applied at `:871-916`) keep the
  caller's own `User-Agent` and Stainless headers. There an interceptor
  *could* change the outbound version, but there is nothing to fix: the
  client's real headers already go out.

The version gate this plugin exists to solve occurs on the OAuth / CLI-profile
path, where the baseline in `cfg` is authoritative and `cfg` only changes
through `claude-header-defaults` in `config.yaml`. Config mutation is therefore
the right lever; header mutation would be a no-op for the affected flows.

Codex is the same story with a twist (section 6).

### 2.3 Why config-file write + hot reload is the viable path

CPA watches its config path with fsnotify for `Write|Create|Rename`
(`internal/watcher/events.go:67-92`), debounces, reads the file, skips the
reload when the sha256 is unchanged, and otherwise runs `config.LoadConfig`
(`config_reload.go:52-95`). A successful reload re-registers executors with the
new `cfg` (`sdk/cliproxy/service_config.go:169-173`,
`forceReplaceAuths: true`) and reconfigures plugins
(`service_plugins.go:101-103` -> `pluginHost.ApplyConfig` ->
`plugin.reconfigure`). CPA's own management console edits the same file the
same way (`internal/api/handlers/management/config_basic.go:101-118`
`WriteConfig`; `PutConfigYAML` at `:120-170`), so a plugin writing
`config.yaml` uses a path the product already exercises.

Conclusion: **the plugin observes fingerprints through the request interceptor
and promotes them by editing `config.yaml`; CPA's hot reload applies the new
baseline.** No CPA change is required.

## 3. Version-source options

| Option | How | Pros | Cons | Decision |
| --- | --- | --- | --- | --- |
| (a) Observed-client learning | Classify inbound headers; promote after quorum | Sees exactly what the client sends, including the package-version and runtime-version that cannot be derived from the version number alone; needs no host access; follows whatever CLI is actually run; works inside a container | Trust-on-first-use; a poisoned request could raise the baseline, so a quorum is needed; only learns from clients that actually hit the proxy | **Primary mechanism** |
| (b) Host reporting | A host agent/cron POSTs the fingerprint to a management route | Deterministic; works before the first real request | Requires host-side plumbing; `claude --version` alone does not yield package-version/runtime-version (they must be captured from a real request, e.g. a header dump), so the report must carry all three anyway | **Implemented as an optional route** `POST /v0/management/plugins/auto-baseline/observe`. Reports go through the same classifier and quorum as inbound observations; `force: true` queues an immediate promotion after validation (still never below the on-disk baseline or the floor) |
| (c) Registry polling | Poll npm `@anthropic-ai/claude-code` / GitHub releases | No client needed | Tracks what is *published*, not what the client *sends*: the baseline must equal the client's exact version or exact-match passthrough breaks and cloaked requests announce a version the operator does not run; package-version and runtime-version are not derivable from the release number; adds outbound network dependency and a new failure mode; a mismatch between the promoted version and the real client would make every native `cli` request fall back to the cloaked baseline | **Not implemented** |

## 4. Learning pipeline

1. **Classification** (`internal/fingerprint`): headers only, no body parsing.
   The interceptor runs on every model request; JSON-decoding bodies on that
   path costs CPU per request for a signal the headers already carry, and the
   local-trust threat model (section 8) does not justify it.
   - Claude: UA matches
     `^claude-cli/(\d+)\.(\d+)\.(\d+)\s+\(external,\s*<entrypoint>(,\s*agent-sdk/\d+\.\d+\.\d+)?\)$`
     (case-insensitive, same shape as CPA's `claudeCodeNativeUserAgentPattern`,
     `claude_client_detection.go:32`), entrypoint in the configured allowlist,
     `x-app: cli`, `anthropic-version: 2023-06-01`, `X-Stainless-Lang: js`,
     `X-Stainless-Runtime: node`, `X-Stainless-Package-Version` `^\d+\.\d+\.\d+$`,
     `X-Stainless-Runtime-Version` `^v\d+\.\d+\.\d+$`, non-empty OS/Arch, and
     (by default) `claude-code-20250219` in `anthropic-beta`. Session identity
     is `X-Claude-Code-Session-Id`.
   - Codex: UA `^(codex_cli_rs|codex-tui)/(\d+)\.(\d+)\.(\d+)\b` plus a
     non-empty `Originator` header. Session identity is
     `Session_id`/`Session-Id`/`Thread-Id` (`X-Client-Request-Id` is per
     request and deliberately not used).
   - count_tokens: the plugin only sees the *inbound* headers. CPA adds
     `claude-code-20250219` to the *outbound* count_tokens request
     (`claude_executor_request.go:209-217,796`) regardless of what arrived,
     so an inbound count_tokens request without the beta is rejected under
     `claude_code_beta_missing`, and one that carries it counts normally.
   - Everything else is counted as ignored.
2. **Candidate** = one internally consistent tuple from ONE request. Claude:
   `{version, package-version, runtime-version, os, arch, entrypoint, agent-sdk}`;
   the written UA is canonicalized to `claude-cli/<M.m.p> (external, cli)`.
   Codex: `{version, full UA}`. The candidate **key** is exactly the tuple that
   would be written (Claude: version+package+runtime; Codex: full UA), so
   observations with different package versions never merge.
3. **Quorum** (`internal/learner`, pure and clock-injected): `min-observations`
   (default 3) of the same key inside `observation-window` (default 24h), from
   `min-distinct-sessions` distinct *named* session IDs (default 1, meaning
   observation count alone). Anonymous requests count toward observations but
   never as a session, so a client that omits the session header can reach
   quorum while a single spoofed session cannot when the operator sets 2.
   Only candidates strictly newer than the effective baseline and not below
   the configured floor are tracked; a provider whose on-disk block is
   malformed or structurally unsupported keeps accumulating evidence but is
   never ready until a later read clears the block. Memory is bounded (32 keys
   per provider, 256 records per key). Restored state is run through the full
   structural validator under the *current* rules (canonical UA, package and
   runtime patterns, OS/arch/entrypoint present, entrypoint in the current
   allowlist, bounded agent-sdk, session IDs passing the live predicate,
   records inside the observation window and not in the future) and merged
   (not replaced) with live evidence.
4. **Promotion** (`internal/engine`, background worker): after
   `promotion-cooldown`, re-read `config.yaml`, re-check "strictly newer than
   what is on disk now", edit only the baseline keys with the yaml.v3 Node API,
   back up, re-hash, write in place, and record the promotion as
   `awaiting_reload` until the next `plugin.reconfigure` (CPA reconfigures
   plugins on every reload) or a fresh read shows the value. `dry-run`
   computes and logs but never writes. The worker is single-flight with a
   rescan flag and a bounded forced-promotion queue, so work that becomes
   ready while it runs is never lost; `Stop` waits for it (write barrier).
5. **Persistence** (`internal/statefile`): `state-dir/state.json`, atomic
   temp+rename (the state dir is a normal directory). Saved on promotion, every
   5 minutes when dirty, and on quiesce/shutdown. Corrupt or missing state
   starts fresh with a warning.
6. **Effective baseline discovery** (`internal/configfile`): at
   register/reconfigure and before every promotion the plugin parses
   `claude-header-defaults` / `codex-header-defaults` / `codex.disable-codex-cloaking`.
   Blank values are treated as absent, exactly like CPA's `hdrDefault`
   (`claude_device_profile.go:133-139`). An absent field means the compiled
   default of the audited build (constants in
   `internal/fingerprint/fingerprint.go`); this is an explicit assumption
   surfaced in status as `assumed_cpa_version`.

## 5. Safety rules and enforcement

| Rule | Enforcement |
| --- | --- |
| Never downgrade, never re-promote an equal version | `learner.eligibleLocked` rejects candidates not strictly newer than the recorded baseline; `engine.promoteProvider` re-reads the file and runs the same check against the fresh on-disk value inside `configfile.Apply`'s `check` hook; a failed check adopts the on-disk baseline and drops the candidate without error. |
| Never overwrite an explicit value the plugin cannot read | An explicit `user-agent` that does not parse marks the provider `Malformed`; the learner keeps collecting evidence but reports `baseline_malformed` instead of readiness, the pre-write check refuses, and status warns. The compiled default is never used as a comparable baseline when an explicit value exists. When a later read shows a valid baseline, evaluation resumes on the retained evidence. `require-explicit-baseline: true` additionally refuses any implicit baseline (`baseline_implicit`). |
| Never write below a floor | `claude-min-version` / `codex-min-version` (defaults = compiled defaults) are enforced in the learner and again in the promotion check, protecting against a config file that carries an ancient explicit baseline. |
| Internally consistent tuples only | A candidate is built from one request; the key includes package-version and runtime-version; the learner never merges keys. Claude UA is canonicalized so `sdk-ts, agent-sdk/...` never reaches the baseline. |
| Only valid fingerprints | Regex-validated versions, bounded UA length (512), no control characters, safe OS/Arch charset; values are written as double-quoted YAML scalars so `0.94.0`-like values can never be re-read as floats. |
| Anti-poisoning | Quorum across distinct sessions within a window; `force` is only available through the authenticated management API with a CSRF header. |
| Touch nothing else in `config.yaml` | yaml.v3 Node edits of exactly `claude-header-defaults.{user-agent,package-version,runtime-version}` or `codex-header-defaults.user-agent`; missing mapping nodes are created; a null placeholder is converted in place; comments, order, `os`/`arch`/`timeout`/`timezone`/`stabilize-device-profile`, and `beta-features` are untouched (tested in `configfile_test.go`). |
| Never destroy operator content, never crash the host | A duplicate mapping key anywhere the plugin traverses is refused (`duplicate_key`): CPA's yaml.v3 struct decode rejects such a file ("mapping key ... already defined"), so it could never reload. Reads resolve aliases and `<<` merge keys (identified by the `!!merge` tag, never by scalar text) so a shared-defaults config yields the real effective values; resolution carries a visited set and a depth bound of 32, so an alias or merge cycle yields `unsupported_config_shape` instead of the stack overflow that would kill CPA. Writes refuse (`unsupported_config_shape`) when the target is a non-empty scalar, a sequence, an alias, or reachable only through a merge key, and refuse multi-document streams (`multi_document_config`). A problem confined to one provider's block only blocks that provider. |
| Concurrent editors | sha256 at read, after the backup, and immediately before the write; `ErrChanged` triggers up to 3 retries of the full read-modify-write; the write is skipped when the rendered bytes are identical. Other writers do not take a lock, so this narrows the race without eliminating it. |
| Write in place, never empty | `writeInPlace` opens the existing inode `O_RDWR`, writes the new bytes from offset 0, truncates to the new length, fsyncs, closes. Unlike CPA's own `WriteConfig` (`config_basic.go:101-118`, which truncates first) the file is never empty between steps, so a crash or ENOSPC cannot hand CPA's watcher an empty file. On any failure after open the previous bytes are written back best-effort. No rename, so a Docker single-file bind mount works; crash atomicity still cannot be guaranteed on such a mount. |
| Backup | Previous bytes are copied to `<backup-dir>/config.yaml.auto-baseline.bak` (mode 0600; `backup-dir` defaults to `state-dir`) before each write; an unwritable backup dir blocks promotion. |
| Stop is a write barrier | `Engine.Stop` (JSON quiesce, `enabled: false` via reconfigure, native shutdown) cancels timers, waits for every worker and timer callback in the WaitGroup (timers are counted into it before they are armed, so no Add can race a Wait), and `promoteProvider` re-checks the barrier and its config generation under the lock immediately before every `apply`. A reconfigure that changes a write-affecting field (`config-path`, `backup-dir`, `state-dir`, `dry-run`, floors, `manage-*`, `enabled`, `require-explicit-baseline`) drains the same way before swapping the config; a `state-dir` change flushes to the old dir, then loads, validates, and merges the new one. Lifecycle RPCs are serialized by a runtime-level mutex. |
| The host may disable the plugin without quiesce | At the audited revision `pluginhost.ApplyConfig` simply skips a plugin whose instance is disabled (`internal/pluginhost/host.go:254`) or drops every plugin when `plugins.enabled` is false (`:220-228`); no `plugin.quiesce` is sent. Two safeguards cover this: an invalid reconfigure quiesces the old engine before returning `invalid_config` (the host drops the capabilities without quiesce), and the fresh read before every write must show both `plugins.enabled` and `plugins.configs.auto-baseline.enabled` true (`plugin_disabled_on_disk` otherwise). |
| Plugin bugs cannot take CPA down | Every plugin-owned goroutine and timer callback defers a recover that marks the engine `faulted` (writes suspended until reconfigure) under a separate fault mutex, never `mu`, so a panic while `mu` is held cannot deadlock; every `mu` critical section on the worker path uses `defer Unlock`. WaitGroup and in-flight flags are still released. `Dispatch` recovers panics into error envelopes. |
| Forced promotions are never lost | The management `force` queue keeps the highest version per provider (ties keep the first); an entry stays queued until its attempt reaches a terminal outcome, and an attempt aborted by the write barrier leaves it queued so an enabling reconfigure replays it. |
| Reload confirmation is value-correlated | A written promotion is marked `awaiting_reload` in the same critical section that records it. It is confirmed only when a later read of `config.yaml` (on `plugin.reconfigure`, restart, or any refresh) shows exactly the promoted tuple; a two-minute grace period ends in a status warning. |
| Hot path is cheap and cannot break requests | `Observe` does bounded header parsing and bounded map bookkeeping under one mutex; no I/O, no host callbacks, no blocking on the worker; the interceptor decodes a Headers-only projection and always returns the precomputed empty response `{}` ("no changes", `adapters_interceptors.go:128-137`); decode failures are answered as no-ops. |

## 6. Claude versus Codex

| | Claude | Codex |
| --- | --- | --- |
| Keys written | `claude-header-defaults.user-agent`, `.package-version`, `.runtime-version` | `codex-header-defaults.user-agent` only (never `beta-features`) |
| UA written | Canonical `claude-cli/<v> (external, cli)` (CPA compares the version only, `claudeCLIVersionPattern` prefix match `claude_device_profile.go:33`; the `cli` entrypoint is what cloaked requests announce) | The full observed UA verbatim; it carries OS/terminal details (`codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/...`) and CPA injects it as-is |
| Authenticity signals | x-app, anthropic-version, stainless lang/runtime/package/runtime-version/os/arch, claude-code beta | `Originator` header (`codex_cli_rs`, `codex-tui`, `Codex Desktop`, ...) |
| Effect once written | Immediate after hot reload | **Only when `codex.disable-codex-cloaking: true`.** `ensureHeaderWithConfigPrecedence` gives `codex-header-defaults.user-agent` precedence over the client UA (`codex_websockets_request.go:309-329`, used at `codex_executor_request.go:342`), but `applyCodexCloakingHeaders` (`codex_executor_request.go:372-378`) then forces the compiled `codexUserAgent` (`:26`) and `Originator` unless `cfg.Codex.DisableCodexCloaking` is set (`config_types.go:147-150`). The plugin reads that flag and shows a status warning when it is not set. |

## 7. Fail-safe analysis

- **Plugin crashes, is fused, or errors**: an interceptor RPC error is logged
  and ignored (`adapters_interceptors.go:31-34`); a panic fuses the plugin
  (`:25-30`). In both cases CPA keeps serving with whatever
  `claude-header-defaults` is on disk (or the compiled default). Requests never
  depend on the plugin.
- **Invalid plugin config**: `plugin.register` returns an error envelope and
  the host refuses the plugin; CPA runs with the static baseline.
- **Missing or read-only `config.yaml`, unwritable backup dir**: status shows
  the error; learning continues; nothing is promoted.
- **Unsupported deployment mode** (home, postgres, object store, git store;
  detected from `-home-jwt`/`HOME_JWT`, `PGSTORE_DSN`, `OBJECTSTORE_ENDPOINT`,
  `GITSTORE_GIT_URL`, `cmd/server/main.go:200-330`): the local file is not the
  effective configuration; automatic writes are disabled unless `config-path`
  is explicit; status and one log line say so.
- **Plugin goroutine panic**: recovered, `faulted`, writes suspended until
  reconfigure; CPA is unaffected.
- **Write not reloaded**: the promotion stays `awaiting_reload`; after two
  minutes status warns to check the watcher / config path / home mode.
- **Bad write / concurrent edit**: sha256 check plus bounded retries; on
  persistent failure the candidate stays pending and `last_error` is set. If a
  written file failed to parse, CPA's reload logs the error and keeps the old
  in-memory config (`config_reload.go:92-95`); the operator has the `.bak`.
- **Wrong promotion**: the operator restores `config.yaml.auto-baseline.bak`
  or edits the file; CPA hot-reloads; the plugin adopts the on-disk value at
  the next read and never re-promotes an equal or older version.
- **Home mode**: when the config comes from a Home control plane the file the
  plugin edits is not the effective configuration; see troubleshooting. CPA
  additionally refuses every plugin *resource* route in Home mode
  (`internal/api/server_management.go:281`, HTTP 404), so the sidebar page
  disappears while the authenticated management routes keep working.

## 8. Threat model

- **Trust boundary**: any client that can reach `/v1/messages` with a valid
  CPA API key can send arbitrary headers. Such a client already has full use of
  the proxied credentials; raising the fingerprint baseline is a much smaller
  capability than that. The quorum rule (multiple observations across distinct
  sessions) prevents a single stray or mistyped request from changing the
  baseline, and the floor prevents downgrades regardless of input.
- **What a malicious client could achieve**: promote a *higher* Claude Code
  version than the operator runs (with a consistent package/runtime tuple).
  Consequence: the operator's native CLI requests stop matching the baseline
  and are cloaked to the promoted version. Anthropic would see a plausible
  newer version. The operator sees it in status/history and can reset.
- **What it cannot achieve**: downgrade; write arbitrary YAML (values are
  regex-bounded and double-quoted); write other keys; write when disabled or in
  dry-run; exfiltrate anything (the plugin makes no outbound network calls).
- **Management routes**: status is read-only; `observe` and `reset` require a
  non-simple custom header (`X-Auto-Baseline-Request: 1`) and same-origin fetch
  metadata when present, closing cross-site form/CORS riding of ambient
  reverse-proxy credentials. The unauthenticated resource route serves a static
  shell with no data.
- **Data at rest**: `state.json` records session IDs (opaque UUIDs) and
  fingerprints, never credentials or request bodies; it is written 0600. Status
  routes expose counts, never session IDs, and only the baseline header values
  from `config.yaml`, never the file contents.

## 9. Known limitations

1. **Compiled-default assumption.** When `config.yaml` omits a field the
   plugin assumes the defaults of `v7.2.146-3-g81e1b53` (shown in status as
   the assumed CPA build, next to the floors). After upgrading CPA to a build
   with a newer compiled default, the operator must raise
   `claude-min-version` / `codex-min-version` to that build's default (or set
   an explicit baseline); otherwise the plugin can regard the older assumed
   value as the effective baseline and write a version CPA already exceeds,
   an implicit downgrade.
2. **Entrypoint-specific measured shapes.** Promoting the baseline to the
   observed version means native `cli` / `sdk-cli` / `claude-vscode` clients at
   that exact version pass through with their own measured shape, which is the
   intent. CPA's *helper-profile* allowlists
   (`matchesMeasuredClaudeCodeHelperProfile`, `claude_client_detection.go:148`
   and the beta/shape tables around `:154+`) were measured against 2.1.220 and
   are not updated by this plugin; helper-request confirmation may therefore be
   less complete for newer versions than for 2.1.220. Cloaked `sdk-ts`/`sdk-py`
   requests are unaffected: they go out with the baseline UA either way.
3. **Home / store modes.** File writes do not change a Home-, Postgres-,
   object-store-, or git-store-managed configuration; the plugin detects these
   and disables writes (see section 7 and troubleshooting).
4. **Learns only what it sees.** A brand-new CLI version that has never made a
   request through the proxy cannot be promoted (use the `observe` route with a
   header dump if needed).
5. **Codex requires `disable-codex-cloaking: true`** for a learned UA to take
   effect (section 6). The plugin never sets that flag itself.
6. **One writer assumption.** Multiple CPA instances sharing one `config.yaml`
   would each learn independently; the sha256 check prevents lost updates but
   not redundant identical writes.
7. **Platform.** `/proc/self/cmdline` discovery of `-config` is Linux-only;
   elsewhere set `config-path` or rely on `<cwd>/config.yaml`.
