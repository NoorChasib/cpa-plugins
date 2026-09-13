# Move to the unified CPA plugin repository

## What changes now

The four plugin source repositories are consolidated. Plugin IDs, native library names, configuration keys, routes, and existing data defaults are preserved. The combined catalog currently points at the same published releases in the old repositories. Switching catalogs alone is not a binary upgrade and does not enable shared quota caching.

Do not uninstall/reinstall merely because source code moved. CPA's store metadata includes source identity; switching the source must be checked against your installed CPA version before treating it as a transparent update.

Your reported target is CPA **v7.2.155 on Coolify**. These steps use its standard Docker/Compose layout; actual mounts, working directory, and custom paths still need confirmation from the deployment. Never replace your whole configuration with a quick-start example.

## 1. Record and back up the current installation

Record installed plugin IDs/versions, CPA image tag and digest, plugin directory, enabled states, and your current custom store sources. Keep a private copy of the existing configuration; it can contain secrets.

Stop CPA cleanly for a consistent filesystem backup. Back up:

| Item | Why it matters |
| --- | --- |
| Existing `config.yaml` | Plugin options, enabled/dry-run state, store metadata, routing, and learned client baselines |
| Entire auth directory/volume, including hidden directories | OAuth credentials, saved priorities, and Account Health state |
| Entire plugins directory/volume | Installed native libraries, Auto Baseline state/backups, default Token Usage database |
| Any paths outside those volumes selected by configuration | Custom state files, SQLite directory, and config backup directory |
| Deployment secret settings | Pushover environment/file settings must remain available after redeployment |

Keep the existing backup private. For SQLite, copy the whole containing directory while stopped, including `-wal`/`-shm` companions if present. Do not copy only a live `usage.sqlite` file.

Do not use `docker compose down -v`, delete/recreate volumes, change their names, or remove custom data-path settings. A different Compose project can silently create new empty named volumes; retain the actual existing volume identities.

## 2. Preserve each plugin's data location

| Plugin | Preserve |
| --- | --- |
| Account Health Pushover | Existing `state-file`; default `<auth-dir>/.plugin-state/account-health-pushover/state.ahp` and the auth directory |
| Auto Baseline | Existing `state-dir`, `backup-dir`, `config-path`; default `<CPA cwd>/plugins/auto-baseline`, plus current baseline values in config |
| Reset Priority | Auth files containing saved priority/quarantine values; all existing policy settings. In-memory scheduling is rediscovered after restart. |
| Token Usage | Existing `database-path`; default `<CPA cwd>/plugins/data/token-usage/usage.sqlite` and its whole directory |

Changing the source repository does not require changing any of these paths. Keep file ownership, permissions, and CPA's working directory the same.

## 3. Switch store source on CPA v7.2.155

The public combined source is:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

**Do not just replace the old source URLs and click Update.** In v7.2.155, CPA rejects a different source with `plugin_store_source_conflict`. Its source identity is derived from the registry URL, even when both catalogs point at the same release. The supported error guidance is to uninstall before switching, and **uninstall also deletes that plugin's saved configuration**. It does not recursively delete the plugin data directory in the inspected implementation.

The lowest-disruption option is to keep current installations and old store sources while using the new repository for development. There is no runtime penalty for doing this.

For an actual store-source switch, migrate one currently enabled plugin at a time:

1. Complete the stopped, consistent backup above. Save a separate private copy of that plugin's complete `plugins.configs.<id>` subtree.
2. Disable that plugin and follow CPA's restart requirement so its native library can be removed. Do not delete any volumes or state directories.
3. Use CPA's plugin uninstall action. Expect its config subtree to disappear. Leave its data directory and auth files intact.
4. Stop CPA in Coolify. Restore the saved plugin options into `config.yaml`, **excluding the old generated `store` subtree**, and set that plugin's `enabled: false` temporarily. Preserve every custom data path and policy setting. Keep unrelated configuration untouched.
5. Add the combined source and remove only this plugin's old source (preserve the others until their turns). Start CPA with the restored options present.
6. Install the same plugin ID from the combined source, selecting the intended version. v7.2.155's install path preserves the other raw configuration fields while setting `enabled: true` and writing new store metadata. Installing auto-enables the plugin, so complete option restoration before this step.
7. Follow the restart prompt, then compare the effective settings and historical data with the backup. Do not migrate the next plugin until this one checks out.

For an intentionally disabled plugin, keep it disabled/uninstalled until you are ready for the install operation's automatic enablement. Never enable mutating behavior merely to match a quick-start example; restore the previous `dry-run`, routing, and provider selections.

These rules are grounded in CPA tag v7.2.155 (`7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`): `validatePluginStoreInstallSource`, `DeletePlugin`, and `enablePluginConfigLocked`. The live deployment's actual config/volume paths still require inspection. Rehearse the complete source-switch sequence on a disposable copy before a production switch if your deployment has custom storage or startup automation.

The combined stable catalog continues to serve existing release artifacts. Switching to it does not by itself install the new quota cache or updated cache consumers.

## 4. When new binaries are released

Upgrade one plugin at a time through CPA's supported update/restart procedure. Do not overwrite a loaded native library. Ensure only the intended version of each plugin is selected in scanned plugin directories; an old versioned library can shadow a manually copied replacement.

Retain the backed-up library version outside scanned plugin directories. Preserve generated store metadata through supported CPA operations rather than editing it by hand.

Before migrating quota consumers, the quota cache must be installed, configured, and verified. Changing store sources alone cannot redirect their HTTP requests. Each consumer needs a release with explicit cache support.

## 5. Verify after restart

- Account Health discovers the same accounts and retains incident/delivery state. Inspect status first; use a test notification only when you want an actual message.
- Auto Baseline retains learning history and the promoted baseline values in config.
- Reset Priority sees the same auth files and expected priorities; verify dry-run/enabled settings did not change.
- Token Usage opens the original database and shows a known historical interval, then retains newly observed events across another restart.
- Existing volume names, file paths, ownership, and plugin settings match the pre-migration record.

A plugin appearing in the store does not prove its native binary loaded or its old data was opened.

## Rollback

Stop CPA. Restore the previous plugin libraries, configuration/source metadata, and image version as a coherent set. Restore data from the stopped backup only if required; doing so discards changes since the backup. Keep current files separately before restoring. Start CPA and repeat the checks above.

## Source provenance

The initial imports retain history and use these source heads:

| Plugin | Source branch | Imported commit | Latest published release observed |
| --- | --- | --- | --- |
| Account Health Pushover | main | `870456ecdbf3a86c76c6274f1d02e14dadddabf4` | v0.4.0 |
| Auto Baseline | main | `a7f5946b90e41d2a89d800cb143561fae0b4d9ea` | v0.1.2 |
| Reset Priority | main | `d5dfcb2a8517d87c7741ec07100f6400c7db60c3` | v0.1.4 |
| Token Usage | feature/token-usage-sidebar | `c08159fbb8d2ae86f36c2a2fc5ddf38274155a62` | v0.1.1 |

Published release tags were checked with GitHub on 2026-09-13. Source heads and hosted release bytes are separate evidence. Old repositories/releases remain intact. Existing local uncommitted work consisted only of the consolidation spec, copied to root `docs/`.

## Release workflow transition

Root `.github/workflows/*-ci.yml` files run the adapted checks. Original plugin-local workflow files remain non-executing reference material for existing contract tests; GitHub discovers workflows only at the repository root. No old generic release triggers have been enabled in the new repo. A verified new publication workflow is required before producing new hosted binaries.
