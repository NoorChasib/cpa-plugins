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

## 3. Switch store source when the new catalog is published

The intended combined URL is:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

This URL is a target until the new repository is actually published. Verify that it loads and lists the four expected IDs before using it.

Keep the old source list in your backup. Replace only these four custom source entries with the combined source; preserve unrelated custom sources and CPA's built-in source. Keep the existing `plugins.configs` mappings exactly as they are, including all custom options and paths.

Restart/reload using the supported CPA flow, then inspect the store. Verify that the installed plugins remain enabled at the expected versions, with their settings intact. If CPA still associates an installed plugin with its previous source, use the version's supported source/update flow. Do not guess at generated `store` fields or uninstall to force reassociation. The exact reassociation step needs rehearsal against your deployed CPA version.

The combined catalog still serves the existing artifacts. You can complete the source-code consolidation while retaining the old store sources if source reassociation is not yet verified.

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
