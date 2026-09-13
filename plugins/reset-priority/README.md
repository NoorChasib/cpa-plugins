# Reset Priority

Give higher priority to healthy Claude and Codex accounts whose regular weekly quota resets sooner. Provider groups are ranked independently.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

The source still serves the existing published release during repository consolidation.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-reset-priority/main/registry.json"
  configs:
    reset-priority:
      enabled: true
      dry-run: true
      reconcile-interval: 1h
```

## First use and data

Inspect Reset Priority's status page after discovery. Confirm the expected account order, then set `dry-run: false` to allow credential-priority writes. Existing installations should preserve their current dry-run setting during migration.

CPA's `routing.strategy: fill-first` is recommended for this hard-priority policy. Keep your existing routing settings during migration; changing routing is a separate decision.

This version reads provider quota endpoints directly. Its regular reconciliation interval does not govern every read: startup, account changes, reset deadlines, recovery, and manual refresh can also cause activity. Avoid repeated manual refreshes while investigating 429s.

Preserve the auth directory: written priority/quarantine values live in the physical credential files. Runtime scheduling state is rebuilt after restart.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)
