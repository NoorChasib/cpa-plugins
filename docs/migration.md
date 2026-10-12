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

### Updating from Quota Glance 0.7

**Before you start**, write down from Quota Glance 0.7.2 each organization's monthly credit and refill date, plus its Console reading if you use them, and each Claude account's renewal date. Quota Cache 0.2.0 doesn't read them, and Quota Glance 0.7.2's **Monthly API Credit** card is gone until the organizations are added again.

1. Update Quota Cache first.
2. Open Quota Cache's page. Under **Organizations**, add each Claude Console organization: a label and its Organization ID (Console → Settings → Organization). Use the same Organization IDs as before, so counting carries on.
3. Under **Claude API credits**, enter each organization's monthly credit and refill date, plus a Console reading if you use them. Enter each Claude account's renewal date where you had set one.
4. Then update Quota Glance, then Reset Priority.

Until Quota Glance is updated, save nothing in its editor: Quota Cache doesn't read those saves, and while only some organizations are added, Quota Glance 0.7.2 offers **Remove** for the others' saved values.

**Counting across the update.** Claude API spend is not counted from the update until the first organization is added. From then on the meter keeps the traffic of every organization Quota Cache 0.1.14 counted, so organizations added later lose nothing more. Add the organizations right after updating. If the gap matters, take a Console reading for each organization once it is added and enter it: the reading covers the gap.

`claude-api-credits` can stay in `config.yaml`: Quota Cache 0.2.0 ignores it and an older Quota Cache reads it, which keeps a rollback simple. Quota Glance's `settings.json` stays where it is, untouched and unread, and rolling Quota Glance back to 0.7.2 shows its old values again. To carry the values entered on Quota Cache's page back instead, first keep a copy of Quota Glance's `settings.json`. Then, while CPA is stopped, for example for the downgrade, copy `snapshot.settings.json` over Quota Glance's `settings.json`: the formats are identical. The copy is a one-off: a value changed afterwards on either side stays where it was changed. See [rollback](../plugins/quota-cache/docs/credit-settings.md#rollback).

## Data paths to preserve

| Plugin | Preserve |
| --- | --- |
| Account Health Pushover | `state-file`; default `<auth-dir>/.plugin-state/account-health-pushover/state.ahp` |
| Auto Baseline | `state-dir`, `backup-dir`, `config-path`, and learned header defaults in CPA config |
| Reset Priority | Auth files containing priority/quarantine values and all policy settings |
| Quota Cache | `cache-path`; default `<CPA cwd>/plugins/data/quota-cache/snapshot.json` and its whole directory, which also holds the API meter and operator data (below) |
| Quota Glance | `data-dir`; default `<CPA cwd>/plugins/data/quota-glance` and its whole directory: `history.json`, the generated web token in `web-token`, and `settings.json` from Quota Glance 0.7 (below) |
| Token Usage | `database-path`; default `<CPA cwd>/plugins/data/token-usage/usage.sqlite` and its whole directory |

Quota Cache's directory is not only cache. Beside the snapshot of observations, request history, and provider cooldowns, it keeps `snapshot.meter.json`, the Claude API usage counted for the credit estimates. From 0.2.0 it also keeps the only copy of what you enter on Quota Cache's page: the organizations in `snapshot.organizations.json`, and monthly credits, refill dates, Console readings, and Claude renewal dates in `snapshot.settings.json`. Quota Cache writes `snapshot.credits.json` again from them within a minute. The names follow `cache-path`: for `/data/qc.json` they are `qc.meter.json` and so on. Move and restore them with the snapshot. Only a save on the page writes the organizations file; nothing fills it from `claude-api-credits` in the configuration. So a location without these files looks like a first start: Quota Cache starts with no organizations and no saved values, and everything entered on the page must be entered again. See [operator data beside the snapshot](../plugins/quota-cache/docs/cache-format.md#operator-data-beside-the-snapshot). Quota Glance 0.8.0 may also leave `snapshot.refresh.json` there: a short-lived request, written after a banked reset is spent, for Quota Cache to poll that account next and every other account after it. It holds nothing to keep, so leave it behind or delete it ([refresh after a banked reset](../plugins/quota-cache/docs/cache-format.md#refresh-after-a-banked-reset)).

Quota Glance's `settings.json` holds the monthly credits, refill dates, Console readings, and Claude renewal dates saved in Quota Glance 0.7 and older. Neither Quota Glance 0.8.0 nor Quota Cache reads or writes it. Keep it with the rest of the directory all the same: until those values are entered on Quota Cache's page it is their only copy, and a rollback to Quota Glance 0.7.2 shows them from it again ([updating from Quota Glance 0.7](#updating-from-quota-glance-07)).

Keep these paths on persistent Coolify storage. A consistent filesystem backup requires stopping CPA; copy SQLite's whole directory, including any `-wal` and `-shm` files. Retain volume names, ownership, and permissions. Pushover environment variables or secret files must remain available after redeployment.

For cache consumers, preserve `use-quota-cache: true` and a `quota-cache-path` matching Quota Cache's `cache-path`. Missing or stale observations cause consumers to wait without direct provider requests.

## Recovery

Stop CPA before restoring a coherent set of plugin libraries, configuration, and any required data backup. Restoring data discards changes made since that backup. Keep recovery libraries outside the plugin directory CPA scans so they cannot shadow the selected version.

See [release and update behavior](releases.md) and each plugin's README for installation and verification commands.
