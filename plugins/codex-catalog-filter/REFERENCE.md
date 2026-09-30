# Codex Catalog Filter reference

Maintained contract for **Codex Catalog Filter 0.1.0**, written 2026-09-30 against CPA **v8.0.4** (commit `d33f63f`) and Codex CLI **0.159**. Install steps are in the [README](README.md); test evidence is in [verification](docs/verification.md).

## How Codex gets its model list

Codex builds its model picker from a Codex-native catalog:

```json
{"models":[{"slug":"gpt-6-sol","visibility":"list", ...}, ...]}
```

It shows every entry whose `visibility` is `list`. `hide` entries stay usable by exact name but are not shown. Each entry is a complete Codex model description: `display_name`, reasoning levels, shell type, truncation policy, instructions, and more are required. A catalog therefore cannot be a bare list of names.

By default Codex fetches `{base_url}/models?client_version=<version>`. CPA answers that with every model it can route, including Claude, Grok, OpenRouter, and alias models. CPA has no separate Codex catalog URL and no setting to filter this one.

A provider's `model_catalog_url` replaces that address with any absolute URL. For that URL, Codex:

- appends `client_version`;
- sends the same provider credential it uses for inference, such as the output of an `auth.command`;
- rejects redirects;
- refuses a response larger than **1 MiB**;
- gives up after **5 seconds**.

This plugin provides that URL.

## Routes

The plugin declares one capability, `management_api`, for three routes:

| Route | Authentication | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/codex-catalog-filter/models` | None by CPA; the forwarded client key is checked | The filtered catalog, for Codex's `model_catalog_url` |
| `GET /v0/resource/plugins/codex-catalog-filter/settings` | None; fixed bytes with no data | The **Codex Models** sidebar page |
| `GET /v0/management/plugins/codex-catalog-filter/state` | Management key, checked by CPA before dispatch | The page's data: the last model list and the current switches |

The page saves switches through CPA's own `PATCH /v0/management/plugins/codex-catalog-filter/config`. The plugin itself never writes CPA's configuration.

## The catalog URL

For each request the plugin:

1. Reads CPA's own catalog from `{cpa-url}/v1/models?client_version=<same version>`. The only header it forwards is the caller's `Authorization`. It sends its own `User-Agent`, so CPA never routes the request to its Claude or Grok list formats.
2. Remembers that unfiltered list for the Codex Models page: slugs, display names, and CPA's visibility only.
3. Keeps each entry whose switch is on. An entry without a switch follows `new-models`. Every other entry is removed (`action: remove`, the default) or has its `visibility` set to `"hide"` (`action: hide`; the member is added when absent).
4. Returns the result as `application/json` with `Cache-Control: no-store`.

CPA has no host call that returns its model catalog, which is why step 1 goes over HTTP. `cpa-url` defaults to `http://127.0.0.1:8317`, CPA's own listener as seen from inside its process in the standard image.

The rewrite splices bytes rather than re-encoding JSON. Kept entries, the separators between them, and everything outside the `models` array are copied verbatim, including CPA's deliberate non-escaping of `<`, `>`, and `&` in instructions. Under `hide`, only the `visibility` value of a changed entry differs.

The plugin declares no interceptor, so CPA's own `/v1/models` responses, in every format, never pass through it.

## Keys and exposure

CPA does not authenticate plugin resource routes, so the catalog URL needs no key of its own and no configuration in Codex beyond `model_catalog_url`. The content still requires a CPA client key: CPA checks the forwarded `Authorization` exactly as it would if Codex asked it directly. A request with no key, or a wrong one, receives CPA's own `401` response.

Failed client keys do not count toward CPA's management-key IP ban (five failures, 30 minutes). That ban applies only to `/v0/management` and `/v8/management`. The catalog URL never touches those.

Anyone who can reach CPA can see that the routes exist. Only a valid client key returns a catalog, and only the management key returns the page's data.

## The Codex Models page

The page is fixed HTML, CSS, and JavaScript. It contains no model data, and its Content-Security-Policy pins the script and style by hash, allows requests only to the same origin, and allows framing only by the same origin.

It uses the console's remembered session, read the same way as Token Usage's sidebar. It needs **Remember password** on this exact origin and API base, and it never offers a login of its own.

It makes one request per action and never retries a rejected key. Any change to the console session in another tab clears the page and stops requests until you select **Refresh**.

It lists the models from the **last catalog Codex fetched** through the URL: the plugin can read CPA's catalog only with a client key, which the page does not have. The list is empty until Codex first fetches after install. The plugin keeps the list in `data-dir/catalog.json` so the page has it after a restart. It writes the file again when the list changes, or hourly otherwise, atomically with mode 0600. A failure to save is shown on the page and does not affect Codex.

The page shows for each model:

- its switch;
- its slug and display name;
- whether the switch is yours or the default;
- whether CPA itself hides it (image models, `codex-auto-review`). Those never appear in the picker, but stay available to Codex while on.

A counter shows how many are on and how many will actually appear in the picker. A warning appears when none would.

**Save** writes the full `models` map in one `PATCH`, then reads the state until the plugin reports it, about a second. Switches for models the latest list no longer contains are listed separately and kept, unless you select **Forget**.

## When something goes wrong

| Situation | Catalog URL response | What Codex does |
| --- | --- | --- |
| Nothing to filter: no switch is off and new models are enabled, or CPA's body is not a well-formed Codex catalog | CPA's catalog, unchanged | Shows it |
| Filtering would leave no entry with `visibility: list` | CPA's catalog, unchanged | Shows it |
| Missing or wrong key | CPA's `401` | Uses its cached catalog while fresh, otherwise its bundled one |
| CPA unreachable, a 5xx or redirect, no answer within 4 seconds, or more than 32 MiB | `502 {"error":"cpa_unavailable"}` | Same |
| Plugin not configured yet, or shutting down | `503` | Same |
| Configuration rejected, such as a switch set to `maybe` | The routes are gone: `404` | Same |

Codex's bundled catalog lists only OpenAI models, so a failure never exposes your other models.

The plugin never emits a catalog without a listed model. Codex treats such a catalog as non-authoritative and merges it into its bundled list, so emitting one would be worse than not filtering.

Configuration changes apply on CPA's next configuration reload, without a restart. If CPA rejects the configuration, it logs `plugin.reconfigure failed` and deactivates the plugin. Fixing the value brings the routes back, again without a restart.

## Configuration

`plugins.configs.codex-catalog-filter` accepts `models`, `new-models`, `action`, `cpa-url`, and `data-dir`, alongside the host-owned `enabled`, `priority`, and `store` keys. Unknown keys are rejected, so a misspelled setting cannot silently change what Codex sees.

- **`models`** maps a model slug to `true` (in Codex) or `false` (not in Codex). YAML's `on`/`off` and `yes`/`no` also work. Slugs are matched exactly and case-sensitively. At most 4,096 switches, each slug at most 256 bytes without surrounding spaces.
- **`new-models`** decides every slug without a switch. `enabled` (default) shows it, so models CPA adds later appear until you switch them off. `disabled` leaves them out until you switch them on; under `disabled`, remember to switch on `codex-auto-review` if you use `approvals_reviewer = "auto_review"`.
- **`action`** is what happens to a switched-off model. `remove` (default) deletes it: if you still select it explicitly (`codex -m claude-...`), Codex uses its generic fallback metadata. `hide` keeps it with `visibility: hide`, so an explicit selection keeps CPA's real metadata.
- **`cpa-url`** must be an `http` or `https` origin with no path, query, or credentials.
- **`data-dir`** is an optional clean absolute path. It defaults to `plugins/data/codex-catalog-filter` beneath CPA's working directory, inside the standard plugins volume.

## Codex caching and its bundled catalog

Codex keeps the catalog in `~/.codex/models_cache.json` with a five-minute TTL. It stores the response's `ETag` (neither CPA nor this plugin sends one) and never makes a conditional request. Delete the cache file to see a change immediately. Remove `model_catalog_json` from Codex's configuration: while it is set, Codex reads that static file and never fetches.

Codex treats a fetched catalog as authoritative only when it lists at least one model and either Codex is signed in with a ChatGPT account or the provider uses an `env_key` or bearer-token API key. Otherwise, as with command-based provider auth and no ChatGPT sign-in, Codex merges the fetched catalog into the one bundled with the binary. Two consequences follow:

- `codex debug models` can show bundled entries, such as `gpt-daybreak-blue-latest` (hidden) or a new GPT model CPA does not serve yet. Codex 0.159.1 bundles `gpt-6.1-sol`, for example.
- A model removed here whose slug Codex also bundles comes back from the bundled copy, still listed. Switching off `gpt-5.5` with `action: remove` leaves Codex's own `gpt-5.5` in the picker. With `action: hide`, the hidden entry replaces the bundled one of the same slug, so it disappears. If you switch off an OpenAI model and it still appears, use `hide`.

## Limits

- Codex rejects a `model_catalog_url` response over 1 MiB. In CPA's catalog, GPT entries are about 40 to 90 KiB each, mostly instructions; other entries are a few KiB. Nine GPT entries plus the hidden image models measured about 580 KB in testing, so roughly 15 GPT models fit. The plugin does not trim to fit; switch models off if Codex reports a catalog error.
- Every catalog request costs one extra local `/v1/models` request in CPA's own log.
- CPA serves no plugin resource routes in Home mode.
- The switches apply to everyone who uses the URL; per-key catalogs are out of scope.

## Compatibility and verification

| Item | Value |
| --- | --- |
| Native contract | ABI **1**, RPC schema **6**, `cliproxy_plugin_init`, Go `c-shared` |
| Capability | `management_api` for the routes above; no interceptor or host callbacks |
| Verified CPA | **v8.0.4**, commit `d33f63f`, image `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d`, with a v8-layout configuration (`config-version: 8`). The suite's v7.2.155 image loads it alongside the other plugins, but serving the catalog there is untested. |
| Verified Codex | CLI **0.159.1** (`codex debug models` through `model_catalog_url`); source read at 0.159.0 |
| Platform | **Linux amd64 only**, CGO enabled, Go 1.27.1; module language floor Go 1.26.0 |

The plugin does not read CPA's own configuration, so moving client keys from the legacy root `api-keys` list to v8's `access.api-keys` does not affect it. It depends on four things:

- plugin ABI 1 / schema 6;
- unauthenticated plugin resource routes;
- management-authenticated plugin routes and `PATCH /v0/management/plugins/:id/config`;
- CPA's Codex catalog at `/v1/models?client_version=`.

Before moving to a later CPA release, update the pinned image and run `make smoke`, which checks all four.

Sources in CPA v8.0.4:

- `internal/api/server_management.go` (`pluginManagementNoRoute`, `pluginResourceNoRoute`, the plugin config routes);
- `internal/pluginhost/management.go` (`ServeResourceHTTP`, route and menu registration);
- `internal/api/handlers/management/plugins.go` (`PatchPluginConfig`);
- `internal/api/server_routes.go` (`unifiedModelsHandler`) and `sdk/api/handlers/openai/openai_handlers.go` (`OpenAIModels`).

In Codex 0.159.0:

- `codex-rs/model-provider-info/src/lib.rs` (`model_catalog_url`);
- `codex-rs/model-provider/src/models_endpoint.rs` (catalog URL, credential, 1 MiB limit, redirect rejection, 5-second timeout);
- `codex-rs/models-manager/src/manager.rs` and `cache.rs`;
- `codex-rs/protocol/src/openai_models.rs` (`ModelInfo`).

## Development

```sh
cd plugins/codex-catalog-filter
make ci      # gofmt, vet, unit and race tests, Linux amd64 shared library
make smoke   # the library in pinned official CPA v8.0.4; requires Docker
```

`make smoke` needs the CPA image above pulled first; it never pulls. It uses synthetic static models and keys only.

Release with the root workflow and a `codex-catalog-filter/vX.Y.Z` tag; see [releases](../../docs/releases.md). Its gate runs `make ci smoke`, then loads the candidate with the published peers in the suite's pinned v7.2.155 image to check coexistence.
