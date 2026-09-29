# Codex Catalog Filter reference

Maintained contract for **Codex Catalog Filter 0.1.0**, written 2026-09-29 against CPA **v8.0.4** (commit `d33f63f`) and Codex CLI **0.159**. Install steps are in the [README](README.md); test evidence is in [verification](docs/verification.md).

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

## The catalog URL

The plugin declares one capability, `management_api`, and uses it only to register one resource route with no sidebar menu:

```text
GET /v0/resource/plugins/codex-catalog-filter/models?client_version=<version>
```

For each request the plugin:

1. Reads CPA's own catalog from `{cpa-url}/v1/models?client_version=<same version>`. The only header it forwards is the caller's `Authorization`. It sends its own `User-Agent`, so CPA never routes the request to its Claude or Grok list formats.
2. Keeps entries whose slug matches at least one `include` pattern and no `exclude` pattern. Every other entry is removed (`action: remove`, the default) or has its `visibility` set to `"hide"` (`action: hide`; the member is added when absent).
3. Returns the result as `application/json` with `Cache-Control: no-store`.

CPA has no host call that returns its model catalog, which is why step 1 goes over HTTP. `cpa-url` defaults to `http://127.0.0.1:8317`, CPA's own listener as seen from inside its process in the standard image.

The rewrite splices bytes rather than re-encoding JSON. Kept entries, the separators between them, and everything outside the `models` array are copied verbatim, including CPA's deliberate non-escaping of `<`, `>`, and `&` in instructions. Under `hide`, only the `visibility` value of a changed entry differs.

The plugin declares no interceptor, so CPA's own `/v1/models` responses, in every format, never pass through it.

## Keys and exposure

CPA does not authenticate plugin resource routes, so the URL itself needs no key of its own and no configuration in Codex beyond `model_catalog_url`. The content still requires a CPA client key: CPA checks the forwarded `Authorization` exactly as it would if Codex asked it directly. A request with no key, or a wrong one, receives CPA's own `401` response.

Failed client keys do not count toward CPA's management-key IP ban (five failures, 30 minutes). That ban applies only to `/v0/management` and `/v8/management`, which this route never touches.

Anyone who can reach CPA can see that the route exists. Only a valid client key returns a catalog.

## When something goes wrong

| Situation | Response | What Codex does |
| --- | --- | --- |
| Nothing to filter: no `include`, every entry allowed, or CPA's body is not a well-formed Codex catalog | CPA's catalog, unchanged | Shows it |
| Filtering would leave no entry with `visibility: list` | CPA's catalog, unchanged | Shows it |
| Missing or wrong key | CPA's `401` | Uses its cached catalog while fresh, otherwise its bundled one |
| CPA unreachable, a 5xx or redirect, no answer within 4 seconds, or more than 32 MiB | `502 {"error":"cpa_unavailable"}` | Same |
| Plugin not configured yet, or shutting down | `503` | Same |
| Configuration rejected, such as `action: drop` | The route is gone: `404` | Same |

Codex's bundled catalog lists only OpenAI models, so a failure never exposes your other models.

The plugin never emits a catalog without a listed model. Codex treats such a catalog as non-authoritative and merges it into its bundled list, so emitting one would be worse than not filtering.

Configuration changes apply on CPA's next configuration reload, without a restart. If CPA rejects the configuration, it logs `plugin.reconfigure failed` and deactivates the plugin. Fixing the value brings the route back, again without a restart.

## Configuration

`plugins.configs.codex-catalog-filter` accepts `include`, `exclude`, `action`, and `cpa-url`, alongside the host-owned `enabled`, `priority`, and `store` keys. Unknown keys are rejected, so a misspelled `include` cannot silently disable filtering.

**Patterns** are shell-style globs matched against the whole slug, case-sensitively:

| Syntax | Matches |
| --- | --- |
| `*` | any run of characters, including `/` (a slug is not a path) |
| `?` | exactly one character |
| `[abc]`, `[a-z]` | one listed character or range |
| `[!a-z]`, `[^a-z]` | one character not listed |
| `\x` | the literal character `x` |

Each list holds at most 256 patterns of at most 256 bytes, without surrounding spaces. `gpt-[0-9]*` matches `gpt-6-sol`, `gpt-5.6-terra`, and `gpt-5.5`, but not `gpt-image-2` or `gpt-reserve`. It also matches GPT slugs CPA adds later, which is the point. List exact slugs instead if you want new models to wait for a configuration change.

**Actions:**

- `remove` (default) deletes other entries. If you still select a removed model explicitly (`codex -m claude-...`), Codex uses its generic fallback metadata for it and logs a fallback warning.
- `hide` keeps every entry but hides the others. An explicitly selected model keeps CPA's real metadata (context window, reasoning levels, instructions).

**`cpa-url`** must be an `http` or `https` origin with no path, query, or credentials, such as `http://127.0.0.1:8317`.

## Codex caching and its bundled catalog

Codex keeps the catalog in `~/.codex/models_cache.json` with a five-minute TTL. It stores the response's `ETag` (neither CPA nor this plugin sends one) and never makes a conditional request. Delete the cache file to force an immediate refetch after changing configuration. Remove `model_catalog_json` from Codex's configuration: while it is set, Codex reads that static file and never fetches.

Codex treats a fetched catalog as authoritative only when it lists at least one model and either Codex is signed in with a ChatGPT account or the provider uses an `env_key` or bearer-token API key. Otherwise, as with command-based provider auth and no ChatGPT sign-in, Codex merges the fetched catalog into the one bundled with the binary. Two consequences follow:

- `codex debug models` can show bundled entries, such as `gpt-daybreak-blue-latest` (hidden) or a new GPT model CPA does not serve yet. Codex 0.159.1 bundles `gpt-6.1-sol`, for example.
- An entry removed here whose slug Codex also bundles comes back from the bundled copy, still listed. Excluding `gpt-5.5` with `action: remove` leaves Codex's own `gpt-5.5` in the picker. With `action: hide`, the hidden entry replaces the bundled one of the same slug, so it disappears.

## Limits

- Codex rejects a `model_catalog_url` response over 1 MiB. CPA's full catalog entries are about 60 KiB each, mostly instructions, so a filtered catalog stays under the limit up to roughly 16 entries. Nine entries measured 568,006 bytes. The plugin does not trim to fit; narrow `include` if Codex reports a catalog error.
- Every catalog request costs one extra local `/v1/models` request in CPA's own log.
- CPA serves no plugin resource routes in Home mode.
- The filter applies to everyone who uses the URL; per-key catalogs are out of scope.

## Compatibility and verification

| Item | Value |
| --- | --- |
| Native contract | ABI **1**, RPC schema **6**, `cliproxy_plugin_init`, Go `c-shared` |
| Capability | `management_api`, for one menu-less resource route; no interceptor, host callbacks, or storage |
| Verified CPA | **v8.0.4**, commit `d33f63f`, image `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d`. The suite's v7.2.155 image loads it alongside the other plugins, but serving the catalog there is untested. |
| Verified Codex | CLI **0.159.1** (`codex debug models` through `model_catalog_url`); source read at 0.159.0 |
| Platform | **Linux amd64 only**, CGO enabled, Go 1.27.1; module language floor Go 1.26.0 |

Sources in CPA v8.0.4:

- `internal/api/server_management.go` (`pluginResourceNoRoute`: no authentication) and `internal/pluginhost/management.go` (`ServeResourceHTTP`, resource registration, menu-less routes);
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
