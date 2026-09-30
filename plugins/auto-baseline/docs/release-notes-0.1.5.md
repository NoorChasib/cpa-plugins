# Auto Baseline 0.1.5

Auto Baseline now understands CPA v8's `config.yaml` layout, so CPA's Management Center panel is safe to use again. Plugin ID, routes, settings, and the state and backup paths are unchanged.

**Upgrading from 0.1.4 on CPA v8:** switch to dry-run with the plugin's own switch *before* updating, then check status and switch back. See [the upgrade steps](install-docker-compose.md#upgrading-from-014-to-015-cpa-v8).

## Why

Any write through `/v8/management` rewrites the whole file into the v8 layout (`config-version: 8`), and the panel only uses v8, including for plugin installs and updates. 0.1.4 only knew the legacy keys. After such a save it found no root `claude-header-defaults`, assumed CPA's old compiled 2.1.220, and wrote a root block every `promotion-cooldown`. CPA deleted that block on every reload, because the v8 key wins, so the promotion never took effect.

## Changes

- **Both layouts, leaf by leaf.** Each baseline value resolves the way CPA v8 does: `oauth.providers.<provider>.header-defaults.<field>`, then the legacy `<provider>-header-defaults.<field>`, then CPA's compiled default. A present v8 key wins even when it is `false`, blank, or null. `disable-codex-cloaking` resolves the same way. Status reports the source of every value (`v8`, `legacy`, `default`), the file's `layout`, and each provider's `write_target`.
- **Writes land where CPA reads.** In order: the existing v8 block, the existing legacy block, the v8 path in a `config-version: 8` file, then the legacy path. The plugin never creates a legacy key beside its v8 counterpart. When it writes a v8 block it also removes the superseded legacy keys, so CPA has nothing to clean up and never rewrites the file after a promotion.
- **Loop guard.** Before writing, the plugin parses the rendered file with CPA's rules and refuses (`promotion_not_effective`) unless CPA would load exactly the promoted tuple. After a write, if a later read shows the file was rewritten without it, the promotion is marked not effective and the provider pauses, with a status warning, instead of being rewritten every cooldown. The pause survives restarts; **Clear pending** (`POST .../reset`) or a plugin setting change such as `dry-run` resumes it.
- **Dry-run previews the real write,** including its target block and every refusal a live write would hit.
- **Compiled defaults refreshed to CPA v8.0.4** (`d33f63f`): Claude `claude-cli/2.1.280 (external, cli)`, package `0.112.1`, runtime `v26.3.0`; Codex `codex-tui/0.154.0 (...)`. The default floors follow: `claude-min-version` 2.1.280 and `codex-min-version` 0.154.0 (were 2.1.220 and 0.146.0). An explicit floor in your configuration is unchanged.
- **Status and state:** new `sources`, `write_target`, `layout`, `paused`, `disable_codex_cloaking`, and per-promotion `target` / `not_effective_at` fields. `state.json` keeps schema 1; the new fields are additive and 0.1.4 ignores them.
- **Smoke test** runs in the pinned `eceasy/cli-proxy-api:v8.0.4` image, in the legacy and v8 layouts (and optionally the interim layout), and asserts that CPA's runtime config carries the promoted tuple and that the promotion happens exactly once.

## Behavior to know about

In the v8 layout CPA applies `oauth.providers.*` values, including these header defaults, to OAuth credentials only. API-key credentials such as `api-keys.claude` no longer receive them, whereas the legacy root keys applied to both. This is CPA's behavior; the plugin documents it and does not work around it.

Install or update Auto Baseline from the [combined catalog](../../../README.md#get-started). This release targets Linux amd64.
