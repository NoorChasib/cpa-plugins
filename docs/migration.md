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
| Quota Cache | `cache-path` snapshot containing observations, request history, and provider cooldowns |
| Token Usage | `database-path`; default `<CPA cwd>/plugins/data/token-usage/usage.sqlite` and its whole directory |

Keep these paths on persistent Coolify storage. A consistent filesystem backup requires stopping CPA; copy SQLite's whole directory, including any `-wal` and `-shm` files. Retain volume names, ownership, and permissions. Pushover environment variables or secret files must remain available after redeployment.

For cache consumers, preserve `use-quota-cache: true` and a `quota-cache-path` matching Quota Cache's `cache-path`. Missing or stale observations cause consumers to wait without direct provider requests.

## Recovery

Stop CPA before restoring a coherent set of plugin libraries, configuration, and any required data backup. Restoring data discards changes made since that backup. Keep recovery libraries outside the plugin directory CPA scans so they cannot shadow the selected version.

See [release and update behavior](releases.md) and each plugin's README for installation and verification commands.
