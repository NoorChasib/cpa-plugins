# Architecture: auto-baseline

This document records the feasibility findings, the design decision, the safety
rules and how each one is enforced, the failure analysis, and the known
limitations of the `auto-baseline` plugin. Unless marked otherwise, CLIProxyAPI
(CPA) file:line references are to commit
`81e1b5374f99c212f196f34956eeed964a46b8fa` (`v7.2.146-3-g81e1b53`), where the
design was first audited. The compiled defaults (section 1), the config layouts
and loop guard (section 2.4), and effective-value discovery (section 4, step 6)
were re-audited against **v8.0.4**
(`d33f63f8e3d98428440ebca5a5b6a981a61ff71e`); those references say so. The
`upstream` section CPA v8.0.11 added (section 2.5) was audited against
**v8.0.13** (`d7914af`).
Revalidate every file:line after a CPA upgrade.

## 1. The problem in CPA terms

CPA normalizes the outbound Claude fingerprint to a "measured baseline":

- Compiled defaults (v8.0.4): `claude-cli/2.1.280 (external, cli)`, package
  `0.112.1`, runtime `v26.3.0`, OS `MacOS`, arch `arm64`
  (`internal/runtime/executor/helps/claude_device_profile.go:23-27`); Codex
  `codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)`
  (`codex_executor_request.go:27`). v7.2.146 compiled `2.1.220` / `0.94.0` and
  `codex-tui/0.146.0`.
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
proxy stamps its compiled version (2.1.220 in v7.2.146) on a request for a
gated model the API refuses it even though the real client is newer.

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

### 2.4 The v8 config layout and the loop guard (re-audited against v8.0.4)

CPA v8 reads two layouts of `config.yaml`. The legacy layout has flat root keys;
the v8 layout (`config-version: 8`) nests them. `buildV8Paths`
(`internal/config/config_v8.go:31-103`) maps them leaf by leaf; the ones this
plugin touches are:

| Legacy leaf | v8 leaf |
| --- | --- |
| `claude-header-defaults.<field>` | `oauth.providers.claude.header-defaults.<field>` |
| `codex-header-defaults.<field>` | `oauth.providers.codex.header-defaults.<field>` |
| `codex.disable-codex-cloaking` | `oauth.providers.codex.disable-codex-cloaking` |

`plugins.configs.<id>` stays at the root in both layouts (`v8AllowedRoots`,
`config_v8.go:419-426`).

What CPA does:

- **Load.** `Config.UnmarshalYAML` calls `flattenV8` (`config_v8.go:154-231`).
  It rejects duplicate keys anywhere, expands aliases and merge keys
  (`expandConfigAliases`, `:662-695`), fails the whole load when an existing
  ancestor of any v8 path is not a mapping (`:191-206`) or `config-version` is
  anything but the integer 8 (`:208-210`), and then, for every v8 leaf that is
  *present*, overwrites its legacy leaf (`:215-220`). Presence decides, not the
  value: a v8 leaf that is `false`, `0`, blank, or null wins, and a blank string
  then falls back to the compiled default in `hdrDefault`
  (`claude_device_profile.go:129-134`). A legacy leaf with no v8 counterpart
  still applies.
- **Loading never migrates.** `LoadConfigOptional` runs
  `NormalizeConfigLayout(data, false)` (`config_load.go:209-222`), which only
  deletes legacy leaves that conflict with a present v8 leaf (and empty legacy
  blocks beside a v8 block), and rewrites the whole file when it did, with
  `yaml.Marshal`: 4-space indentation, anchors and merge keys expanded.
  `config-version: 8` on its own migrates nothing.
- **Only `/v8/management` writes migrate.** `SaveConfigPreserveComments(...,
  migrateV8=true)` (`config_yaml.go:15-110`) ends in
  `NormalizeConfigLayout(data, true)` (`config_v8.go:324-417`), which moves every
  legacy leaf to its v8 path and sets `config-version: 8`. The Management Center
  panel only uses `/v8/management`, so any panel save, including a Plugin Store
  install or update, migrates the file. `/v0/management` saves keep the layout
  (`restoreV8Layout`, `config_v8.go:586-640`; `docs/management-api-v8.md`).
- **Scope.** `UnmarshalYAML` records every present `oauth.providers.*` leaf in
  `OAuthOnlyFields` (`config_v8.go:164-173`), and `Config.ForAPIKey`
  (`internal/config/oauth_scope.go:12-34`) zeroes those fields for API-key
  credentials. In the v8 layout the header defaults and
  `disable-codex-cloaking` therefore reach OAuth credentials only. The plugin
  documents this and does not work around it.

What the plugin does (`internal/configfile`):

- **Read.** Each managed leaf resolves as v8 leaf, then legacy leaf, then the
  compiled default, with the alias/merge semantics above; the same order
  applies to `disable-codex-cloaking`. An existing non-mapping ancestor on the
  plugin's own v8 paths marks the provider `unsupported_config_shape`, and a
  non-8 `config-version` fails the read (`unsupported_config_version`). Status
  reports the source of every value (`v8`, `legacy`, `default`) and the file
  layout.
- **Write target**, per provider: the existing v8 block, else the existing
  legacy block, else the v8 path in a `config-version: 8` file, else the legacy
  path. The legacy path is chosen only when no v8 block exists, so the plugin
  never creates a legacy leaf beside its v8 counterpart. Writing a v8 block also
  removes the legacy leaves it superseded (and an emptied, un-anchored legacy
  block): exactly what `NormalizeConfigLayout(false)` would delete, so CPA has
  nothing to clean up and never rewrites the file after a promotion.
- **Loop guard before the write.** The rendered bytes are parsed again with
  these rules; unless they yield exactly the candidate for its provider, and
  unchanged values for everything else the plugin reads, nothing is written
  (`promotion_not_effective`) and the provider pauses until the file hash
  changes. `dry-run` runs the same read, check, render and guard (`Preview`).
- **Loop guard after the write.** The promotion awaits reload as before. Every
  config read first captures a write epoch, so only a read that began after the
  write was recorded can judge it. Such a read showing a different tuple means
  the file was rewritten without the promotion; it is marked
  `not_effective_at`, the provider is paused (persisted in `state.json`, so a
  restart does not restart the loop), `last_error` and a status warning explain
  it, and nothing is retried. The operator resumes it with `POST .../reset`, a
  write-affecting plugin setting change, or by restoring the tuple. A newer
  value or an unreadable block on disk ends the wait without a pause.

The 2026-09-29 incident this closes: 0.1.4 knew only the legacy keys. After a
`/v8/management` write migrated production's file, it found no root
`claude-header-defaults`, assumed its compiled 2.1.220, and wrote a root block
every `promotion-cooldown`; CPA deleted it on every load because the v8 block
wins, so the promotion never took effect. `scripts/smoke-test.sh` reproduces
that layout (`CPA_SMOKE_LAYOUT=v8`) and asserts exactly one effective promotion.

Verification against CPA's real loader: the fixtures in
`internal/configfile/layout_test.go` can be exported
(`AUTO_BASELINE_V8AUDIT_DIR=<dir> go test ./internal/configfile/`) as pairs of
a fixture and a legacy-only "twin" carrying the plugin's reading of it. Loading
each pair with v8.0.4's loader gave identical header defaults and cloaking in
every read case; CPA refused the non-mapping-parent case exactly as the plugin
does; and none of the files the plugin writes is rewritten on load.

### 2.5 The upstream section (CPA v8.0.11+, audited against v8.0.13)

CPA v8.0.11 (commits `52d5507d` "add shared upstream provider settings" and
`3be5fa44` "support historical v8 aliases") moved several v8 keys under a new
`upstream` root. References below are to v8.0.13 (`d7914af`); the files involved
are unchanged through v8.0.16. For this plugin:

| Setting | Canonical from v8.0.11 | Historical alias (v8.0.4 canonical) | Legacy |
| --- | --- | --- | --- |
| Claude header defaults | `upstream.claude.header-defaults.<field>` | `oauth.providers.claude.header-defaults.<field>` | `claude-header-defaults.<field>` |
| Codex cloaking | `upstream.codex.disable-codex-cloaking` | `oauth.providers.codex.disable-codex-cloaking` | `codex.disable-codex-cloaking` |
| Codex header defaults | not moved: `oauth.providers.codex.header-defaults.<field>` | | `codex-header-defaults.<field>` |

(`v8SharedPaths` and `v8SharedStructPaths`, `config_v8.go:34-64`; `buildV8Paths`
`:93,96`.)

What CPA v8.0.11+ does differently:

- **Load.** `flattenV8` first copies each present alias leaf to its upstream
  path only when that path is absent, and always drops the alias
  (`config_v8.go:297-304`, "canonical upstream fields win by presence"); it
  then moves every present upstream leaf over the legacy leaf as before
  (`:330-336`). Precedence is per leaf: upstream, then alias, then legacy, then
  the compiled default.
- **Loading cleans conflicts and writes the file.** `NormalizeConfigLayout(data,
  false)` (`config_v8.go:452-535`, called at `config_load.go:212-226`) deletes
  every alias or legacy leaf whose upstream leaf exists, prunes the mappings
  that leaves empty (`deleteYAMLPath`, `:214-231`), deletes an empty or null
  alias or legacy struct beside its upstream counterpart, and `os.WriteFile`s
  the result. An alias with no upstream counterpart is kept and applies.
- **`/v8/management` saves migrate aliases too.** `SaveConfigPreserveComments`
  forces migration for any v8 document (`config_yaml.go:42`, `:107`), which
  moves alias leaves under `upstream`. That includes the plugin Store's delete
  (`management/plugins.go:388`). v8.0.4 has no `upstream` root at all
  (`v8AllowedRoots`), so a file with one was written by v8.0.11 or later.

What the plugin does (0.1.6):

- **Read.** Every Claude leaf and `disable-codex-cloaking` resolve as upstream,
  then v8 (now the alias), then legacy, then the compiled default, by presence.
  Status reports `upstream` as the source. A non-mapping ancestor on an upstream
  path marks the provider `unsupported_config_shape`, as CPA refuses to load it.
- **Write target.** For Claude, `upstream.claude.header-defaults` whenever the
  file has an `upstream` root (`Snapshot.Upstream`); the plugin cannot ask CPA
  for its version (the ABI exposes only ABI and schema versions), so the file is
  the evidence. Otherwise section 2.4's rules apply unchanged, so v8.0.4-v8.0.10
  files are written exactly as by 0.1.5. Codex is never written under
  `upstream`.
- **Superseded leaves.** An upstream write removes the written keys from the
  alias block and the legacy block, and every plain, un-anchored mapping that
  leaves empty, exactly what `NormalizeConfigLayout(false)` would delete. CPA's
  reload then has nothing to clean and never writes the file. `os`, `arch`,
  `timeout`, `timezone` and `stabilize-device-profile` stay wherever they are.
  Nothing is pruned from a mapping with a merge key (that could expose a merged
  value) and no anchored leaf is removed (that would leave an alias dangling);
  such shapes, an alias-shared block, or conflicts already in the file are left
  for CPA, which normalizes the file once and keeps the written values.
- **Shape.** `upstream`, `upstream.claude`, `upstream.claude.header-defaults`
  and `upstream.codex` must be mappings when present, or CPA refuses the whole
  file; the plugin marks the provider that owns the path unsupported (both, for
  `upstream` itself) instead of writing into a file CPA cannot load.
- **Downgrades.** v8.0.4-v8.0.10 ignore an `upstream` section silently. After a
  downgrade to such a build, a leftover `upstream` root keeps the plugin
  writing there and confirming values CPA does not apply. Remove the section
  (moving its Claude block back to `oauth.providers.claude.header-defaults`)
  when downgrading.

The 2026-10-04 incident this closes: production ran CPA v8.0.12/v8.0.13 with
the Claude block at the alias path, which still applied. At 11:50:37 CST a
panel `DELETE /v8/management/plugins/token-usage` re-saved the file, moving the
block to `upstream.claude.header-defaults` (2.1.289). 0.1.5 no longer found it,
reported the compiled default 2.1.280, and wrote 2.1.289 to the alias path;
CPA ignored that write, its reload deleted it as a conflicting legacy field
(the second `WRITE` event ~155 ms later, inside `LoadConfig`), and the plugin
paused with `promotion_not_effective`. **Clear pending** on 2026-10-07 repeated
it with 2.1.291. CPA kept serving the upstream 2.1.289 throughout, and the
status page's Codex cloaking warning was also false, because 0.1.5 did not read
`upstream.codex.disable-codex-cloaking: true`.

Verification against CPA's real loaders, with the fixture export of section
2.4: on v8.0.13 and v8.0.16 every read fixture, including the six `upstream`
ones and production's shape, loads to exactly the plugin's reading (the null
alias parent case is refused by the plugin and loaded by v8.0.11+, which is
conservative), and every file the plugin writes is left alone by
`NormalizeConfigLayout(false)` except the three shapes deliberately left for
CPA above, whose written values survive CPA's normalization and re-read
identically. On v8.0.4 every fixture without an `upstream` root still passes,
and 0.1.6 renders byte-identical output to 0.1.5 for those files. A private
copy of production's `config.yaml` was promoted to 2.1.292 by 0.1.6: v8.0.13's
normalizer left the result unchanged and its loader served 2.1.292, while the
same run with 0.1.5 reproduced the incident (alias written, deleted on load,
2.1.289 served).

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
   what is on disk now", edit only the baseline keys of the write target
   (section 2.4) with the yaml.v3 Node API, run the loop guard, back up,
   re-hash, write in place, and record the promotion as `awaiting_reload`
   until the next `plugin.reconfigure` (CPA reconfigures plugins on every
   reload: `ApplyConfig` calls `callRegister` for every loaded plugin,
   `internal/pluginhost/host.go:366,1035-1046` in v8.0.4) or a fresh read
   shows the value. A fresh read showing something else pauses the provider
   (section 2.4). `dry-run` runs everything but the write and logs the target. The worker is single-flight with a
   rescan flag and a bounded forced-promotion queue, so work that becomes
   ready while it runs is never lost; `Stop` waits for it (write barrier).
5. **Persistence** (`internal/statefile`): `state-dir/state.json`, atomic
   temp+rename (the state dir is a normal directory). Saved on promotion, every
   5 minutes when dirty, and on quiesce/shutdown. Corrupt or missing state
   starts fresh with a warning.
6. **Effective baseline discovery** (`internal/configfile`): at
   register/reconfigure and before every promotion the plugin resolves the
   Claude and Codex header defaults and `disable-codex-cloaking` leaf by leaf
   in both layouts (section 2.4). Blank values are treated as absent, exactly
   like CPA's `hdrDefault` (v8.0.4 `claude_device_profile.go:129-134`). An
   absent field means the compiled default of the audited build, v8.0.4
   (constants in `internal/fingerprint/fingerprint.go`); this is an explicit
   assumption surfaced in status as `assumed_cpa_version`, and every value's
   source is reported.

## 5. Safety rules and enforcement

| Rule | Enforcement |
| --- | --- |
| Never downgrade, never re-promote an equal version | `learner.eligibleLocked` rejects candidates not strictly newer than the recorded baseline; `engine.promoteProvider` re-reads the file and runs the same check against the fresh on-disk value inside `configfile.Apply`'s `check` hook; a failed check adopts the on-disk baseline and drops the candidate without error. |
| Never overwrite an explicit value the plugin cannot read | An explicit `user-agent` that does not parse marks the provider `Malformed`; the learner keeps collecting evidence but reports `baseline_malformed` instead of readiness, the pre-write check refuses, and status warns. The compiled default is never used as a comparable baseline when an explicit value exists. When a later read shows a valid baseline, evaluation resumes on the retained evidence. `require-explicit-baseline: true` additionally refuses any implicit baseline (`baseline_implicit`). |
| Never write below a floor | `claude-min-version` / `codex-min-version` (defaults = compiled defaults) are enforced in the learner and again in the promotion check, protecting against a config file that carries an ancient explicit baseline. |
| Internally consistent tuples only | A candidate is built from one request; the key includes package-version and runtime-version; the learner never merges keys. Claude UA is canonicalized so `sdk-ts, agent-sdk/...` never reaches the baseline. |
| Only valid fingerprints | Regex-validated versions, bounded UA length (512), no control characters, safe OS/Arch charset; values are written as double-quoted YAML scalars so `0.94.0`-like values can never be re-read as floats. |
| Anti-poisoning | Quorum across distinct sessions within a window; `force` is only available through the authenticated management API with a CSRF header. |
| Touch nothing else in `config.yaml` | yaml.v3 Node edits of exactly the Claude header-defaults `user-agent`, `package-version`, `runtime-version`, or the Codex header-defaults `user-agent`, in the write target of section 2.4, plus removal of the lower-precedence leaves a write supersedes, and of mappings that leaves empty (which CPA would delete anyway, sections 2.4 and 2.5); missing mapping nodes are created; a null placeholder is converted in place; comments, order, `os`/`arch`/`timeout`/`timezone`/`stabilize-device-profile`, and `beta-features` are untouched (tested in `configfile_test.go`). |
| Never destroy operator content, never crash the host | A duplicate mapping key anywhere the plugin traverses is refused (`duplicate_key`): CPA's yaml.v3 struct decode rejects such a file ("mapping key ... already defined"), so it could never reload. Reads resolve aliases and `<<` merge keys (identified by the `!!merge` tag, never by scalar text) so a shared-defaults config yields the real effective values; resolution carries a visited set and a depth bound of 32, so an alias or merge cycle yields `unsupported_config_shape` instead of the stack overflow that would kill CPA. Writes refuse (`unsupported_config_shape`) when the target is a non-empty scalar, a sequence, an alias, or reachable only through a merge key, and refuse multi-document streams (`multi_document_config`). A problem confined to one provider's block only blocks that provider. |
| Never write what CPA would not load | The loop guard of section 2.4: the rendered file is parsed again with CPA's layout rules before the write, and a read after the write that shows a different tuple pauses the provider instead of retrying every cooldown (`promotion_not_effective`). |
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
| Keys written | `user-agent`, `package-version`, `runtime-version` of `oauth.providers.claude.header-defaults` (legacy `claude-header-defaults`) | `user-agent` of `oauth.providers.codex.header-defaults` (legacy `codex-header-defaults`) only, never `beta-features` |
| UA written | Canonical `claude-cli/<v> (external, cli)` (CPA compares the version only, `claudeCLIVersionPattern` prefix match `claude_device_profile.go:33`; the `cli` entrypoint is what cloaked requests announce) | The full observed UA verbatim; it carries OS/terminal details (`codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/...`) and CPA injects it as-is |
| Authenticity signals | x-app, anthropic-version, stainless lang/runtime/package/runtime-version/os/arch, claude-code beta | `Originator` header (`codex_cli_rs`, `codex-tui`, `Codex Desktop`, ...) |
| Effect once written | Immediate after hot reload (in the v8 layout, for OAuth credentials only) | **Only when `disable-codex-cloaking` is `true`** (`upstream.codex.` on CPA v8.0.11+, `oauth.providers.codex.` before that, legacy `codex.`). The references in this cell are to v7.2.146; in v8.0.4 the flag is still read from `cfg.Codex.DisableCodexCloaking` (`codex_executor_request.go:303-306`). `ensureHeaderWithConfigPrecedence` gives `codex-header-defaults.user-agent` precedence over the client UA (`codex_websockets_request.go:309-329`, used at `codex_executor_request.go:342`), but `applyCodexCloakingHeaders` (`codex_executor_request.go:372-378`) then forces the compiled `codexUserAgent` (`:26`) and `Originator` unless `cfg.Codex.DisableCodexCloaking` is set (`config_types.go:147-150`). The plugin reads that flag and shows a status warning when it is not set. |

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
- **Write discarded or reverted**: a read after the write that no longer shows
  the promoted tuple marks it not effective and pauses the provider (section
  2.4); nothing is rewritten until the operator resumes it.
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
- **Management routes**: status is read-only; `observe`, `reset`, and
  `dry-run` require a non-simple custom header (`X-Auto-Baseline-Action: 1`)
  and same-origin fetch metadata when present (a well-formed plain-`http://`
  `Origin` when the browser sends none), closing cross-site form/CORS riding
  of ambient reverse-proxy credentials. The unauthenticated resource route
  serves a redacted view (baselines, candidate tuples, counters; no paths,
  errors, or identifiers) that upgrades itself only with a management key the
  browser already holds for the same origin. The `dry-run` route is the one
  operator-driven write outside the baseline keys; it edits only
  `plugins.configs.auto-baseline.dry-run`, through the same node surgery and
  write discipline, and never creates the subtree.
- **Data at rest**: `state.json` records session IDs (opaque UUIDs) and
  fingerprints, never credentials or request bodies; it is written 0600. Status
  routes expose counts, never session IDs, and only the baseline header values
  from `config.yaml`, never the file contents.

## 9. Known limitations

1. **Compiled-default assumption.** When `config.yaml` omits a field the
   plugin assumes the defaults of v8.0.4 (`d33f63f`; shown in status as
   the assumed CPA build, next to the floors). The assumption is live only
   until the first promotion: from then on the block exists, CPA reads it in
   preference to its compiled constant, and the plugin compares against the
   on-disk value. A CPA upgrade therefore never resets or lowers a promoted
   baseline. If a newer build compiles in a default above the client's real
   version while the block is still absent, the plugin writes the client's
   real tuple; that corrects the outbound fingerprint to the client actually
   in use rather than downgrading it. `require-explicit-baseline: true` lets
   an operator opt out of the assumption entirely.
2. **Entrypoint-specific measured shapes.** Promoting the baseline to the
   observed version means native `cli` / `sdk-cli` / `claude-vscode` clients at
   that exact version pass through with their own measured shape, which is the
   intent. CPA's *helper-profile* allowlists
   (`matchesMeasuredClaudeCodeHelperProfile`, `claude_client_detection.go:148`
   and the beta/shape tables around `:154+`) were measured against that build's
   compiled version and are not updated by this plugin; helper-request
   confirmation may therefore be less complete for newer versions. Cloaked `sdk-ts`/`sdk-py`
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
8. **v8 scope.** In the v8 layout CPA applies the header defaults to OAuth
   credentials only (section 2.4); API-key credentials keep their own headers.
   The plugin cannot change that.
9. **Panel saves rewrite the file.** Any `/v8/management` write migrates the
   whole file and expands anchors and merge keys. The plugin follows the new
   keys, but a panel save made from a stale in-memory config can restore an
   older baseline over a fresh promotion; the loop guard then pauses the
   provider instead of fighting it.
