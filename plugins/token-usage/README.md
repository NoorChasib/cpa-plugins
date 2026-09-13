# Token Usage

Keep CPA-reported token usage in SQLite and view statistics from the CPA sidebar. These are observed usage records, not guaranteed lossless accounting or billing totals.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

The source still serves the existing published release during repository consolidation.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json"
  configs:
    token-usage:
      enabled: true
```

## First use and data

The published v0.1.1 targets **Linux amd64**, ABI 1 / RPC schema 6; its documented CPA target is v7.2.155. Check the [compatibility reference](REFERENCE.md#compatibility-and-verification) before using another host/platform.

Sign into CPA with **Remember password** enabled, then click **Token Usage** in the sidebar. The page uses the compatible remembered session on the same origin. It shows the last 24 hours by default; choose another interval or provider/model filter as needed.

In the standard image, the default database is `/CLIProxyAPI/plugins/data/token-usage/usage.sqlite`, inside your existing plugin volume. Preserve any explicit `database-path`; removing it does not move existing history. Back up the entire database directory while CPA is stopped, including any SQLite companions. See [operations](docs/operations.md) for storage requirements and recovery.

Token Usage collects CPA usage events and does not poll provider quota endpoints.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
