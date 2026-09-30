# Verification record: Codex Catalog Filter 0.1.0

Local results from 2026-09-30 on Linux amd64 with Go 1.27.1 and GCC 15.2.0, before the first release. Hosted CI reruns the same `make ci` and `make smoke` gates on each pull request and release. No real provider credentials or production configuration were used, and no production state was changed.

## Unit, race, and static checks

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

## Official CPA v8.0.4 image

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

## The Codex Models page in a real browser

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

Not covered by an automated browser test in CI. The console's obfuscated (`enc::v1::`) storage and legacy fallback use Token Usage's audited reader unchanged, but were not exercised here.

## Codex CLI

Codex CLI 0.159.1 ran against a throwaway CPA container with `model_catalog_url` set to the plugin URL. It used an isolated temporary `CODEX_HOME` with command-based provider auth, like a Keychain-backed setup.

- **`codex debug models`** listed only the models the configuration kept. The hidden entries included `codex-auto-review` and the bundled `gpt-daybreak-*`, which shows Codex merged the catalog into its bundled one.
- **CPA's log** showed Codex requesting the plugin URL with `client_version=0.159.1`, and the plugin requesting `/v1/models` from `127.0.0.1`.
- **With `model_catalog_url` at a missing path and no cache,** Codex got a `404` and showed its bundled, OpenAI-only catalog.

Separately, excluding a model Codex also bundles (`gpt-5.5`) under `remove` left Codex's bundled copy listed, while `hide` hid it. That comes from Codex's merge, not from the plugin.

## Suite coexistence in CPA v7.2.155

`python3 scripts/quota-cache-smoke.py --candidate codex-catalog-filter` and the full no-candidate suite, as `quota-cache` CI runs it, both passed. The plugin loaded and registered alongside the six other plugins, with status routes, persistence, restart, and both optional-cache modes checked. A run with another plugin as the candidate skips the unreleased plugin, as the release tooling now does for any peer without a catalog entry. `python3 -m unittest discover -s scripts/tests` (10 tests) and `python3 scripts/check-catalog.py` passed.

## Not verified here

- The production CPA instance and your real Codex configuration.
- Serving the catalog or the page on CPA releases other than v8.0.4.
- Platforms other than Linux amd64.
