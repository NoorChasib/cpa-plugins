# Auto Baseline

Keep CPA’s Claude Code and Codex CLI fingerprint baselines current by learning genuine client headers and updating selected keys in CPA’s configuration.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

The source still serves the existing published release during repository consolidation.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-auto-baseline/main/registry.json"
  configs:
    auto-baseline:
      enabled: true
      dry-run: true
```

## First use and data

CPA must be able to write its existing `config.yaml`. Send normal requests from your Claude Code/Codex clients, then inspect Auto Baseline's status page. Once it reports the expected candidate, set `dry-run: false` to allow promotion. Existing installations should retain their current dry-run setting during migration.

For learned **Codex** baselines to affect outgoing requests, the existing implementation requires `codex.disable-codex-cloaking: true`. Review that deliberate setting before enabling it; see the [architecture reference](docs/architecture.md).

The default state directory is `plugins/auto-baseline` relative to CPA's working directory. Preserve it, any custom `state-dir`/`backup-dir`/`config-path`, and the promoted header defaults in `config.yaml`.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)
