# Model Picker

Choose, client by client, which of CPA's models each client shows. A **Model Picker** page in CPA's console has one section each for Codex, Antigravity (agy), and Claude Code, with an on/off switch per model. You set each client up once; after that you change its list on the page and never edit a model list by hand.

| Client | How it gets its list | Models without a switch |
| --- | --- | --- |
| Codex | Its `model_catalog_url` points at the plugin's catalog URL | Shown |
| Antigravity (agy CLI and `agy acp`, including T3) | `models.listSource` points at the plugin's agy URL | Left out |
| Claude Code | The plugin rewrites the model list CPA returns to Claude Code | Shown |

The plugin ID stays `codex-catalog-filter`, so existing Codex URLs, switches, and saved data keep working after an upgrade from 0.1.1.

Verified in CPA **v8.0.73** on **Linux amd64**, with Codex CLI 0.170, agy 1.5.12, and Claude Code 2.1.327 as the client versions the behaviour was checked against. See the [verification record](docs/verification.md) for what was and was not tested.

## Install

1. Keep your plugins directory persistent. In the standard Docker image, that is the existing volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to your existing `plugins.store-sources` list if it is not there yet, then install **Model Picker** from CPA's Plugin Store.
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

Set up only the clients you use. Each list on the page fills in after that client's first fetch through CPA.

## Codex

In Codex's `config.toml`, add `model_catalog_url` to the provider that already uses CPA, and remove any `model_catalog_json` line:

```toml
[model_providers.cliproxyapi]
base_url = "http://your-cpa-host:8317/v1"
model_catalog_url = "http://your-cpa-host:8317/v0/resource/plugins/codex-catalog-filter/models"
```

The URL needs no key of its own: Codex sends its usual CPA key with the request. Start Codex once to fill the Codex list. Codex picks up changes within five minutes; delete `~/.codex/models_cache.json` to see them at once.

## Antigravity (agy)

As root, add this to `/etc/antigravity/admin_settings.json`. If the file already exists, add the `models` key to it rather than replacing it:

```json
{"models":{"listSource":"http://127.0.0.1:8317/v0/resource/plugins/codex-catalog-filter/agy/models"}}
```

- The scheme, host, and port must match agy's gateway URL exactly. agy sends its CPA key only to that origin, so `localhost` against a `127.0.0.1` gateway gets no key and fails.
- The file applies to every agy profile on the machine: the CLI and each T3 `agy acp` instance.
- Remove `models.list`, `models.defaultModelId`, and `models.tiers` from `~/.gemini/config/config.json` and from any T3 profile's `config/config.json`. A default or tiers set there override the plugin's default and title model.

Run `agy models` once to fill the Antigravity list. agy fetches the list each time it starts; a running `agy acp` refreshes it at most every five minutes, so a change can take a little longer to appear there.

On the page, switch on the models agy should offer. Choose a **Default** model, and a **Title** model whose id contains `flash-lite` for thread titles; without them, agy gets the first model by id as its default and the first `flash-lite` model by id for titles. agy shows at most 32 models it has no built-in entry for, and the page counts them.

## Claude Code

Set `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` for Claude Code, for example in the `env` block of `~/.claude/settings.json`. Nothing else is needed: the plugin edits the list CPA returns to Claude Code, which is on by default.

Claude Code fetches its list at each start, so restart it to see a change. CPA lists non-Claude models to Claude Code under disguised ids (`claude-fable-5-dd-…`); the page shows and switches their real ids.

## Choose your models

Sign in to CPA's console with **Remember password**, then open **Model Picker** from the sidebar. In each client's section, switch models on or off, use the filter with **Enable shown** or **Disable shown** for groups, and select **Save**. Save writes only that client's settings.

The page saves into the plugin's configuration, so you can also edit it directly:

```yaml
codex-catalog-filter:
  enabled: true
  # Codex
  models:              # true = in Codex, false = not
    claude-fable-5-1: false
  new-models: enabled
  action: remove
  # Antigravity
  antigravity:
    models:
      gemini-4-argon-high: true
      gemini-3.1-flash-lite: true
    new-models: disabled
    default-model: gemini-4-argon-high
    title-model: gemini-3.1-flash-lite
  # Claude Code
  claude-code:
    filter: true
    models:            # real ids, not claude-fable-5-dd-…
      gpt-6-sol: false
    new-models: enabled
```

| Key | Default | Meaning |
| --- | --- | --- |
| `models` | none | Codex: per-model switches, slug to `true` or `false`. |
| `new-models` | `enabled` | Codex: what a model without a switch does. |
| `action` | `remove` | Codex: `remove` deletes switched-off models; `hide` keeps them selectable by exact name. |
| `antigravity` | see above | agy: `models`, `new-models` (default `disabled`), `default-model`, `title-model`. |
| `claude-code` | see above | Claude Code: `filter` (default `true`), `models`, `new-models` (default `enabled`). |
| `cpa-url` | `http://127.0.0.1:8317` | Where the plugin reads CPA's own model lists. Change it only if CPA listens elsewhere inside its container. |
| `data-dir` | `plugins/data/codex-catalog-filter` | Where the last model lists are kept for the page and for agy's fallback. |

Downgrading to 0.1.1 needs care: it rejects the `antigravity` and `claude-code` keys and stays inactive until you remove them. See the [reference](REFERENCE.md#upgrading-and-downgrading).

## More detail

- [Behavior, safety, caching, and compatibility reference](REFERENCE.md)
- [Verification record](docs/verification.md)
