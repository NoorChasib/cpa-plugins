# Codex Catalog Filter reference

Maintained contract for **Codex Catalog Filter 0.1.0**, written 2026-09-29 against CPA **v8.0.4** (commit `d33f63f`) and Codex CLI **0.159.0**. Install steps are in the [README](README.md); test evidence is in [verification](docs/verification.md).

## How Codex gets its model list from CPA

Codex builds its picker by calling `GET {base_url}/models?client_version=<version>` on the configured provider. CPA answers with a Codex-native catalog:

```json
{"models":[{"slug":"gpt-6-sol","visibility":"list", ...}, ...]}
```

Codex shows every entry whose `visibility` is `list`; `hide` entries stay usable by exact name but are not shown. CPA builds this catalog from every model it can route, so Claude, Grok, OpenRouter (`or-*`), and alias models appear in Codex too. CPA itself does not filter it, apart from a fixed list of image and video models it always hides.

From CPA v8.0.0 (upstream commit `5b278561`, "expose model list responses to plugin interceptors"), `BaseAPIHandler.WriteModelListResponse` passes each model-list body through every plugin's response interceptor before writing it. A non-empty returned body replaces the response. That hook is the only thing this plugin uses.

## What is rewritten

The plugin advertises one capability, `response_interceptor`. CPA also calls that hook for every successful non-streaming response, so each call is checked cheapest-first, and anything that fails a check is answered with an empty result (`{}`), which leaves CPA's response exactly as it was:

1. The plugin has rules with at least one `include` pattern.
2. `SourceFormat` is `openai`. The Claude and Gemini model lists arrive as `claude` and `gemini`.
3. `Model` and `RequestedModel` are empty and `Stream` is false. Model responses always carry a model.
4. `OriginalRequest` and `RequestBody` are absent. Model lists have no request body.
5. The status is 200, and the body starts with `{"models":` (after optional whitespace). Only the first 48 decoded bytes are read for this. OpenAI and Grok lists start with `object` or `data`.
6. The whole body parses as a JSON object with exactly one `models` array, every entry of which is an object with one non-empty string `slug`. Gemini's catalog also has a `models` array, but its entries have `name`, not `slug`.

Checks 2 to 4 run on the host's request buffer in place, before anything is copied into Go, and stop at the first disqualifying field. CPA encodes `SourceFormat` and `Model` ahead of every header and body, so an ordinary response is rejected after roughly its first hundred bytes, whatever its size. Only a candidate that passes them is copied and decoded, and even then only a short body prefix is decoded until check 6.

Only then are entries filtered. An entry is **kept** when its slug matches at least one `include` pattern and no `exclude` pattern. Every other entry is removed (`action: remove`, the default) or has its `visibility` set to `"hide"` (`action: hide`; the member is added when absent).

The rewrite splices bytes rather than re-encoding JSON. Kept entries, the separators between them, and everything outside the `models` array are copied verbatim. CPA's deliberate non-escaping of `<`, `>`, and `&` in instructions therefore survives. Under `hide`, only the `visibility` value of a changed entry differs.

CPA's Home mode builds its Codex catalog through the same writer, so it is filtered identically.

## Fail-open rules

The plugin never makes a response worse than CPA's original. It returns the original body unchanged when:

- the body is malformed, truncated, has trailing data, has duplicate `models`, `slug`, or `visibility` members, or any entry lacks a string `slug`;
- the `models` array is empty;
- filtering would change nothing;
- filtering would leave **no entry with `visibility: list`**. Codex does not treat a catalog without a listed model as authoritative and falls back to its bundled catalog, so emitting one would be worse than not filtering;
- the native request exceeds 64 MiB (checked before it is copied);
- the plugin is not yet configured, has no `include` patterns, or has been shut down.

The output is validated as JSON before it is returned. A panic inside a call is recovered and reported to CPA as a plugin error, which CPA logs and ignores for that response.

## Configuration

`plugins.configs.codex-catalog-filter` accepts `include`, `exclude`, and `action`, alongside the host-owned `enabled`, `priority`, and `store` keys. Unknown keys are rejected, so a misspelled `include` cannot silently disable filtering.

**Patterns** are shell-style globs matched against the whole slug, case-sensitively:

| Syntax | Matches |
| --- | --- |
| `*` | any run of characters, including `/` (a slug is not a path) |
| `?` | exactly one character |
| `[abc]`, `[a-z]` | one listed character or range |
| `[!a-z]`, `[^a-z]` | one character not listed |
| `\x` | the literal character `x` |

Each list holds at most 256 patterns of at most 256 bytes, without surrounding spaces. `gpt-[0-9]*` matches `gpt-6-sol`, `gpt-5.6-terra`, and `gpt-5.5`, but not `gpt-image-2` or `gpt-reserve`. Note that it also matches future GPT slugs CPA adds, such as `gpt-6.1-sol`, which is the point: new GPT models appear without a configuration change. Use `exclude` to drop specific ones.

**Actions:**

- `remove` (default) deletes other entries. The catalog Codex downloads is smaller. If you still select a removed model explicitly (`codex -m claude-...`), Codex uses its generic fallback metadata for it and logs a fallback warning.
- `hide` keeps every entry but hides the others. An explicitly selected model keeps CPA's real metadata (context window, reasoning levels, instructions).

**Changing the configuration** takes effect on CPA's next configuration reload; a restart is not needed. If CPA rejects the plugin's configuration (for example `action: drop`), CPA logs `plugin.reconfigure failed` and deactivates the plugin, so catalogs pass through unfiltered. Fixing the configuration re-registers it, again without a restart.

## Codex caching and ETags

Codex 0.159.0 stores the catalog in `~/.codex/models_cache.json` with a five-minute TTL, along with the `ETag` header of the `/models` response. It never sends a conditional request. It only compares that stored ETag with any `X-Models-Etag` header on later `/responses` traffic, to decide between extending the cache TTL and refetching.

CPA v8.0.4 sets no `ETag` or `Content-Length` on model lists; the Codex catalog is written chunked. There is therefore nothing to keep in sync. If a later CPA does derive either header from the unfiltered bytes, the plugin removes it whenever it rewrites the body, rather than pass on a validator for different bytes.

Two Codex settings bypass CPA entirely and must be removed for the filter to have any effect: `model_catalog_json` (a static catalog file; Codex then never fetches) and, on a provider, `model_catalog_url` (a different catalog source).

Codex treats a fetched catalog as authoritative only when it lists at least one model and Codex is signed in with a ChatGPT account or uses API-key discovery. Otherwise, as with command-based provider auth and no ChatGPT sign-in, Codex merges CPA's catalog into the catalog bundled with the binary. `codex debug models` can then also show bundled entries such as `gpt-daybreak-blue-latest`. In Codex 0.159.0 every bundled entry that is not in the target GPT set is `hide`, so the listed set is still what the filter keeps.

That merge has one consequence for `exclude`. A removed entry whose slug is also in Codex's bundled catalog comes back from the bundled copy, still listed; for example, excluding `gpt-5.5` with `action: remove` leaves Codex's own `gpt-5.5` in the picker. With `action: hide`, CPA's hidden entry replaces the bundled one of the same slug, so it disappears. Use `hide` if you exclude a model Codex ships with and Codex merges rather than replaces.

## Cost and limits

- Declaring a response interceptor means CPA serializes every successful non-streaming response, including its request bodies, and hands it to the plugin. That host-side cost is inherent to the capability. The plugin's own share is small: it screens each call in place and rejects an ordinary response in about a microsecond with under 1 KiB allocated, measured on an 89 MB request. Streaming responses and WebSocket traffic, which Codex uses for turns, never reach it.
- The rule applies to every client that requests the Codex catalog through CPA, not per client or per API key. CPA gives the interceptor no request URL or key identity, and upstream declined per-key model lists in core.
- A slug CPA serves under a routing prefix (`team/gpt-6-sol`) is matched as the whole string; `gpt-[0-9]*` does not match it, but `*gpt-[0-9]*` does.

## Compatibility and verification

| Item | Value |
| --- | --- |
| Native contract | ABI **1**, RPC schema **6**, `cliproxy_plugin_init`, Go `c-shared` |
| Capability | `response_interceptor` only; no host callbacks, routes, storage, or network access |
| CPA minimum | **v8.0.0**. Older releases (the suite pins v7.2.155) load it, but never send model lists through the hook. |
| Verified CPA | **v8.0.4**, commit `d33f63f`, image `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d` |
| Verified Codex | CLI **0.159.0** (`codex debug models` against the filtered catalog) |
| Platform | **Linux amd64 only**, CGO enabled, Go 1.27.1; module language floor Go 1.26.0 |

Sources in CPA v8.0.4: `sdk/api/handlers/handlers_interceptors.go` (`WriteModelListResponse`, `applyResponseInterceptors`), `internal/pluginhost/adapters_interceptors.go` (`InterceptResponseExcept`), `internal/api/server_routes.go` (`unifiedModelsHandler`), `internal/client/codex/models/models.go` (`BuildResponseForClient`, `MarshalCompact`), and `sdk/pluginapi/types.go` (`ResponseInterceptRequest`, which has no JSON tags, so fields travel in PascalCase with byte slices as base64). In Codex 0.159.0: `codex-rs/codex-api/src/endpoint/models.rs`, `codex-rs/models-manager/src/manager.rs`, and `codex-rs/models-manager/src/cache.rs`.

## Development

```sh
cd plugins/codex-catalog-filter
make ci      # gofmt, vet, unit and race tests, Linux amd64 shared library
make smoke   # the library in pinned official CPA v8.0.4; requires Docker
```

`make smoke` needs the CPA image above and `python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285` pulled first; it never pulls. It uses synthetic static models and keys only.

Release with the root workflow and a `codex-catalog-filter/vX.Y.Z` tag; see [releases](../../docs/releases.md). Its gate runs `make ci smoke`, then loads the candidate with the published peers in the suite's pinned v7.2.155 image to check coexistence.
