# Verification record: Model Picker

Model Picker was called Codex Catalog Filter up to 0.1.1; the plugin ID is still `codex-catalog-filter`. The 0.2.0 record comes first; the 0.1.0 record follows unchanged.

## Model Picker 0.2.0

Local results from 2026-11-19 on Linux amd64 with Go 1.27.4, before release. No real provider credentials or production configuration were used, and no production state was changed. Every CPA run used a throwaway container from the local `eceasy/cli-proxy-api:latest` image (v8.0.73), bound to `127.0.0.1` on a random port, with synthetic keys and static models, and was removed afterwards. The live CPA was only read through its client model-list routes, to capture the test fixtures; its management API was never called.

### Unit, race, fuzz, and static checks

`make ci` passed: gofmt, `go vet`, `go test`, `go test -race`, and the Linux amd64 `c-shared` build. The new tests cover:

| Area | Covered |
| --- | --- |
| agy tables | The built-in id, alias, and metadata tables copied from agy 1.5.12 (file SHA-256 prefix `94104f028e6962fa`) reproduce what agy was seen to do: placeholders M363, M335, M339, M362, and M356 for the ids they were observed on, and no slot for enums 1018, 1402, and 1405 |
| agy list | Parsing CPA's `/v1beta/models`; non-text detection by name and modality; labels never empty, never equal to the id (every fixture model with its display name empty, equal to the id, padded, or `models/<id>`), and carrying the effort once; token limits (built-in, passed, clamped, none); thinking budgets for aliased effort ids; the Gemini fallback, also when only image models are switched on; the list sorted by id, identical for shuffled and reversed inputs; default and title ordering; default and title included unless switched off; non-text models only when switched on; the 32-slot cap with skipped ids reported; built-ins that share an enum taking a slot; duplicate labels; an image model never the default, with a warning when it is the chosen default; never an empty list; no HTML escaping |
| Claude list | Cloaking round-trips as CPA's does; a captured CPA list summarises to real ids; the rewrite keeps entry bytes and recomputes `first_id` and `last_id`; non-lists, nothing removed, and nothing left pass through |
| Interceptor input | Only Claude Code's Claude-format lists are accepted; inference bodies are rejected without being read; list bodies are bounded |
| agy URL | Only the caller's key is forwarded; CPA's client errors pass through; the last good list goes only to a key CPA accepted within 24 hours, is lost when CPA rejects that key, and survives a restart without storing the key; a `200` whose list has no text model is not kept and the last good list is served instead, or `502` without one; during shutdown, accepted keys are answered from memory; only exact GETs are answered |
| Configuration | Client defaults; valid subtrees; a bad subtree disables only that client; inside a subtree, `yes`/`no`/`on`/`off`, quoted booleans, numbers as ids, aliases, and non-mapping `models` are rejected while real booleans, quoted numeric ids, and `null` fields are accepted; errors name the unknown or duplicate key, bounded to 64 bytes; unknown top-level keys are still rejected |
| Registration and state | The interceptor is declared only while `claude-code.filter` is true; the state route reports each client, including from a captured CPA list, with the Antigravity models sorted by id |
| Settings page | The page is titled Model Picker and its script names the Codex, agy, state, settings, and config paths |
| ABI | A Claude Code list is rewritten through the real plugin; intercept calls pass through on any failure; a nested intercept call during shutdown does not deadlock |
| Concurrency | The agy URL, interception, the state route, and reconfiguration run concurrently under the race detector |

`FuzzBuild` (agy list) and `FuzzRewrite` (Claude list) each ran for 20 seconds without a failure; their seeds run with every `go test`. After the review fixes below, `FuzzBuild` also checks that no label equals its id, that the list always has a model agy shows and a default that is not an image model, and that reversing CPA's order changes nothing; it ran another 20 seconds (about 1.9 million inputs) without a failure. The fixtures under `internal/agy/testdata` and `internal/claudelist/testdata` were captured from a real CPA's client routes and contain no key, email, or URL.

`scripts/tests/session.test.mjs` and `scripts/tests/clients.mjs` (35 Node.js tests, run by `go test` through `TestSettingsPageReadsConsoleSessions` when `node` is on the path) load the shipped `app.js` unchanged into a stub page built from `index.html`'s own element ids. Besides the console session formats (below), they cover the page's per-client logic and save flow: the live Antigravity preview equals the plugin's own for seven cases over CPA's real 89-model list (`agy-parity-fixture.mjs`, generated from the Go code, with the models in CPA's order so that the page must sort them as the plugin does); selection, slots, id order, default and title rules; pending changes; one-client `PATCH` bodies; the stale-tab merge; the bounded confirmation after a save; a rejected key stopping all requests; an unreadable client section; **Enable shown** leaving non-text agy models alone without a filter; **Save** replacing a rejected value with no change pending; and a success notice clearing on the next edit.

### Smoke test in CPA v8.0.73

`scripts/smoke.py --image eceasy/cli-proxy-api:latest` ran the built library in a container whose log reports `CLIProxyAPI Version: v8.0.73, Commit: 43806c8`. Its synthetic configuration has Codex, Vertex, Claude, and xAI API keys, two OpenAI-compatible groups (one with 40 aliases, `mp-slot-01` to `mp-slot-40`), and two client keys. Switches were saved through CPA's own `PATCH /v0/management/plugins/codex-catalog-filter/config`, as the page does.

| Step | Result |
| --- | --- |
| Load | Registered and enabled as **Model Picker** 0.2.0, with one sidebar entry, `Model Picker` at `/v0/resource/plugins/codex-catalog-filter/settings` |
| Codex | With no switches, CPA's 90-entry catalog byte for byte; with five models switched off, 85 entries (1,049,748 bytes), byte-identical to CPA's catalog without them; `hide` and `new-models: disabled` as in 0.1.0; no key or a wrong key gets CPA's `401` |
| Other callers | CPA's own `/v1/models`, with and without `client_version`, still lists every model while the interceptor is declared |
| agy defaults | Nothing switched on, so CPA's 17 Gemini text models are served; no key or a wrong key gets CPA's `401` |
| agy switches | Default first and `defaultModelId`, the title model ahead of the other `flash-lite` id, by id otherwise. Tokens: none sent for the built-ins `gemini-4-argon` (CPA advertises 1,048,576 in and out), `gemini-2.5-pro`, and `gemini-2.5-flash-lite`; `gemini-omni-1.2-flash` (131,072 in, 260,640 out) clamped to 65,536; `gemini-3.5-flash-lite` passed through; none for models without limits. `gemini-4-neon-xhigh`, an alias CPA names after itself, labelled `Gemini 4 Neon (XHigh)` with `supportsThinking` and `thinkingBudget: 32768`. No label equals its id. A switched-on video model stays listed and is flagged non-text |
| agy slots | With 40 slot-needing ids switched on, the first 32 by id are served and the other 8 reported as skipped |
| agy new models enabled | 45 text models served and no image, video, or speech model; 28 beyond the slots reported |
| agy bad subtree | A non-boolean switch is reported in the state route's `error` (`antigravity: models: the switch for "gemini-4-argon" must be true or false`) with `using: last-good`; the plugin stays enabled and both catalog URLs keep serving |
| Claude Code | With defaults, CPA's 90-entry list (89 ids cloaked) passes unchanged and the state route lists the real ids. With three models switched off, 87 entries with `first_id` and `last_id` recomputed, both for `claude-code/` and for a `claude-cli/` agent without `anthropic-version`; another agent's Claude-format list and Claude Code's OpenAI-format list keep every model. With `new-models: disabled`, only the switched-on model remains, in its cloaked form. With `filter: false`, the list passes unchanged |
| Page | `200 text/html` with the locked-down CSP and no model data |
| Invalid Codex switch | Every route returns `404` until it is fixed, then serves again without a restart |
| CPA away | With `cpa-url` pointed at a closed port, the key that fetched before gets the last good list; another valid key, a wrong key, and no key get `502`. `clients.json` has mode 0600 and no raw key |
| Restart, CPA still away | The state route lists the saved lists before any fetch; the accepted key gets the list from disk and no key gets `502`. With `cpa-url` restored, both catalog URLs serve with the saved switches |

The first full run stopped only at the page title, because the page was still being renamed at the time; a copy of the smoke test with only that assertion relaxed passed every step. After the page landed, the unmodified smoke test passed every step against a library built by `make ci` with the renamed page. After the review fixes below, it passed every step again (the table above shows that run), and so did a scratch copy whose catalog fixture list named three models CPA does not list, as CPA v8.0.4's embedded catalog would lack them: it printed the skip note, used `mp-slot-01` as the default, waited 30 seconds for the catalog, and passed.

The same tests showed that CPA's Gemini-format list order changes on every configuration reload: in a separate throwaway container, the order differed after each of three `PATCH` requests, with the same set of models, while repeated reads between reloads matched. CPA builds that list from a Go map (`buildAvailableModelsLocked` in `internal/registry/model_registry.go`). The plugin therefore sorts agy's list by id; see [REFERENCE](../REFERENCE.md#the-agy-url).

### Review checks before the fixes

Three reviewers tested the build before the fixes below, all against throwaway CPA v8.0.73 containers bound to `127.0.0.1` with synthetic keys, removed afterwards; the live CPA's management API was never called. Their results, apart from the findings fixed below, still apply:

- **Integration:** `make ci`; Codex regression with `client_version` 0.170.0 and 0.159.0 (byte-exact, `hide`, `401`s, unknown top-level key); a 1,050,254-byte Codex catalog through the interceptor unchanged; agy shape, defaults, keys, tokens, thinking, slots (48 entries, 32 slots, real `agy models` logging Tier 2 only); 8 bad `antigravity` and 4 bad `claude-code` values; CPA unreachable, `500`, slow, garbage, redirect, and `401` upstreams with last-good only for accepted keys; key revocation; 13,379 agy and 213 Codex requests during 16 reconfigurations, all `200`; a container restart with CPA away; `agy acp` with T3's launch arguments (default, list, and order in `session/new`, Tier 2); five tiny prompts through the live CPA with the key only in the child's environment (every model answered, the clamp needed for `gemini-4-neon-xhigh`, which failed locally unclamped); Claude Code 2.1.327 with a temporary `HOME` (its cache held exactly the filtered list with the filter on, the full list with it off); non-list responses up to 24 MiB intact.
- **Page:** Chromium through agent-browser in the isolated Management Center at 1440 by 900 and 390 by 844: the sidebar entry and frame under the hash-pinned CSP with no errors, collapsible sections remembered, filters and bulk switches, one-client saves (`GET` state, `PATCH`, one confirming `GET`), the stale-tab merge for all three clients, Default and Title selectors, the live slot counter and flags matching the served route, bad values shown per section, the filter toggle, no management request without a session, exactly one `401` and then nothing, and no horizontal page scroll at 390 pixels.
- **Code:** `go vet` and race tests in a copy; CPA's interceptor, gate, buffer, thinking, and model-list sources at v8.0.4, v8.0.73, and v8.0.77.

### Review fixes

The reviewers' findings were fixed and re-tested on 2026-11-19: `make ci`, the 35 Node.js tests, `FuzzBuild` for 20 seconds, and the smoke test above (plus its catalog-fixture variant) all passed.

| Finding | Change | Re-tested by |
| --- | --- | --- |
| agy's list followed CPA's order, which changes on every reload, so the default, title model, and slot cut-off moved after any Save | The list is sorted by id before the default and title rules; the state route lists agy's models by id; the page's preview, selectors, and table follow | `TestBuildIgnoresCPAsOrder`, `FuzzBuild`, the parity fixture, the smoke test's order checks |
| An entry whose label equals its id (an alias without a display name) appears in ACP and T3 under a shifting `MODEL_PLACEHOLDER` id | Such labels become the id in words (`mp-echo` to `Mp Echo`), with the effort once; a label never equals its id | `TestLabelNeverEqualsTheID`, `FuzzBuild`, the smoke test's shape check. `agy acp` was not rerun: the reviewer's A/B run showed labels that differ from the id, even only in case, keep the real id |
| Only image models switched on gave agy a list it shows nothing of | Treated as nothing switched on: the Gemini fallback | `TestOnlyImageModelsSwitchedOnServesTheFallback`, parity fixture |
| A `200` without text models replaced the last good list and returned `502` | That list is not kept; the key is still recorded as accepted and the last good list is served | `TestAgyListWithoutTextModelsKeepsTheLastGoodList` |
| Client subtrees accepted `yes`/`no` and numbers as ids; an unknown key was not named | Values are checked by YAML type; errors name the key | `TestParseBadClientSubtreeDisablesOnlyThatClient`, `TestParseClientSubtreeErrorsNameTheKey` |
| The smoke test needed CPA's network catalog refresh for its agy fixtures | Catalog fixtures are waited for briefly and their checks skipped with a note when absent | The smoke variant above |
| Page: **Save** unavailable for a rejected value; ambiguous "Default: on" chips; **Enable shown** switching on non-text models; Refresh under the console's toolbar; phone tables hiding notes; a stale success notice | **Save** available while the value is rejected; "No switch: on/off" chips; **Enable shown** skips non-text agy models unless filtered; Refresh beside the status pills on the left; rows stack under 640 pixels; edits clear the notice | Node.js tests; Chromium through agent-browser against a stand-in server for the page and a state route built from the real CPA list: no horizontal scroll at 390 pixels with id, name, and notes visible in each row, and Refresh hit-tested inside a frame with a 200-pixel overlay at its top right |

### agy thinking for ids without metadata

agy 1.5.12 ran against a mock gateway (temporary `HOME` and `TMPDIR`, `AGY_CLI_DISABLE_AUTO_UPDATE=true`, proxies pointed at a closed port, a static `listSource`), and `agy -p` sent one prompt per model. For an id it has no metadata for and no thinking fields: `gemini-4-neon` got `thinkingBudget: 1024` with `includeThoughts: true` (CPA's `low`), and `grok-x` got `thinkingBudget: 0` with `includeThoughts: false` (thinking off). `supportsThinking: true` without a budget still gave 1,024 for a `gemini-*` id; with `thinkingBudget: -1`, agy sent `-1` for both a `gemini-*` and a `grok-*` id; `--effort high` sent `thinkingLevel: HIGH`. The plugin does not send `-1`, for the reasons in [REFERENCE](../REFERENCE.md#the-agy-url); what the upstream applies for these requests was not checked.

### Integration checks during development

A separate throwaway v8.0.73 container ran 33 checks while the backend was written, all passing: loading and metadata; the agy list's shape, default, and labels; CPA's `401` for no key or a wrong key; the Claude Code list filtered while other Anthropic-format and OpenAI-format lists were not; the Codex URL byte-identical to CPA's catalog; the state route's sections; a bad `antigravity` value reported while the Codex and agy URLs stayed up; a `PATCH`ed subtree applied without a restart; `filter: false` and deleting a key; the last good list only for an accepted key with `cpa-url` at a closed port; `clients.json` at mode 0600 without raw keys; and the last good list surviving a container restart.

### agy 1.5.12

agy 1.5.12 ran against a local mock gateway with a temporary `HOME`, `AGY_CLI_DISABLE_AUTO_UPDATE=true`, and `HTTP(S)_PROXY` pointed at a closed port, so nothing left the machine.

- `agy models` listed the plugin's exact list bytes through Tier 2 (`models.listSource`).
- `agy -p` on `gemini-4-neon-xhigh` sent `gemini-4-neon` with `thinkingBudget: 32768` and `maxOutputTokens: 65536`; without the plugin's thinking fields it sent `thinkingBudget: 1024`.
- `grok-4.7` with `maxOutputTokens` clamped to 65,536 ran without the `MaxTokenLimit` executor error.
- `gemini-4-argon-high` used agy's own `thinkingLevel: HIGH`.

An earlier investigation established, with the same isolation, the agy behaviour REFERENCE relies on: the accepted list shapes and fields, the same-origin rule for the key, the 15-second and 10 MiB limits, the fallback to a random part of CPA's list on any failure, the `MODEL_PLACEHOLDER` numbering and the 32-slot cap, the token-limit failures, and that `/etc/antigravity/admin_settings.json` accepts `models.listSource` and beats a user-level `models.list` (tested as root in a private mount namespace; the real `/etc` was not touched).

### CPA versions

- **v8.0.73:** at runtime, above. The binary copied from a new, never-started container of the same image contains `response_interceptor`, `response.intercept_after`, `WriteModelListResponse`, `claude-fable-5-dd-`, `normalizeResourceRoute`, and `InterceptResponseExcept`.
- **Model-list interception:** introduced by CPA commit `5b278561` ("expose model list responses to plugin interceptors"), first released in v7.2.158; v7.2.157 does not contain it. The smoke test skips its Claude Code steps on earlier images.
- **v8.0.4:** from source at the tag, it accepts schema 6, maps `response_interceptor` without a schema check, rebuilds capabilities on reconfigure, applies the same resource-route rules, and runs interceptors on its Claude and Gemini lists. At runtime, the smoke test passed every step in the pinned image (`eceasy/cli-proxy-api@sha256:72205ea2…`): the Codex URL, the agy URL, the Claude Code rewrite, `filter: false`, a bad client value, and the last good list with CPA away. That image's catalog lists `gemini-3.5-flash-lite` and `gemini-omni-1.2-flash` without token limits, so the clamped token case cannot occur there and is skipped with a note. The first run failed only because the smoke test expected the clamped case whenever its fixture was listed; it now expects a token case only when the image's own limits produce it, and every served entry is still checked against the token rule. The same build passed every step, including the clamped case, against v8.0.73.

### Codex CLI 0.170 and Claude Code 2.1.327

The Codex URL's behaviour is unchanged from 0.1.1. The Codex 0.170 statements in REFERENCE come from its source at `rust-v0.170.0` and a mock capture with a temporary `CODEX_HOME`. Claude Code 2.1.327's discovery request (URL, headers, `claude-code/` agent, 3-second timeout, `claude`/`anthropic` filter, cache file, empty list ignored) comes from a mock capture with a temporary configuration directory and Anthropic's gateway documentation.

### Not verified for 0.2.0

- Coexistence with the other plugins in the suite's image (the release workflow checks it).
- The page inside the real Management Center after the review fixes (the stand-in server above served the same bytes and CSP), and `agy acp` with the new labels.
- Codex CLI against the plugin inside a real CPA (its route is unchanged from 0.1.1).
- Which thinking effort the live upstream applies to the budgets agy sends.
- `/etc/antigravity/admin_settings.json` on a real system, and agy versions other than 1.5.12.
- Hosted CI, and platforms other than Linux amd64.

## Codex Catalog Filter 0.1.0

Local results from 2026-09-30 on Linux amd64 with Go 1.27.1 and GCC 15.2.0, before the first release. Hosted CI reruns the same `make ci` and `make smoke` gates on each pull request and release. No real provider credentials or production configuration were used, and no production state was changed.

### Unit, race, and static checks

`make ci` passed: gofmt, `go vet`, `go test`, `go test -race`, and the Linux amd64 `c-shared` build. `node --check` accepts the page script.

| Area | Covered |
| --- | --- |
| Catalog rewrite | Remove and hide produce byte-exact expected output; kept entries, separators, other top-level members, and pretty-printed formatting are preserved; a missing `visibility` is added; order is preserved |
| Switches | A switch overrides `new-models` both ways; `new-models: disabled` keeps only models switched on; switches for models not in the catalog are harmless; no switch off with new models enabled is idle |
| Fail-open | Malformed or truncated JSON, trailing data, a second document, non-object bodies, empty or non-array `models`, entries without a string slug, duplicate members, nothing to change, and nothing left listed all return CPA's body unchanged |
| Summary | The page's list keeps catalog order, display names, and CPA's visibility; non-catalog bodies are refused |
| Configuration | Defaults; `models` map with `true`/`false` (YAML `on`/`off` too); `new-models`; `action`; `cpa-url` accepts only an http(s) origin; `data-dir` accepts only a clean absolute path; host `enabled`, `priority`, and `store` keys tolerated; unknown and retired keys (`include`), non-boolean switches, bad slugs, too many switches, extra documents, and oversized documents rejected |
| Catalog URL | Filters CPA's catalog read with the caller's key: path, `client_version` (always sent), and `Authorization` forwarded, and nothing else. CPA's 401 is passed through. An unreachable, 5xx, redirecting, slow, or oversized CPA gives `502` |
| Routes | Registration advertises only `management_api`: one menu-less private `GET /state` route, the menu-less catalog resource, and the `Codex Models` page resource. Only exact GETs are answered; everything else is `404` without contacting CPA; before configuration and after shutdown, `503` |
| State | Empty before Codex's first fetch; afterwards, every model in order with its effective state and whether it was switched; all saved switches; a rejected fetch is not remembered |
| Saved list | Written with mode 0600 and only slugs, names, and visibility; reloaded after a restart without contacting CPA; rewritten on change or hourly, not on every fetch; a corrupt file is ignored; an unwritable directory still serves and reports `saved: false` |
| Page | Fixed bytes whatever the request; hash-pinned script and style in the CSP, `connect-src 'self'`, `frame-ancestors 'self'` |
| ABI | Envelope shapes; a full native round trip through the real plugin; errors and panics never echo payloads; shutdown drains an admitted call |

Targeted mutations were each caught:

- key forwarding, header forwarding, `client_version`, and route matching;
- filtering, error passthrough, and the size limit;
- switch precedence and the `new-models` setting;
- the state's `switched` flag;
- remembering, reloading, and the hourly rewrite limit of the saved list.

`FuzzRewrite` checks every rewrite against an independent decode: valid JSON, other top-level members unchanged, exactly the enabled entries in order (others hidden under `hide`), and at least one listed entry. It ran about 5.2 million inputs in 45 seconds with no failure, after about 9.9 million in 60 seconds on the earlier rules. Its seeds run with every `go test`.

### Official CPA v8.0.4 image

`make smoke` passed three consecutive times. It runs the built library in `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d`, whose log reports `CLIProxyAPI Version: v8.0.4, Commit: d33f63f`, with a v8-layout configuration.

- The library is installed under the Plugin Store's name and layout, `plugins/linux/amd64/codex-catalog-filter-v0.1.0.so`.
- Models come from synthetic static Codex, Claude, xAI, and OpenAI-compatible API-key groups.
- Switches are saved through CPA's own `PATCH /v0/management/plugins/codex-catalog-filter/config`, exactly as the page does.

| Step | Result |
| --- | --- |
| Load | CPA loads and registers the plugin, enabled, with one sidebar entry: `Codex Models` at `/v0/resource/plugins/codex-catalog-filter/settings` |
| Before Codex fetches | The state route reports no list, no switches, `new-models: enabled`, `action: remove` |
| No switches | The catalog URL serves CPA's 18-entry catalog (590,828 bytes) byte for byte; the state route then lists all 18, all on and unswitched |
| Four non-GPT models switched off | 14 entries (580,660 bytes), byte-identical to CPA's catalog without them. Listed: the eight GPT models; `codex-auto-review` and the image models kept as `hide`. The state route marks the four as switched off |
| CPA's own lists | `GET /v1/models`, with and without `client_version`, still list every model |
| `action: hide` | All 18 entries kept, same listed set |
| `new-models: disabled`, two switched on | Only `gpt-6-sol` listed and `codex-auto-review` hidden |
| No key, six times, then a wrong key | Each gets CPA's `401`; the management API still answers afterwards |
| Other paths, `POST`, the state path as a resource | `404` |
| Page | `200 text/html` with the locked-down CSP and no model data |
| A non-boolean switch | CPA deactivates the plugin and the URL returns `404`; fixing it brings it back without a restart |
| Saved list | `plugins/data/codex-catalog-filter/catalog.json` exists and holds no instructions |
| Restart | Before any new Codex fetch, the state route lists the saved models; the switches apply from startup |

Each comparison reads CPA's own catalog in the same attempt as the plugin URL, because CPA can refresh model metadata in the background after startup.

### The Codex Models page in a real browser

The page ran in Chromium through agent-browser, against a throwaway CPA v8.0.4 container with the plugin and `grok-4.7` switched off in config. One simulated Codex fetch through the catalog URL filled the list.

- **No console session:** the page showed the sign-in guidance, and CPA's log showed no request to the state route.
- **With a remembered console session** (the Management Center's `cli-proxy-auth` record, set in local storage):
  - All 18 models were listed with their switches, display names, and notes. `grok-4.7` showed "Set by you"; CPA-hidden models and `codex-auto-review` were marked.
  - The counter read "17 of 18 on · 11 in the picker", and the exact `model_catalog_url` line was shown.
- **Group switch:**
  - Filtering on "claude" matched `claude-fable-5-1` and `cpa-sonnet`, whose display name is `claude-sonnet-5-5`.
  - **Disable shown** and **Save** wrote both switches to `config.yaml` beside the existing `grok-4.7: false`.
  - The catalog URL immediately listed only the GPT models plus `or-kimi-k2`.
- **Single switch:** switching `or-kimi-k2` off and saving left exactly the eight GPT models listed. The whole session made two `PATCH` and three state requests, all `200`.
- **After a CPA restart:** the page still listed every model with no new Codex fetch.
- **Discard:** it restored a pending switch and disabled **Save**.
- **Stale switch:** a switch for a model not in the list appeared under "Switches for models Codex was not offered". **Forget** and **Save** removed it from `config.yaml`.
- **Always dark:** with the browser's colour-scheme preference set to light, the page still rendered on the dark background `#0c111d`.

Not covered by an automated browser test in CI. The console's obfuscated (`enc::v1::`) storage and legacy fallback use Token Usage's audited reader unchanged, but were not exercised here. Both console storage forms are now exercised outside a browser; see [Console session formats](#console-session-formats).

### Console session formats

Local results from 2026-11-04 on Linux amd64 with Go 1.27.3 and Node.js 26.13.0, for the change that reads the `enc::v2::` session format official Management Center v1.27.0 introduced (see [REFERENCE](../REFERENCE.md)). The page's reader decoded only `enc::v1::` before this change, so a session saved by v1.27.0 or later gave no session; the earlier `app.js` fails the `enc::v2::` cases below.

`scripts/tests/session.test.mjs` loads the shipped `app.js` unchanged into a stub page at CPA's resource route and checks whether its first state request carries the remembered key. `TestSettingsPageReadsConsoleSessions` runs it with `go test` when `node` is on the path; it skips without Node on a workstation and fails without it when `CI` is set. Its vectors are literals that upstream's own `src/utils/encryption.ts` produced: tag v1.27.2 (commit `f154fa9380825c4fe94bc577a9abaa7e505f8875`) for `enc::v2::`, and v1.26.7 for `enc::v1::`.

| Case | Result |
| --- | --- |
| `enc::v2::` session from v1.27.2 | Recovered, under any user agent |
| `enc::v2::` written under another host, or opened on another host | No session |
| `enc::v2::` with `rememberPassword: false`, with or without the key | No session |
| `enc::v1::` session from v1.26.7 | Recovered with the user agent it was written under, and not with another |
| An unknown prefix (`enc::v3::`) | No session |
| `enc::v2::` that decodes to invalid UTF-8 | No session (strict decoding) |
| An unreadable or remember-off `cli-proxy-auth` beside valid older entries | No session: the older entries are not revived |

Each rule was mutated in a scratch copy of `app.js` and its case failed: no `enc::v2::` branch (the page before this change), the user agent in the `enc::v2::` key or missing from the `enc::v1::` key, lenient UTF-8, no `rememberPassword` check, an accepted `enc::v3::`, and falling back to the older entries when `cli-proxy-auth` cannot be read. `make ci` passed.

Not verified: a live sign-in through Management Center v1.27 or later in a real browser, and the test's run on the hosted CI runner. The new form was checked against upstream's own encoder output only.

### Codex CLI

Codex CLI 0.159.1 ran against a throwaway CPA container with `model_catalog_url` set to the plugin URL. It used an isolated temporary `CODEX_HOME` with command-based provider auth, like a Keychain-backed setup.

- **`codex debug models`** listed only the models the configuration kept. The hidden entries included `codex-auto-review` and the bundled `gpt-daybreak-*`, which shows Codex merged the catalog into its bundled one.
- **CPA's log** showed Codex requesting the plugin URL with `client_version=0.159.1`, and the plugin requesting `/v1/models` from `127.0.0.1`.
- **With `model_catalog_url` at a missing path and no cache,** Codex got a `404` and showed its bundled, OpenAI-only catalog.

Separately, excluding a model Codex also bundles (`gpt-5.5`) under `remove` left Codex's bundled copy listed, while `hide` hid it. That comes from Codex's merge, not from the plugin.

### Suite coexistence in CPA v7.2.155

`python3 scripts/quota-cache-smoke.py --candidate codex-catalog-filter` and the full no-candidate suite, as `quota-cache` CI runs it, both passed. The plugin loaded and registered alongside the six other plugins, with status routes, persistence, restart, and both optional-cache modes checked. A run with another plugin as the candidate skips the unreleased plugin, as the release tooling now does for any peer without a catalog entry. `python3 -m unittest discover -s scripts/tests` (10 tests) and `python3 scripts/check-catalog.py` passed.

### Not verified here

- The production CPA instance and your real Codex configuration.
- Serving the catalog or the page on CPA releases other than v8.0.4.
- Platforms other than Linux amd64.
