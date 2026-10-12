# Preserve settings and data during updates

All six plugins are developed, packaged, and published from [NoorChasib/cpa-plugins](https://github.com/NoorChasib/cpa-plugins). Install and update each plugin independently through CPA's Plugin Store.

## Store source

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

`preview/registry.json` is an identical compatibility alias. If you installed from that URL, keep it: CPA v7.2.155 derives source identity from the exact URL, so swapping aliases is a source change.

## Update an installed plugin

1. Keep a private backup of `config.yaml` and persistent plugin data.
2. Refresh the Plugin Store and update the selected plugin. CPA saves the new version and artifact metadata while preserving its configured options.
3. Follow any restart prompt, then confirm the plugin is registered and its sidebar shows the expected settings and data.

Do not uninstall merely to update. In CPA v7.2.155, uninstalling removes the plugin's configuration subtree. A reinstall needs those options restored. A source conflict also requires CPA's supported uninstall/reinstall procedure; save the plugin settings first and restore options without the generated `store` block before installing from the source above.

## Data paths to preserve

| Plugin | Preserve |
| --- | --- |
| Account Health Pushover | `state-file`; default `<auth-dir>/.plugin-state/account-health-pushover/state.ahp` |
| Auto Baseline | `state-dir`, `backup-dir`, `config-path`, and learned header defaults in CPA config |
| Reset Priority | Auth files containing priority/quarantine values and all policy settings |
| Quota Cache | `cache-path`; default `<CPA cwd>/plugins/data/quota-cache/snapshot.json` and its whole directory, which also holds the API meter and operator data (below) |
| Quota Glance | `data-dir`; default `<CPA cwd>/plugins/data/quota-glance` and its whole directory: `history.json`, the generated web token in `web-token`, and `settings.json`, which Quota Cache 0.2.0 imports (below) |
| Token Usage | `database-path`; default `<CPA cwd>/plugins/data/token-usage/usage.sqlite` and its whole directory |

Quota Cache's directory is not only cache. Beside the snapshot of observations, request history, and provider cooldowns, it keeps `snapshot.meter.json`, the Claude API usage counted for the credit estimates. From 0.2.0 it also keeps the only copy of what you enter on Quota Cache's page: the organizations in `snapshot.organizations.json`; monthly credits, refill dates, Console readings, and Claude renewal dates in `snapshot.settings.json`; and the record of the import from Quota Glance in `snapshot.settings-import.json`. Quota Cache writes `snapshot.credits.json` again from them within a minute. The names follow `cache-path`: for `/data/qc.json` they are `qc.meter.json` and so on. Move and restore them with the snapshot. A location without them looks like a first start: Quota Cache moves the configuration's `claude-api-credits` list again, or starts with no organizations, and imports Quota Glance's old `settings.json` again, or starts empty, so every change made on the page since is lost. See [operator data beside the snapshot](../plugins/quota-cache/docs/cache-format.md#operator-data-beside-the-snapshot). Quota Glance 0.8.0 may also leave `snapshot.refresh.json` there: a short-lived request, written after a banked reset is spent, for Quota Cache to poll that account next and every other account after it. It holds nothing to keep, so leave it behind or delete it ([refresh after a banked reset](../plugins/quota-cache/docs/cache-format.md#refresh-after-a-banked-reset)).

Quota Glance's `settings.json` holds the monthly credits, refill dates, Console readings, and Claude renewal dates saved in Quota Glance 0.7 and older. Until Quota Cache 0.2.0 has imported it, it is their only copy: if a redeploy loses it, Quota Cache has nothing to import and starts empty. Quota Cache looks for it in `quota-glance` beside its own directory and in Quota Glance's default `data-dir`; for a `data-dir` elsewhere, place the drop-in that [importing Quota Glance's settings](../plugins/quota-cache/docs/credit-settings.md#importing-quota-glances-settings) describes. Keep the directory after the import too. Quota Glance 0.8.0 only reads the file, but if Quota Cache's settings file is lost, its page offers to import this file again, and a rollback to Quota Glance 0.7.2 copies Quota Cache's settings back into it. Unless a drop-in is in place, Quota Cache then picks up each save Quota Glance 0.7.2 makes to it, until a credit, date or reading is saved on Quota Cache's page ([rollback](../plugins/quota-cache/docs/credit-settings.md#rollback)).

Keep these paths on persistent Coolify storage. A consistent filesystem backup requires stopping CPA; copy SQLite's whole directory, including any `-wal` and `-shm` files. Retain volume names, ownership, and permissions. Pushover environment variables or secret files must remain available after redeployment.

For cache consumers, preserve `use-quota-cache: true` and a `quota-cache-path` matching Quota Cache's `cache-path`. Missing or stale observations cause consumers to wait without direct provider requests.

## Recovery

Stop CPA before restoring a coherent set of plugin libraries, configuration, and any required data backup. Restoring data discards changes made since that backup. Keep recovery libraries outside the plugin directory CPA scans so they cannot shadow the selected version.

See [release and update behavior](releases.md) and each plugin's README for installation and verification commands.
