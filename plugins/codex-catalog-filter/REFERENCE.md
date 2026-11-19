# Model Picker reference

Maintained contract for **Model Picker 0.2.0**, plugin ID `codex-catalog-filter`, written 2026-11-19. It was checked at runtime against CPA **v8.0.73** (commit `43806c8`) and from source at CPA **v8.0.77**, with Codex CLI **0.170**, Antigravity CLI (agy) **1.5.12**, and Claude Code **2.1.327** as the client versions. Install steps are in the [README](README.md); test evidence is in [verification](docs/verification.md).

Model Picker was called Codex Catalog Filter up to 0.1.1. Only the human-facing name changed: the plugin ID, routes, Codex configuration keys, and data directory are the same.

## Clients at a glance

| Client | Mechanism | Setup, once | Unswitched models | Failure behaviour |
| --- | --- | --- | --- | --- |
| Codex | Filtered copy of CPA's Codex catalog at the plugin's catalog URL | `model_catalog_url` in Codex's `config.toml` | Shown (`new-models: enabled`) | Codex keeps its cached catalog, except on a 401/403 |
| Antigravity (agy CLI, `agy acp`, T3) | An agy-format list built from CPA's Gemini-format list at the plugin's agy URL | `models.listSource` in `/etc/antigravity/admin_settings.json` | Left out (`antigravity.new-models: disabled`) | The plugin serves the last good list to keys CPA accepted |
| Claude Code | The plugin's response interceptor rewrites the Claude-format list CPA returns to Claude Code | `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` | Shown (`claude-code.new-models: enabled`) | CPA's unfiltered list is served (fail open) |

The switches apply to everyone who uses CPA with that client; per-key lists are out of scope. A model left out of a list stays callable by exact id: the plugin changes what clients are offered, not what CPA routes.

## Routes and capabilities

| Route | Authentication | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/codex-catalog-filter/models` | None by CPA; the forwarded client key is checked by CPA | Codex's filtered catalog |
| `GET /v0/resource/plugins/codex-catalog-filter/agy/models` | None by CPA; the forwarded client key is checked by CPA | agy's model list |
| `GET /v0/resource/plugins/codex-catalog-filter/settings` | None; fixed bytes with no data | The **Model Picker** sidebar page |
| `GET /v0/management/plugins/codex-catalog-filter/state` | Management key, checked by CPA before dispatch | The page's data: each client's last list, switches, and status |

All four answer exact `GET` paths only; a trailing slash, another method, or a longer path gets `404`.

The plugin always declares `management_api` for these routes. It declares `response_interceptor` as well only while `claude-code.filter` is true, which is the default. CPA rebuilds a plugin's capabilities from every `plugin.reconfigure` answer (CPA `internal/pluginhost/host.go`, about lines 1364 to 1415, and `rpc_client.go` about line 233), so switching the filter off removes the interceptor, and its cost, at the next configuration reload without a restart.

The page saves through CPA's own `PATCH /v0/management/plugins/codex-catalog-filter/config`. The plugin never writes CPA's configuration.

## Codex

### How Codex 0.170 gets its model list

Codex builds its picker from a Codex-native catalog, `{"models":[{"slug":...,"visibility":"list",...},...]}`. It shows each entry whose `visibility` is `list`; `hide` entries stay usable by exact name. Each entry is a complete Codex model description, so a catalog cannot be a bare list of names.

Without `model_catalog_url`, Codex fetches `{base_url}/models?client_version=<version>`, and CPA answers with every model it can route. With `model_catalog_url`, Codex 0.170:

- requests that URL with `client_version` appended and the same provider credential it uses for inference;
- uses that catalog **exclusively**: it is not merged with the catalog bundled in the binary (`models-manager/src/manager.rs`, lines 351 to 358 and 989);
- applies **no response-size limit** (`model-provider/src/models_endpoint.rs`, line 249);
- gives up after 5 seconds and rejects redirects (`models_endpoint.rs`, lines 49 and 94 to 100);
- caches the result in `~/.codex/models_cache.json` for **300 seconds** (`manager.rs`, line 42);
- **clears its catalog and the cache on a 401 or 403**, which empties the picker (`manager.rs`, lines 849 to 880). On any other failure it keeps its last catalog; with no cache, the picker is empty.

Codex 0.159 and earlier differ: they refused a response over 1 MiB and could merge the fetched catalog into the bundled one. CPA's catalog has grown past 1 MiB even after filtering (1,112,621 bytes measured on a real installation in November 2026), so use Codex 0.170 or later with this URL.

### The catalog URL

For each request the plugin:

1. Reads CPA's own catalog from `{cpa-url}/v1/models?client_version=<same version>`. The only header it forwards is the caller's `Authorization`. It sends its own `User-Agent`, `codex-catalog-filter/0.2.0`, so CPA never answers in its Claude or Grok formats.
2. Remembers that unfiltered list for the page: slugs, display names, and CPA's visibility only.
3. Keeps each entry whose switch is on. An entry without a switch follows `new-models`. Every other entry is removed (`action: remove`, the default) or has its `visibility` set to `"hide"` (`action: hide`; the member is added when absent).
4. Returns the result as `application/json` with `Cache-Control: no-store`.

The rewrite splices bytes rather than re-encoding JSON. Kept entries, the separators between them, and everything outside the `models` array are copied verbatim, including CPA's deliberate non-escaping of `<`, `>`, and `&` in instructions. Under `hide`, only the `visibility` value of a changed entry differs.

The plugin never emits a catalog without a listed model: CPA's catalog is served unchanged instead. For Codex 0.170 an empty catalog would mean an empty picker.

## Antigravity (agy)

### How agy 1.5.12 gets its model list

agy resolves its model list in tiers. Tier 1 is an inline `models.list` in its settings. Tier 2 is `models.listSource`, a URL. Tier 3 is gateway discovery, `GET {gateway}/v1beta/models?pageSize=1000`, which on CPA returns every model. This plugin is a Tier 2 source.

- `models.listSource` is a plain string. Writing it as an object makes agy reject the whole settings file. No environment variable or command-line flag sets it; it is read from `<gemini dir>/config/config.json` and `/etc/antigravity/admin_settings.json`. The admin file applies to every profile, and in testing its `listSource` won over a user-level `models.list`. A user-level `models.defaultModelId` overrides the list's default, and a user-level `models.tiers` overrides the title model, so remove both.
- agy sends `Authorization: Bearer <gateway key>` and its own `User-Agent` (`antigravity/cli/<version> (...)`) to the URL only when its scheme, host, and port equal the gateway URL's. Otherwise it sends no key and CPA answers 401.
- It accepts a bare array or `{"defaultModelId": ..., "list": [...]}`. The plugin sends the object. Entries honour `modelId`, `displayName`, `maxOutputTokens`, `maxTokens`, `supportsThinking`, and `thinkingBudget`; a `tiers` object is ignored. `defaultModelId` becomes the current model and moves to the top.
- It waits 15 seconds and reads at most 10 MiB.
- **It keeps no last good list.** A non-200 answer, a timeout, an unparseable body, or an empty list makes it fall back to Tier 3. There, agy keeps a different subset of about 42 of CPA's models on each run, with a different default each time, which can be an image model or one that fails locally. A running `agy acp` keeps that list until its next refresh, at least 300 seconds later. The plugin is built so that this fallback does not happen while CPA is merely unavailable.
- agy derives its tiers from list order: the first id containing `flash-lite` titles threads, the first other id containing `flash` runs flash subagents, and the first other id is the pro tier.
- agy sends an id from its built-in alias table under a base name: `gemini-4-argon-high` goes out as `gemini-4-argon` with its own thinking level.

### The agy URL

For each request the plugin:

1. Reads `{cpa-url}/v1beta/models` with only the caller's `Authorization` forwarded, its own `User-Agent`, `Accept: application/json`, no query, and a 4-second timeout.
2. Remembers the list for the page and for the fallback below: id, display name, description, token limits, and output modalities only. A 200 carrying a valid Gemini model list also records that CPA accepted the caller's key.
3. Selects, orders, and shapes the list as below, and returns `{"defaultModelId":"<id>","list":[...]}` as `application/json; charset=utf-8` with `Cache-Control: no-store`. HTML characters in labels are not escaped.

**Selection.** A model is in the list when its explicit switch is on. A model without a switch is in when it is the `default-model` or `title-model`; otherwise a non-text model is out, and any other model follows `antigravity.new-models` (default `disabled`). If nothing is selected, or every selected model is an image model agy hides, the plugin serves CPA's `gemini-*` text models instead, then any text model, so agy never receives a list it shows nothing of. If CPA answers `200` with a list that has no text model at all (for example while it is still registering models after a restart), that list is not kept: a key CPA accepted gets the last good list, as when CPA is unavailable, and any other request gets `502`.

**Non-text models.** CPA advertises `generateContent` for every model, so methods cannot identify one. A model is non-text when CPA lists output modalities without `text`, or when a token of its id (split on anything but letters and digits) is `tts`, `voice`, `image`, `imagine`, `imagen`, `dall`, `video`, `veo`, `embedding`, `embed`, `audio`, `whisper`, or `moderation`, or starts with one of those of four letters or more. So `gemini-3.1-flash-image`, which answers text and images, is non-text. Non-text models are listed only when switched on explicitly. agy itself drops image models (`IsGatewayImageModel`), so an image model never becomes the default.

**Order.** The list is sorted by id (byte order, as Go compares strings). The default model then comes first and is also `defaultModelId`; without one, the first model by id that agy shows is the default, and a `default-model` that is not in the list, or is an image model agy hides, gives a warning on the page. The title model, when its id contains `flash-lite`, moves ahead of every other id containing `flash-lite`; any other title model only produces a warning on the page, because agy would not title with it.

The plugin sorts because CPA's own order is not stable. CPA builds its Gemini-format list by iterating a Go map and caches the result (`buildAvailableModelsLocked` in `internal/registry/model_registry.go`), so its order changes whenever CPA rebuilds that cache: on every configuration reload, including each Save on the page, and on restart. In a test on v8.0.73 the order differed after each of three reloads, and with CPA's order agy's default, title model, and the models beyond the 32 slots changed with it. Sorted, they change only when the switches or CPA's set of models change, and the page's preview matches what agy receives after a Save. With nothing switched on, the `gemini-*` fallback is sorted too: for the list captured from a CPA v8.0.73 installation, its default is `gemini-3-flash` and its title model `gemini-3.1-flash-lite`. Set a default and a title model to choose them yourself.

**The 32-slot limit.** agy 1.5.12 gives each id it has no built-in metadata for one of 32 placeholder enums, `MODEL_PLACEHOLDER_M334` to `M365`: slot `crc32.ChecksumIEEE(id) & 31`, with linear probing. Ids beyond 32 are dropped ("custom gateway model(s) exceeded the maximum placeholder limit (32)"). A built-in id needs no slot unless an earlier entry already took its enum; image models are not offered and take none. Whether an id is built-in depends on agy finding metadata for it, not only on its id table: `gemini-3.5-flash-lite` is in the table but still takes a slot. The plugin emits at most 32 slot-needing ids, in list order (so the last ones by id are left out), and reports the rest on the page as skipped. Its copies of agy's tables (the 36-key id-to-enum map, the 25-key alias map, and the 86-entry metadata table) were extracted from the agy 1.5.12 binary and are hard-coded in `internal/agy/builtin.go`; a later agy may change them.

**Labels.** Every entry gets a `displayName` that differs from its id. agy 1.5.12's ACP treats a missing `displayName`, or one equal to the `modelId` after trimming spaces, as no label, and lists an id it has no label of its own for under an enum-style `MODEL_PLACEHOLDER_Mnnn` id; T3 stores that as the model's slug, and the number shifts whenever the list changes. (A label that differs only in case is kept as a label.) The label is CPA's display name when it has a real one. CPA gives an alias without a display name its own id as the display name, so then, and when there is none, the label is the id in words: separators become spaces and each word starts with a capital, `mp-echo` becoming `Mp Echo`. The effort of an id ending in `-low`, `-medium`, `-high`, or `-xhigh` is appended once when the name lacks it: `Gemini 4 Argon (High)`, and `Gemini 4 Neon (XHigh)` for an alias named `gemini-4-neon-xhigh`. An id that reads the same in words, such as `GPT4`, gets ` (model)` appended. A duplicate label gets the id appended.

**Token limits.** agy refuses to run a model whose output limit is too close to its context window (`failed to construct executor: config.MaxTokenLimit ... cannot exceed`). CPA advertises output equal to input for `gemini-4-*` and several Grok models. So:

- For an id agy has built-in metadata for (such as `gemini-4-argon`, `gemini-4-hydrogen`, `gemini-3.8-flash`, `gemini-3.1-flash-lite`, and their effort variants), neither `maxTokens` nor `maxOutputTokens` is sent, and agy uses its own limits.
- Otherwise `maxTokens` is CPA's input limit, and `maxOutputTokens` is CPA's output limit, lowered to 65,536 (or half the input, when that is smaller) when it is more than half the input. With no input limit, an output limit above 65,536 is lowered to 65,536. At 500,000 input, 499,999 output still fails in agy, which is why the margin is half the input rather than "below the input".
- When CPA gives no limits, none are sent, and agy uses 65,535 output tokens.

**Thinking.** For an id agy sends under a base name but has no metadata for (in agy 1.5.12, the `gemini-4-neon` effort variants), agy would send a default thinking budget of 1,024, which CPA reads as the `low` level, so the effort in the id is lost. The plugin sets `supportsThinking: true` and the `thinkingBudget` of the suffix's level from CPA's own level-to-budget table (`internal/thinking/convert.go`): low 1,024, medium 8,192, high 24,576, xhigh 32,768. With that, agy 1.5.12 sends `gemini-4-neon-xhigh` as `gemini-4-neon` with `thinkingBudget: 32768`. From CPA's source, CPA turns a budget back into the same level for models that take only levels (`ConvertBudgetToLevel`: above 24,576 is `xhigh`) and passes it on as a budget otherwise. A test through the live CPA succeeded with that request; which effort the upstream applied was not observed. Built-in effort ids keep agy's own thinking level.

Every other id is sent without thinking fields, and agy 1.5.12 then picks the thinking itself. Against a mock gateway it sent `thinkingBudget: 1024` with `includeThoughts: true` for any `gemini-*` id it has no metadata for (plain `gemini-4-neon`, and a made-up `gemini-5-eta` even with `supportsThinking: true` but no budget), which CPA reads as `low`; and `thinkingBudget: 0` with `includeThoughts: false` for other ids such as `grok-*`, which CPA reads as thinking off. agy's `--effort` flag overrides this per session (`--effort high` sent `thinkingLevel: HIGH`). Sending `supportsThinking: true` with `thinkingBudget: -1` made agy send `-1`, which CPA reads as `auto`; the plugin does not do that, because CPA passes `auto` to a user-defined OpenAI-compatible model as `reasoning_effort: "auto"` (`internal/thinking/provider/openai/apply.go`), which such upstreams may reject, and because it would turn thinking on for every non-Gemini model agy is offered.

### Keys and the last good list

CPA does not authenticate plugin resource routes, and CPA's port is often reachable from other machines. The agy URL therefore never serves cached data on its own authority:

- A request without `Authorization`, or with a key CPA rejects, gets CPA's own `401` (or `403`) while CPA is reachable. A `401` or `403` also removes that key from the accepted set.
- When CPA cannot answer (unreachable, a 5xx, a redirect, no answer within 4 seconds, more than 32 MiB, or a 200 that is not a Gemini model list), the plugin rebuilds the list from the last one CPA returned, with the current switches, **only** for a request whose `Authorization` CPA accepted with a valid list within the last 24 hours. Every other request gets `502 {"error":"cpa_unavailable"}`.
- Accepted keys are kept as SHA-256 hashes with a fixed, public domain-separation prefix (`sha256("codex-catalog-filter/agy-key/v1\0" + Authorization)`), not a secret salt, at most 64, pruned after 24 hours. A raw key is never stored or logged.
- After CPA's `plugin.shutdown` call and until CPA unloads the library, an accepted key still gets the list from memory rather than a `503`.

So agy keeps its curated list through a CPA restart or outage, as long as it fetched successfully in the last 24 hours.

## Claude Code

### How Claude Code 2.1.327 gets its model list

With `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` set and a non-Anthropic `ANTHROPIC_BASE_URL`, Claude Code sends `GET {ANTHROPIC_BASE_URL}/v1/models?limit=1000` once at each start, with `User-Agent: claude-code/<version>`, `anthropic-version: 2023-06-01`, and its key. It waits 3 seconds (`CLAUDE_CODE_GATEWAY_MODEL_DISCOVERY_TIMEOUT_MS`), keeps the ids that contain `claude` or `anthropic`, and caches the result in `~/.claude/cache/gateway-models.json`. An empty list is ignored and the previous one kept. There is no separate discovery URL, so the list can only be changed where CPA answers it.

CPA answers that request in its Claude format because of the `anthropic-version` header (or a `claude-cli/` agent). It lists every model it routes and disguises each id that does not start with `claude-` as `claude-fable-5-dd-` plus the id reversed, so Claude Code keeps it; CPA reverses that when it routes a request (`internal/client/claude/models/models.go`). Static `modelPicker` or `availableModels` settings in Claude Code would need editing for every change, which this plugin avoids. T3's Claude picker also adds T3's own Claude catalog and its configured custom models, which the plugin cannot hide.

### The rewrite

The response interceptor acts only on a response that is all of these: source format `claude`, no model or requested model, not streamed, no request body, status 200, a `User-Agent` starting with `claude-code/` or `claude-cli/` (and not the plugin's own), and a body of at most 4 MiB and 5,000 entries shaped `{"data":[{"id":...},...],"first_id":...,"last_id":...,"has_more":...}`. That is CPA's model-list response, which carries no path or query to match on (`WriteModelListResponse` in CPA's `sdk/api/handlers/handlers_interceptors.go`).

For such a list it matches each entry by its real, de-cloaked id: an explicit switch decides, and anything else follows `claude-code.new-models` (default `enabled`). It keeps the entries' bytes as CPA sent them, recomputes `first_id` and `last_id`, and keeps `has_more`. When nothing would be removed, or nothing would remain, it changes nothing. It records the de-cloaked ids, display names, and the time for the page.

Everything else gets an empty result, which keeps CPA's response. That includes every inference response, every other list format, and any error, panic, or oversized call: the interceptor fails open and never returns an error, so CPA logs none.

Side effect: CPA picks `GET /v1/models/{id}` details from the intercepted list, so for a Claude Code agent in Claude format, that route returns `404` for a switched-off model.

### Costs

While the interceptor is declared, CPA sends every successful non-streaming response through it, not only model lists: inference responses and every model list in every format, including CPA's Codex catalog (about 1.4 MB) on each Codex fetch. The body travels base64-encoded inside the RPC.

- The plugin reads the request in place from CPA's buffer and rejects anything that is not a Claude Code list from its leading fields, without decoding the body.
- A call larger than 160 MiB, the default limit Chat Archive also uses, is passed through unchanged without being read; other native calls stay capped at 1 MiB.
- CPA applies no timeout of its own to an interceptor call; the client's request context bounds it.
- Interceptors run by priority, highest first, then by plugin ID. Another interceptor, such as Chat Archive's, sees the list before or after this rewrite depending on its priority.
- The plugin's own loopback fetches to CPA pass through the interceptors too. They carry its own `User-Agent` and are never rewritten. Calls are admitted through a gate that never blocks, so a nested interceptor call during shutdown passes the response through at once instead of waiting.

Set `claude-code.filter: false` to remove the interceptor and its cost if you do not use Claude Code through CPA.

## Keys and exposure

CPA does not authenticate plugin resource routes, so the catalog URLs need no key of their own. The content still requires a CPA client key: CPA checks the forwarded `Authorization` exactly as it would for the client directly, and a request without a valid key gets CPA's `401`. The agy URL's last good list goes only to a key CPA accepted recently, as described above.

Failed client keys do not count toward CPA's management-key IP ban (five failures, 30 minutes); that ban applies only to `/v0/management` and `/v8/management`, which the catalog URLs never touch. The plugin itself never calls CPA's management API.

Anyone who can reach CPA can see that the routes exist. Only a valid client key returns a list, and only the management key returns the page's data.

## The Model Picker page

The page is fixed HTML, CSS, and JavaScript with no model data. Its Content-Security-Policy pins the script and style by hash, allows requests only to the same origin, and allows framing only by the same origin.

It has one collapsible section per client: Codex, Antigravity, and Claude Code. Each lists the models that client was last offered, with a switch per model, a text filter with **Enable shown** and **Disable shown**, the setting for models without a switch, the time of the client's last fetch, its one-time setup line, and **Discard**. A note on each row says whether the model has a switch of its own ("No switch: on" follows the setting for models without a switch). Switches for models the latest list no longer contains are listed separately and kept. The Codex section adds the setting for switched-off models (`action`). The Antigravity section lists models by id, as agy receives them, and adds the **Default** and **Title** model selectors, a live count of placeholder slots out of 32, the order agy receives, and notes on non-text models and adjusted token limits. With an empty filter, its **Enable shown** leaves models that do not answer in text as they are; filter for one to switch it on. The Claude Code section adds the filter switch. Under 640 pixels wide, each table row stacks its id, name, and notes under the switch.

Before a client has fetched, its section uses the list another client last saw, as CPA lists the same models in every format; the state route says which (`models_source`).

**Save** sends one `PATCH` for that client only: `models`, `new-models`, and `action` for Codex, the whole `antigravity` value, or the whole `claude-code` value. CPA's `PATCH` replaces whole top-level keys, so saving one client never touches another's. Right before saving, the page reads the state again and applies your changes to that client's current settings, so a stale tab does not undo newer switches. When the plugin rejected a client's value, that section's **Save** is available even without a change: it replaces the rejected value with the settings the page shows, which are the ones in effect.

The page uses the console's remembered session, with a reader adapted from Token Usage's sidebar. It needs **Remember password** on this exact origin and API base, and it never offers a login of its own. The session is the Management Center's `cli-proxy-auth` entry in the browser's local storage. Management Center v1.27.0 and later store it as `enc::v2::`, keyed by the page's host; older consoles stored `enc::v1::`, keyed by the host and the browser's user agent, and the page still reads that form. Both are reversible obfuscation, not encryption. An entry with another `enc::` version gives no session, and so does one written under another host. While a `cli-proxy-auth` entry exists, the page never falls back to the older separate storage entries.

It makes one request per action and never retries a rejected key. Any change to the console session in another tab clears the page and stops requests until you select **Refresh**.

## The state route

`GET /v0/management/plugins/codex-catalog-filter/state` returns JSON. Its first seven members are the Codex client, unchanged from 0.1.1: `catalog_path`, `seen_at`, `saved`, `new_models`, `action`, `models`, and `switches`. It adds `version` and one section per other client.

Both client sections carry `config_key`, `configured`, `error`, `using` (`config`, `last-good`, or `defaults`), `new_models`, `switches`, `models_source`, and `saved`.

- `antigravity` adds `path`, `default_model`, `title_model`, `fetched_at`, `served_at`, `served_from` (`cpa` or `last-good`), `last_error`, and `last_error_at`. Its `models` list gives each model's label, limits, effective state, non-text and image flags, built-in status, token handling, and its place and slot in the served list. Its `preview` describes the list the URL serves now: entries, default and title ids, slots used, skipped ids, and warnings.
- `claude_code` adds `filter`, `seen_at`, `lists`, `rewritten`, `intercept_errors`, and `models` with real and cloaked ids.

## When something goes wrong

| Situation | Codex URL | agy URL | Claude Code list |
| --- | --- | --- | --- |
| Nothing to filter | CPA's catalog, unchanged | A built list, never empty | Unchanged |
| Filtering would leave nothing | CPA's catalog, unchanged | CPA's Gemini text models (also when only image models are switched on) | Unchanged |
| Missing or wrong key | CPA's `401`; Codex 0.170 empties its picker | CPA's `401`; agy falls back to Tier 3 | Not applicable |
| CPA unreachable, a 5xx or redirect, no answer within 4 s, or over 32 MiB | `502 {"error":"cpa_unavailable"}`; Codex keeps its cache | The last good list for an accepted key; otherwise `502` | Not applicable |
| CPA lists no text model | Not applicable | The last good list for an accepted key; otherwise `502` | Not applicable |
| An invalid `antigravity` or `claude-code` value | Unaffected | Last valid settings, or defaults | Last valid settings, or defaults |
| An invalid top-level value, such as a Codex switch set to `maybe`, or an unknown top-level key | All routes are gone: `404` | `404`; agy falls back to Tier 3 | Unchanged |
| Plugin not configured yet | `503` | `503` | Unchanged |
| Plugin shutting down | `503` | The last good list for an accepted key; otherwise `503` | Unchanged |

An invalid client value is reported on the page and in the state route's `error`, and that client keeps the last valid settings this process applied, or its defaults after a restart. It never fails `plugin.reconfigure`, so the other clients keep working.

An invalid top-level value does fail it: CPA logs `plugin.reconfigure failed` and deactivates the plugin until the value is fixed, which then brings the routes back without a restart.

Configuration changes apply on CPA's next configuration reload, about a second after a Save.

## Configuration

`plugins.configs.codex-catalog-filter` accepts `models`, `new-models`, `action`, `antigravity`, `claude-code`, `cpa-url`, and `data-dir`, alongside the host-owned `enabled`, `priority`, and `store` keys. Unknown top-level keys are rejected, so a misspelled setting cannot silently change what Codex sees.

Codex:

- **`models`** maps a model slug to `true` (in Codex) or `false` (not in Codex). YAML's `on`/`off` and `yes`/`no` also work. Slugs are matched exactly and case-sensitively. At most 4,096 switches, each slug at most 256 bytes without surrounding spaces.
- **`new-models`** decides every slug without a switch. `enabled` (default) shows it, so models CPA adds later appear until you switch them off. `disabled` leaves them out until you switch them on; under `disabled`, remember to switch on `codex-auto-review` if you use `approvals_reviewer = "auto_review"`.
- **`action`** is what happens to a switched-off model. `remove` (default) deletes it: if you still select it explicitly (`codex -m claude-...`), Codex uses its generic fallback metadata. `hide` keeps it with `visibility: hide`, so an explicit selection keeps CPA's real metadata.

Antigravity, under **`antigravity`**:

- **`models`**: model id to `true` or `false`.
- **`new-models`**: `enabled` or `disabled` (default).
- **`default-model`**: optional; listed first and sent as `defaultModelId`. It is in the list unless switched off.
- **`title-model`**: optional; an id containing `flash-lite`, moved ahead of other such ids. It is in the list unless switched off.

Claude Code, under **`claude-code`**:

- **`filter`**: `true` (default) declares the interceptor and rewrites Claude Code's list; `false` removes both.
- **`models`**: real model id (not the `claude-fable-5-dd-…` form) to `true` or `false`.
- **`new-models`**: `enabled` (default) or `disabled`.

Both client values are validated strictly but on their own: unknown or duplicate keys (the error names the key), the limits above, and values of the wrong YAML type are rejected for that client only. `filter` and each switch must be the YAML booleans `true` or `false`: unlike the Codex `models` map, the words `yes`, `no`, `on`, and `off` are rejected, because CPA writes a JSON string such as `"no"` sent to its `PATCH` as a plain `no`. `new-models`, `default-model`, `title-model`, and the model ids must be strings, so `default-model: 5` is rejected and `default-model: "5"` is not. An absent key, or `null`, means that client's defaults, and a `null` field means that field's default; the page sends `null` to reset a client.

Shared:

- **`cpa-url`** must be an `http` or `https` origin with no path, query, or credentials. It defaults to `http://127.0.0.1:8317`, CPA's own listener as seen from inside its process in the standard image.
- **`data-dir`** is an optional clean absolute path. It defaults to `plugins/data/codex-catalog-filter` beneath CPA's working directory, inside the standard plugins volume.

CPA's `PATCH /v0/management/plugins/codex-catalog-filter/config` replaces each top-level key it receives as a whole and deletes a key sent as `null`. Send a client's complete value each time.

## Saved lists

The plugin keeps two files in `data-dir`, each written atomically with mode 0600 when its content changes, or hourly otherwise. A failure to save is reported on the page and does not affect any client.

- `catalog.json`: the Codex catalog as Codex last fetched it (slugs, display names, and CPA's visibility only), unchanged from 0.1.1.
- `clients.json`: the last Gemini-format list the agy URL received (ids, names, descriptions, token limits, output modalities), the last Claude-format list Claude Code received (real ids, cloaked ids, names), and the hashes and times of accepted agy keys.

Neither file holds a key, a request, or model instructions. An unreadable or invalid file is ignored. The page uses them after a restart, before any client fetches again, and the agy URL uses `clients.json` for its last good list.

## Upgrading and downgrading

**From 0.1.1:** nothing to change. The Codex switches, URL, and `catalog.json` carry over. Claude Code filtering is on by default but changes nothing until you switch a model off; the interceptor's cost starts at once, so set `claude-code.filter: false` if you do not use Claude Code through CPA.

**Back to 0.1.1:** 0.1.1 rejects the `antigravity` and `claude-code` keys as unknown. CPA then deactivates it, so the Codex URL returns `404` until those keys are removed, for example with a `PATCH` that sets each to `null`. 0.1.1 ignores `clients.json`, and agy falls back to Tier 3 while its URL is gone.

## Limits

- agy shows at most 32 models it has no built-in entry for, and the plugin's built-in tables are agy 1.5.12's.
- agy's list beyond the default and title model is ordered by id, not by anything CPA or you choose.
- Thinking for ids agy has no metadata for is agy's own choice (see Thinking above): `low` for `gemini-*` ids and off for others, unless agy's `--effort` sets it.
- Every catalog request costs one extra local request in CPA's own log: `/v1/models` for Codex, `/v1beta/models` for agy.
- CPA serves no plugin resource routes in Home mode.
- The switches apply to everyone who uses each client; per-key lists are out of scope.

## Compatibility and verification

| Item | Value |
| --- | --- |
| Native contract | ABI **1**, RPC schema **6**, `cliproxy_plugin_init`, Go `c-shared` |
| Capabilities | `management_api`; `response_interceptor` while `claude-code.filter` is true. CPA v8.0.4 to v8.0.77 accept `response_interceptor` from a schema-6 plugin (no schema check in `rpc_client.go`) |
| Claude Code rewrite | Needs CPA **v7.2.158** or later, which runs response interceptors on model lists (commit `5b278561`, `WriteModelListResponse`). Earlier releases never show the list to the plugin |
| Verified CPA at runtime | **v8.0.73**, commit `43806c8`, image `eceasy/cli-proxy-api@sha256:5727997d6407a7be54e58275d595ffeac0200eb388520b87bf2d2e1e042d9b5b`, with a v8-layout configuration |
| CPA v8.0.4 | Checked from source at the tag only for 0.2.0: model-list interception, capability rebuild on reconfigure, and the same resource-route rules. 0.1.1 was verified there at runtime |
| Clients | Codex CLI **0.170** (source and mock capture), agy **1.5.12** (a local mock gateway, and the CLI and `agy acp` against a throwaway CPA), Claude Code **2.1.327** (a mock capture, its documentation, and a run against a throwaway CPA) |
| Platform | **Linux amd64 only**, CGO enabled, Go 1.27; module language floor Go 1.26.0 |

The plugin does not read CPA's own configuration. It depends on:

- plugin ABI 1 / schema 6, unauthenticated plugin resource routes, management-authenticated plugin routes, and `PATCH /v0/management/plugins/:id/config`;
- CPA's Codex catalog at `/v1/models?client_version=` and its Gemini-format list at `/v1beta/models`;
- response interceptors on model-list responses, CPA's Claude-format list, and its id cloaking.

Before moving to a later CPA release, run the smoke test against it (see Development).

Sources in CPA v8.0.77 (and v8.0.73 where noted in verification):

- `internal/api/server_management.go` (`pluginManagementNoRoute`, `pluginResourceNoRoute`, the plugin config routes) and `internal/api/handlers/management/plugins.go` (`PatchPluginConfig`);
- `internal/pluginhost/management.go` (`ServeResourceHTTP`, route and menu registration), `host.go` and `rpc_client.go` (registration, capabilities), `adapters_interceptors.go` and `snapshot.go` (interceptor order and fail-open);
- `internal/api/server_routes.go` (`unifiedModelsHandler`, the Anthropic list detection), `sdk/api/handlers/gemini/gemini_handlers.go` (`GeminiModels`), `sdk/api/handlers/handlers_interceptors.go` (`WriteModelListResponse`), `sdk/pluginapi/types.go` (`ResponseInterceptRequest`);
- `internal/client/claude/models/models.go` (cloaking), `internal/registry/model_registry.go` (list order), `internal/thinking/convert.go` (level budgets).

In Codex rust-v0.170.0: `codex-rs/model-provider/src/models_endpoint.rs`, `codex-rs/model-provider/src/provider.rs`, and `codex-rs/models-manager/src/manager.rs`.

In agy 1.5.12 (binary, file SHA-256 prefix `94104f028e6962fa`): `fetchRemoteModelCatalog` and `shouldAttachGatewayHeaders` (Tier 2), `geminimodels` maps `map.init.0` and `map.init.1` and `LookupModelInfo` (aliases and built-ins), and `IsGatewayImageModel`.

## Development

```sh
cd plugins/codex-catalog-filter
make ci      # gofmt, vet, unit and race tests, Linux amd64 shared library
make smoke   # the library in pinned official CPA v8.0.4; requires Docker
```

`make smoke` needs the pinned CPA image pulled first; it never pulls. To run the same smoke test against another local image:

```sh
python3 scripts/smoke.py --library dist/codex-catalog-filter.so --version 0.2.0 --image eceasy/cli-proxy-api:latest
```

It uses synthetic static models and keys only. It checks the Codex URL, the agy URL's shape, labels, order, slot limit, token limits, and last good list, the Claude Code rewrite (skipped on CPA before v7.2.158), the page, and recovery after a restart. The agy token checks use Vertex models from CPA's model catalog; when the image's catalog lacks one (CPA v8.0.4's embedded catalog has no `gemini-4-argon` or `gemini-omni-1.2-flash`, which arrive only with CPA's startup refresh from `router-for-me/models`), the checks that need it are skipped with a message rather than failing.

Release with the root workflow and a `codex-catalog-filter/vX.Y.Z` tag; see [releases](../../docs/releases.md). Its gate runs `make ci smoke`, then loads the candidate with the published peers in the suite's pinned image to check coexistence.
