# Codex Catalog Filter

Show only the models you choose in Codex's model picker when Codex uses CPA as its provider. CPA sends Codex one catalog of every model it can route, including Claude, Grok, and OpenRouter aliases. This plugin filters that catalog on the way out. New GPT models added to CPA still appear automatically, and nothing changes on the Codex side.

Nothing else is touched: OpenAI-, Claude-, and Gemini-format model lists and every model request pass through unchanged.

Requires **CPA v8.0.0 or later** (verified on v8.0.4) on **Linux amd64**. Older CPA loads the plugin but never sends it a model list.

## Install

1. Keep your plugins directory persistent. In the standard Docker image, that is the existing volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to your existing `plugins.store-sources` list if it is not there yet, then install **Codex Catalog Filter** from CPA's Plugin Store.
3. Merge the `include` list below into the plugin's configuration. Do not create a second `plugins:` mapping.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
  configs:
    codex-catalog-filter:
      enabled: true
      include:
        - "gpt-[0-9]*"
        - "codex-*"
```

`gpt-[0-9]*` matches GPT models such as `gpt-6-sol` and `gpt-5.5`, but not `gpt-image-2`. Keep `codex-*`: it retains `codex-auto-review`, the hidden model Codex uses when `approvals_reviewer = "auto_review"`.

Until `include` lists at least one pattern, the plugin leaves every catalog unchanged.

## First use

1. If your Codex `config.toml` sets `model_catalog_json`, remove it. While it is set, Codex never asks CPA for models.
2. Delete `~/.codex/models_cache.json`, or wait up to five minutes for Codex to refresh it.
3. Run `codex debug models`. Only models matching `include` should have `"visibility": "list"`.

Options:

| Key | Default | Meaning |
| --- | --- | --- |
| `include` | none | Glob patterns for the model slugs to keep. `*` matches any characters, including `/`. |
| `exclude` | none | Patterns that drop a slug even when `include` matches it. |
| `action` | `remove` | `remove` deletes other entries. `hide` keeps them with `visibility: hide`, so they stay selectable by exact name. |

## More detail

- [Behavior, safety, caching, and compatibility reference](REFERENCE.md)
- [Verification record](docs/verification.md)
