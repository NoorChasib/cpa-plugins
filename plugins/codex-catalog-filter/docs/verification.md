# Verification record: Codex Catalog Filter 0.1.0

Local results from 2026-09-29 on Linux amd64 with Go 1.27.1 and GCC 15.2.0, before the first release. Hosted CI reruns the same `make ci` and `make smoke` gates on each pull request and release. No real provider credentials or production configuration were used, and no production state was changed.

## Unit, race, and static checks

`make ci` passed: gofmt, `go vet`, `go test`, `go test -race`, and the Linux amd64 `c-shared` build.

| Area | Covered |
| --- | --- |
| Catalog rewrite | Remove and hide produce byte-exact expected output; kept entries, separators, other top-level members, and pretty-printed formatting are preserved; a missing `visibility` is added; order is preserved |
| Fail-open | Malformed or truncated JSON, trailing data, a second document, non-object bodies, empty or non-array `models`, entries without a string slug, duplicate members, nothing to change, and nothing left listed all return CPA's body unchanged |
| Rules | Include/exclude/action combinations; exclude beats an exact include; `codex-auto-review` is kept by `codex-*` and dropped without it |
| Globs | `*` across `/` and newlines, `?`, classes, ranges, negation, literal `]` and `-`, escapes, Unicode; malformed patterns rejected |
| Configuration | Default `remove`; `hide`; idle without `include`; `cpa-url` defaults to `http://127.0.0.1:8317` and accepts only an http(s) origin; host `enabled`, `priority`, and `store` keys tolerated; unknown keys, bad actions, bad globs, bad patterns, extra documents, and oversized documents rejected |
| Catalog URL | Registration advertises only `management_api` and one menu-less `/models` resource. Serving filters CPA's catalog read with the caller's key: path, `client_version` (always sent), and `Authorization` forwarded, and nothing else, including `Anthropic-Version` and unrelated headers. CPA's 401 is passed through. An unreachable, 5xx, redirecting, slow, or oversized CPA gives `502`. Other paths and methods give `404` without contacting CPA; before configuration and after shutdown, `503`. Rejected reconfiguration keeps the previous rules; concurrent serving and reconfiguration pass under `-race` |
| ABI | Envelope shapes; a full native round trip through the real plugin; errors and panics never echo payloads; shutdown drains an admitted call |

Seven targeted mutations of the key forwarding, filtering, error passthrough, `client_version`, header forwarding, route matching, and size-limit logic were each caught.

`FuzzRewrite` checks every rewrite against an independent decode: valid JSON, other top-level members unchanged, exactly the allowed entries in order (others hidden under `hide`), and at least one listed entry. It ran about 9.9 million inputs over 60 seconds with no failure; `FuzzGlob` ran 20 seconds without a panic. Their seeds run with every `go test`.

## Official CPA v8.0.4 image

`make smoke` passed five consecutive times. It runs the built library in `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d`, whose log reports `CLIProxyAPI Version: v8.0.4, Commit: d33f63f`. The library is installed under the Plugin Store's name and layout, `plugins/linux/amd64/codex-catalog-filter-v0.1.0.so`. Models come from synthetic static Codex, Claude, xAI, and OpenAI-compatible API-key groups.

| Step | Result |
| --- | --- |
| Load | CPA loads and registers the plugin at version 0.1.0, enabled, with no sidebar entry |
| Idle (no `include`) | The plugin URL serves CPA's 18-entry catalog (590,828 bytes) byte for byte, as `application/json; charset=utf-8` |
| `include: [gpt-[0-9]*, codex-*]` (default `remove`) | 9 entries (568,006 bytes), byte-identical to CPA's catalog with non-matching entries removed. Listed: `gpt-6.1-sol`, `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`. `codex-auto-review` kept as `hide` |
| CPA's own lists | `GET /v1/models`, with and without `client_version`, still list every model |
| `action: hide` | All 18 entries kept, byte-identical except the changed `visibility` values |
| `exclude: [gpt-5.5]` | Removed, byte-exact |
| No key, six times, then a wrong key | Each gets CPA's `401`; the management API still answers afterwards |
| Other paths, `POST` | `404` |
| `action: drop` | CPA deactivates the plugin and the URL returns `404`; fixing the value brings it back without a restart |
| Restart | The persisted configuration serves the filtered catalog from startup |

Each comparison reads CPA's own catalog in the same attempt as the plugin URL. CPA can refresh model metadata in the background after startup, and an early run that compared against a catalog captured at startup failed once for that reason. Five further runs of that earlier version did not reproduce it.

CPA v8.0.4's built-in Codex model list includes `gpt-6.1-sol`, which `gpt-[0-9]*` keeps. That is intended: new GPT models appear without a configuration change.

## Codex CLI

Codex CLI 0.159.1 ran against a throwaway CPA container with the plugin configured. It used an isolated temporary `CODEX_HOME` with `model_catalog_url` set to the plugin URL and command-based provider auth, like a Keychain-backed setup. It never touched a real Codex configuration.

- `codex debug models` listed `gpt-6-astra`, `gpt-6.1-sol`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, and `gpt-5.5`. Hidden entries were `codex-auto-review` and the bundled `gpt-daybreak-blue-latest` and `gpt-daybreak-red-latest`, which shows Codex merged the catalog into its bundled one.
- CPA's log showed Codex requesting `/v0/resource/plugins/codex-catalog-filter/models?client_version=0.159.1`, and the plugin requesting `/v1/models?client_version=0.159.1` from `127.0.0.1`.
- With `model_catalog_url` pointed at a missing path and no cache, Codex got a `404` and showed its bundled catalog. `codex debug models --bundled` confirms that list is OpenAI models only, and includes `gpt-6.1-sol` in 0.159.1.

Earlier, while the plugin rewrote CPA's own response instead of serving a URL, Codex 0.159.0 against the same synthetic models showed that excluding a model Codex also bundles (`gpt-5.5`) under `remove` leaves Codex's bundled copy listed, while `hide` hides it. That behavior belongs to Codex's merge, not to where the catalog comes from.

## Suite coexistence in CPA v7.2.155

`python3 scripts/quota-cache-smoke.py --candidate codex-catalog-filter` passed: the candidate loaded and registered alongside the six published peers in the suite's pinned v7.2.155 image, with status routes, persistence, restart, and both optional-cache modes checked. A run with `--candidate token-usage` also passed and skipped the unreleased plugin, as the release tooling now does for any peer without a catalog entry. The full no-candidate suite, as `quota-cache` CI runs it, passed with all seven local libraries. `python3 -m unittest discover -s scripts/tests` (10 tests) and `python3 scripts/check-catalog.py` passed.

## Not verified here

- The production CPA instance and your real Codex configuration.
- Serving the catalog on CPA releases other than v8.0.4.
- Platforms other than Linux amd64.
