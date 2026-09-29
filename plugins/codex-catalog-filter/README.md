# Codex Catalog Filter

Give Codex a model list that contains only the models you choose. CPA's own Codex catalog includes every model CPA can route, such as Claude, Grok, and OpenRouter aliases. This plugin serves a filtered copy at its own URL, and Codex's `model_catalog_url` points there. New GPT models added to CPA still appear automatically.

CPA's own model lists are not changed. Any Codex install without `model_catalog_url` still sees everything.

Verified on **CPA v8.0.4**, **Linux amd64**, with Codex CLI 0.159.

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

`gpt-[0-9]*` matches GPT models such as `gpt-6-sol` and `gpt-5.5`, but not `gpt-image-2`. Keep `codex-*`: it retains `codex-auto-review`, the hidden model Codex uses when `approvals_reviewer = "auto_review"`. Until `include` lists at least one pattern, the URL serves CPA's catalog unfiltered.

## Point Codex at it

In Codex's `config.toml`, add `model_catalog_url` to the provider that already uses CPA, and remove any `model_catalog_json` line:

```toml
[model_providers.cliproxyapi]
base_url = "http://your-cpa-host:8317/v1"
model_catalog_url = "http://your-cpa-host:8317/v0/resource/plugins/codex-catalog-filter/models"
```

The URL needs no key of its own. Codex sends its usual CPA key with the request, and the plugin uses that key to read CPA's catalog.

Then delete `~/.codex/models_cache.json`, or wait up to five minutes, and run `codex debug models`. Only models matching `include` should have `"visibility": "list"`.

Options:

| Key | Default | Meaning |
| --- | --- | --- |
| `include` | none | Glob patterns for the model slugs to keep. `*` matches any characters, including `/`. Exact names work too. |
| `exclude` | none | Patterns that drop a slug even when `include` matches it. |
| `action` | `remove` | `remove` deletes other entries. `hide` keeps them with `visibility: hide`, so they stay selectable by exact name. |
| `cpa-url` | `http://127.0.0.1:8317` | Where the plugin reads CPA's own catalog. Change it only if CPA listens elsewhere inside its container. |

## More detail

- [Behavior, safety, caching, and compatibility reference](REFERENCE.md)
- [Verification record](docs/verification.md)
