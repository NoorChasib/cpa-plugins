# Troubleshooting

## Compatibility baseline

The plugin was audited against CLIProxyAPI commit `81e1b5374f99c212f196f34956eeed964a46b8fa` (`v7.2.146-3-g81e1b53`). The compiled fingerprint defaults it assumes when `config.yaml` omits a field are those of that build: `claude-cli/2.1.220 (external, cli)` / `0.94.0` / `v26.3.0` and `codex-tui/0.146.0 (...)`. The status route reports this as `assumed_cpa_version` and the HTML view shows it next to the floors.

**CPA upgrades need no action.** The assumed default matters only while `config.yaml` has no baseline block. Once the plugin has promoted once, CPA reads the block instead of its compiled constant, so a newer image neither resets nor lowers the baseline and the plugin keeps comparing against the on-disk value. If a newer build compiles in a default above your client's version while the block is still absent, the plugin writes your client's real tuple, which corrects the outbound fingerprint to what you actually run. After a CPA upgrade, revalidate the `docs/architecture.md` file:line references if you maintain the plugin.

## The plugin does not load

- Check `docker logs` for `pluginhost: plugin loaded plugin_id=auto-baseline`. If absent: the file must be `auto-baseline.so` at the plugin directory root, executable, built for the container's architecture and a GLIBC not newer than the image's (use a release artifact).
- `plugins.enabled` must be `true` and `plugins.dir` must point at the mounted directory.
- A `plugin registration failed ... invalid_config` line means the plugin rejected its own config subtree; the message names the offending key (for example `min-distinct-sessions (3) cannot exceed min-observations (2)`). CPA keeps serving with the static baseline.

## Status says the config file does not exist or is not writable

`config_file.path` shows which file the plugin resolved (`path_source` says how: `config-path`, `process -config flag`, or `cwd default`). If it is wrong, set `config-path` explicitly. If it is right but `writable` is `false`, the mount is read-only (`:ro`) or owned by another user; fix the mount. Learning continues while this is broken; nothing is promoted and `last_error` says `cannot promote ...`.

## Status says the backup dir is not writable

`backup.dir` (default: `state-dir`) must be creatable and writable; the plugin copies the previous `config.yaml` there before every write and refuses to promote otherwise. Set `backup-dir` to a persisted, writable directory.

## CPA runs in home, postgres, object-store, or git-store mode

`config_file.mode` / `mode_reason` report that CPA was started with `-home-jwt` / `HOME_JWT`, `PGSTORE_DSN`, `OBJECTSTORE_ENDPOINT`, or `GITSTORE_GIT_URL`. In these modes the file on disk is not what CPA serves from, so with `mode_unsupported: true` the plugin logs once, warns in status, and never writes. Options:

- If you know the local file is in fact the one CPA loads (for example a git-store checkout path), set `config-path` explicitly; that overrides the detection.
- Otherwise run a host-side updater: read the status route, take `providers[].pending_candidates` with `quorum_met: true`, and apply the value through CPA's own `PUT /v0/management/config.yaml` (or the store that feeds it). The plugin keeps learning and reporting; it just does not write.

## The explicit baseline in config.yaml is malformed

If `claude-header-defaults.user-agent` (or the Codex one) is set but does not parse as `claude-cli/M.m.p ...` (or `codex_cli_rs|codex-tui/M.m.p`), status shows `malformed_user_agent: true`, every observation for that provider is counted under `decisions.baseline_malformed`, and nothing is written. CPA itself falls back to its compiled version in this case, but the plugin refuses to guess what you meant. Evidence keeps accumulating; fix or remove the value and promotion resumes on the retained evidence at the next reload.

## config.yaml has a shape the plugin refuses to edit

`unsupported` in the provider's `effective_baseline` (and `last_error`) says `duplicate_key` when the file defines the same mapping key twice (CPA's own decoder rejects such a file with "mapping key ... already defined", so it would never hot-reload anyway) or `unsupported_config_shape` when the baseline key is a non-empty scalar, a sequence, an alias (`*name`), lives only inside a `<<: *defaults` merge, or is part of an alias/merge cycle; `multi_document_config` means the file contains more than one YAML document. The plugin reads well-formed alias/merge files correctly but will not rewrite them, because a yaml.v3 re-encode would destroy the shared structure, and it never follows a cycle (which would crash CPA). Put an explicit `claude-header-defaults:` mapping in the file; evidence is retained meanwhile.

## The plugin was disabled in config.yaml but status still shows it learning

At the audited revision the host drops a disabled plugin on reload without sending `plugin.quiesce`, so the in-process engine may keep observing until CPA restarts. It cannot write: the fresh read before every write checks `plugins.enabled` and `plugins.configs.auto-baseline.enabled` and refuses with `plugin_disabled_on_disk`. If a reconfigure carried an invalid plugin config, the plugin quiesces itself before returning the error.

## require-explicit-baseline blocks every promotion

With `require-explicit-baseline: true` a provider whose `config.yaml` carries no explicit `user-agent` is never promoted (`baseline_implicit`). Write the block once by hand (the current CPA compiled default is a fine starting value); the plugin raises it from there.

## Nothing is learned (counters stay at zero or everything is rejected)

- `counters.requests` at zero means the interceptor is not being called: the plugin is stopped/disabled, or CPA rejected the request before interception (for example `unknown provider for model`, HTTP 400: interceptors only run for routable models).
- `counters.ignored` counts requests whose User-Agent is not Claude Code or Codex CLI at all (curl, SDKs, browsers).
- `counters.rejected` with `reject_reasons` tells you which rule failed. Common ones:
  - `claude_entrypoint_not_allowed`: add the entrypoint to `claude-entrypoints`.
  - `claude_code_beta_missing`: the *inbound* request lacked `claude-code-20250219` in `anthropic-beta` and was rejected. Inbound count_tokens and helper requests often omit it (CPA adds it on the way out, which the plugin never sees) and then simply do not count; requests that carry it count normally. If your host never sends it on `/v1/messages`, set `require-claude-code-beta: false`.
  - `claude_package_version_malformed` / `claude_runtime_version_malformed`: the client does not send Stainless headers in the expected shape; the plugin refuses to guess.
- `decisions.not_newer_than_baseline` means the observed version is already the baseline (or older). `decisions.below_min_version` means it is below `claude-min-version` / `codex-min-version`. `decisions.baseline_malformed`, `duplicate_key`, and `unsupported_config_shape` mean the provider block on disk cannot be compared against (see below); `plugin_disabled_on_disk` means the fresh read showed the plugin disabled in `config.yaml`; `baseline_implicit` means `require-explicit-baseline` is set and no explicit baseline exists. In all of these the evidence is retained.
- Unknown or misspelled plugin config keys (`dry_run`, `min_observations`) are rejected at registration with `field ... not found`; CPA then refuses the plugin, so check the CPA log if the status route 404s after a config edit.

## A candidate is pending but never promoted

- Check `pending_candidates[].observations` and `distinct_sessions` against `rules`. Anonymous requests (no session header) never count as a session: with `min-distinct-sessions: 2` a single Claude Code session (one `X-Claude-Code-Session-Id`) or any number of header-less Codex requests never satisfies it; start a second session or set the rule to 1 (the default).
- Changing any classifier rule (`claude-entrypoints`, `require-claude-code-beta`, `min-*`, `observation-window`, floors, `manage-*`) clears all pending evidence; it must be re-collected under the new rules.
- `faulted: true` in status means a plugin goroutine panicked (see `fault_reason` / `last_error`); writes stay suspended until the plugin is reconfigured (any config.yaml change) and the CPA process was not affected.
- `next_write_allowed_at` shows a cooldown in effect.
- `dry_run: true` records the promotion in history but writes nothing.

## config.yaml was written but the outbound version did not change

- The provider shows `awaiting_reload: true` until CPA reloads (any `plugin.reconfigure`, or a fresh read of the file by the plugin, confirms it). After two minutes without confirmation status warns.
- CPA must log `config file changed, reloading`. If it does not, the watcher is not watching that path (an unsupported deployment mode, or CPA started with a different `-config` than the plugin edited).
- **Home / store modes**: see the section above; the plugin should already have refused to write.
- **Codex**: the learned UA takes effect only with `codex.disable-codex-cloaking: true` (the status page warns while it is off).
- A native `cli` request whose version equals the new baseline now passes through with its own shape; an `sdk-ts`/`sdk-py` request is cloaked to the baseline UA. Both carry the promoted version.

## The baseline was promoted to a version I do not run

Someone sent Claude-Code-shaped requests with a newer version through the proxy (see the threat model in `docs/architecture.md`). Restore `<backup-dir>/config.yaml.auto-baseline.bak` or edit the block by hand; CPA hot-reloads, the plugin adopts the on-disk value, and it will not re-promote an equal or older version. Use `POST .../reset` to drop pending evidence, and consider raising `min-observations` / `min-distinct-sessions` (to 2 if your clients send session IDs) or narrowing `claude-entrypoints`.

## config.yaml looks truncated or empty after a failed write

The plugin never truncates before writing (it writes the new bytes, then shrinks the file, then fsyncs) and rewrites the previous bytes back if any step fails, so this should not happen; but crash atomicity cannot be guaranteed on a single-file bind mount because no rename is possible. Restore from `<backup-dir>/config.yaml.auto-baseline.bak`.

## `last_error` mentions "changed during read-modify-write"

Another writer (the management console, CPA's secret-key hashing at startup, an editor) modified `config.yaml` between the plugin's read and write. The plugin retried three times and gave up for now; the candidate stays pending and the next observation retries.

## The state file is missing or was reset

A missing `state.json` is normal on first start and produces no warning. A corrupt one (`state_file.error` says `decode state file`, schema mismatch, or oversize) makes the plugin start fresh with a logged warning; evidence must be re-collected. Individual restored candidates that fail validation are dropped and counted in `state_file.restored_dropped`. Baselines are always re-read from `config.yaml`, so nothing is downgraded.

## observe, reset, or dry-run returns 403

All three require `X-Auto-Baseline-Action: 1`. When a browser sends fetch metadata, only `Sec-Fetch-Site: same-origin` or `none` is accepted. Without fetch metadata (plain HTTP on a non-loopback host, the usual private-deployment case) the gate falls back to the `Origin` header: a single well-formed `http://` origin is accepted, because mixed-content blocking means only a plain-HTTP page can post to a plain-HTTP server, and any `https://`, `null`, multi-valued, or malformed origin is refused. So the sidebar actions work over plain `http://192.0.2.10:8317`-style URLs; if they return 403 there, the request reached CPA with a rewritten or stripped `Origin` (a reverse proxy in between) or without the action header. Non-browser clients only need the management key and the action header. `scripts/smoke-test.sh ... --browser` reproduces the accepted and refused shapes against a real CPA.

## The sidebar page stays on the redacted view

The redacted page upgrades itself only when the browser already holds a same-origin management session: the script reads the key the official management console stored in this origin's localStorage (`cli-proxy-auth`, with "Remember password" on) and fetches the authenticated view with it. It stays redacted when the console runs on a different origin or port than the CPA it manages, when the key was not remembered, or when the console has never been signed in from this browser; the note under the header says which. Sign in to the console served from the same origin with the key remembered and reload, or open `GET /v0/management/plugins/auto-baseline/status/html` through a client that supplies the management header. In Home mode CPA returns 404 for the resource route itself.

## The dry-run switch says "awaiting reload" and never confirms

The route only edits `config.yaml`; the runtime flag flips when CPA hot-reloads the file and reconfigures the plugin. If it stays pending for more than two minutes status warns: check that CPA logged `config file changed, reloading`, that the file the plugin edited is the one CPA watches (see the deployment-mode section), and that the edit did not get rewritten by another writer. `POST .../dry-run` answers 409 while a promotion write is in flight (retry), 422 when `plugins.configs.auto-baseline` is missing or the file shape is unsupported (the plugin never creates the subtree), and 503 when writes are disabled.

## Management routes return 404 or 503

`404`: the route path is wrong or `management_api` registration failed (check logs). `503`: `plugin.register` has not completed yet.

## Reinstalling at the same library path fails to initialize until CPA restarts

Once the resident process has run the plugin's terminal native shutdown, `cliproxy_plugin_init` for the same path is refused by design (a Go runtime cannot be reinitialized after `dlclose`). Restart CPA after any in-place update.

## Smoke test

`scripts/smoke-test.sh <CPA binary> [auto-baseline.so]` runs the full flow locally and keeps evidence (CPA log, before/after config, backup, state.json, status JSON/HTML) under `dist/smoke/<run-id>/`. It needs `curl`, `python3` and a CPA binary built with `CGO_ENABLED=1 go build ./cmd/server`. Requests fail with `auth_unavailable` / connection refused by design: the placeholder Claude credential points at a closed local port.
