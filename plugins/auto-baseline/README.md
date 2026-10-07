# Auto Baseline

Keep CPA’s Claude Code and Codex CLI fingerprint baselines current by learning genuine client headers and updating selected keys in CPA’s configuration.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

This source lists all five plugins from this repository, including the optional quota-cache preview. If this plugin is already installed from an old source, follow the [migration guide](../../docs/migration.md) before switching; CPA v7.2.155 will otherwise reject the source change.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
  configs:
    auto-baseline:
      enabled: true
      dry-run: true
```

## First use and data

CPA must be able to write its existing `config.yaml`. Send normal requests from your Claude Code/Codex clients, then inspect Auto Baseline's status page. Once it reports the expected candidate, set `dry-run: false` to allow promotion. Existing installations should retain their current dry-run setting during migration.

The plugin reads and writes both CPA config layouts, and status shows which keys it uses. **Upgrading from 0.1.4 on CPA v8:** switch to dry-run with the plugin's own switch *before* updating; see [the upgrade steps](docs/install-docker-compose.md#upgrading-from-014-to-015-cpa-v8).

For learned **Codex** baselines to affect outgoing requests, the existing implementation requires `disable-codex-cloaking: true` (`upstream.codex.disable-codex-cloaking` on CPA v8.0.11+, `oauth.providers.codex.disable-codex-cloaking` before that, legacy name `codex.disable-codex-cloaking`). Review that deliberate setting before enabling it; see the [architecture reference](docs/architecture.md).

The default state directory is `plugins/auto-baseline` relative to CPA's working directory. Preserve it, any custom `state-dir`/`backup-dir`/`config-path`, and the promoted header defaults in `config.yaml`.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)

Quota Cache is not required or used by this plugin.
