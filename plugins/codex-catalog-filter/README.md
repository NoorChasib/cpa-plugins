# Codex Catalog Filter

Choose, model by model, what Codex shows when it uses CPA. A **Codex Models** page in CPA's console lists every model CPA offers Codex, each with an on/off switch. The plugin serves the resulting list at its own URL, and Codex's `model_catalog_url` points there.

CPA's own model lists are not changed. Any Codex install without `model_catalog_url` still sees everything.

Verified on **CPA v8.0.4**, **Linux amd64**, with Codex CLI 0.159.

## Install

1. Keep your plugins directory persistent. In the standard Docker image, that is the existing volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to your existing `plugins.store-sources` list if it is not there yet, then install **Codex Catalog Filter** from CPA's Plugin Store.
3. Make sure the plugin is enabled. Do not create a second `plugins:` mapping.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
  configs:
    codex-catalog-filter:
      enabled: true
```

## Point Codex at it

In Codex's `config.toml`, add `model_catalog_url` to the provider that already uses CPA, and remove any `model_catalog_json` line:

```toml
[model_providers.cliproxyapi]
base_url = "http://your-cpa-host:8317/v1"
model_catalog_url = "http://your-cpa-host:8317/v0/resource/plugins/codex-catalog-filter/models"
```

The URL needs no key of its own. Codex sends its usual CPA key with the request, and the plugin uses that key to read CPA's catalog.

Start Codex once. That first fetch is what fills the model list on the Codex Models page.

## Choose your models

Sign in to CPA's console with **Remember password**, then open **Codex Models** from the sidebar. Switch models on or off, use the filter with **Enable shown** or **Disable shown** for groups, and select **Save**. Codex picks up the change on its next refresh, within five minutes; delete `~/.codex/models_cache.json` to see it at once.

Models you have not switched follow `new-models`: by default they are on, so a model CPA adds later appears in Codex until you switch it off.

The page saves your switches into the plugin's configuration, so you can also edit them directly:

```yaml
codex-catalog-filter:
  enabled: true
  models:              # true = in Codex, false = not
    claude-fable-5-1: false
    grok-4.7: false
  new-models: enabled  # models without a switch: enabled or disabled
  action: remove       # switched-off models: remove or hide
```

| Key | Default | Meaning |
| --- | --- | --- |
| `models` | none | Per-model switches: slug to `true` or `false`. |
| `new-models` | `enabled` | What a model without a switch does: `enabled` shows it, `disabled` leaves it out. |
| `action` | `remove` | `remove` deletes switched-off models. `hide` keeps them with `visibility: hide`, so they stay selectable by exact name. |
| `cpa-url` | `http://127.0.0.1:8317` | Where the plugin reads CPA's own catalog. Change it only if CPA listens elsewhere inside its container. |
| `data-dir` | `plugins/data/codex-catalog-filter` | Where the last model list is kept, so the page has it after a restart. |

## More detail

- [Behavior, safety, caching, and compatibility reference](REFERENCE.md)
- [Verification record](docs/verification.md)
