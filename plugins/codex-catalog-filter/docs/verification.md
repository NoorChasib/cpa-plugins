# Verification record: Codex Catalog Filter 0.1.0

Local results from 2026-09-29 on Linux amd64 with Go 1.27.1 and GCC 15.2.0, before the first release. Hosted CI reruns the same `make ci` and `make smoke` gates on each pull request and release. No real provider credentials, production configuration, or production CPA instance were used.

## Unit, race, and static checks

`make ci` passed: gofmt, `go vet`, `go test`, `go test -race`, and the Linux amd64 `c-shared` build, which exports `cliproxy_plugin_init`, `cliproxyPluginCall`, `cliproxyPluginFree`, and `cliproxyPluginShutdown`.

| Area | Covered |
| --- | --- |
| Catalog rewrite | Remove and hide produce byte-exact expected output; kept entries, separators, other top-level members, and pretty-printed formatting are preserved; a missing `visibility` is added; order is preserved |
| Scope | OpenAI, Claude, Gemini (`models` with `name`), and Grok lists; wrong source format; completion responses (`Model`, `RequestedModel`, request bodies present); streams; non-200 status |
| Fail-open | Malformed or truncated JSON, trailing data, a second document, non-object bodies, empty or non-array `models`, entries without a string slug, duplicate members, nothing to change, and nothing left listed all return the original |
| Rules | Include/exclude/action combinations; exclude beats an exact include; `codex-auto-review` is kept by `codex-*` and dropped without it |
| Globs | `*` across `/`, `?`, classes, ranges, negation, literal `]` and `-`, escapes, Unicode; malformed patterns rejected |
| Configuration | Default `remove`; `hide`; idle without `include`; host `enabled`, `priority`, and `store` keys tolerated; unknown keys, bad actions, bad globs, empty/padded/oversized patterns, too many patterns, extra documents, and oversized documents rejected |
| Plugin | Registration advertises only `response_interceptor` at schema 6 with required metadata; old schema rejected; rejected reconfiguration keeps the previous rules; idle before configuration and after shutdown; `ETag`/`Content-Length` cleared on rewrite; concurrent intercept and reconfigure under `-race` |
| ABI | Envelope shapes; the literal oversized-request answer decodes to an empty interceptor result; ordinary and oversized interceptor calls are answered without reading or copying the request, while model-list candidates and lifecycle calls are not; errors and panics never echo payloads; shutdown drains an admitted call |
| Screen | Rejects at the first disqualifying field even when the rest of the request is not JSON; never rejects a model-list request or an unrecognised layout |

Seven targeted mutations of the scoping, fail-open, include/exclude, separator, default-action, and ETag logic were each caught by at least one test.

`FuzzRewrite` checks every rewrite against an independent decode: valid JSON, other top-level members unchanged, exactly the allowed entries in order (others hidden under `hide`), and at least one listed entry. It ran about 9.9 million inputs over 60 seconds with no failure; `FuzzGlob` ran 20 seconds without a panic. Their seeds run with every `go test`.

An independent review before the first release found no correctness, memory-safety, or concurrency defect. It led to three changes, each covered by tests:

- `*` and `?` now also match a newline.
- Configuration is parsed inside the lifecycle lock, so concurrent reconfigurations apply in call order.
- Interceptor calls are screened in place before any copy. `BenchmarkScreenLargeCompletion` rejects an 89 MB completion request in about 0.9 µs with 648 bytes allocated. Before, every call copied the full request and scanned all of it.

## Official CPA v8.0.4 image

`make smoke` passed six consecutive times, including twice after the review changes, (about 3 seconds each). It runs the built library in `eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d`, whose log reports `CLIProxyAPI Version: v8.0.4, Commit: d33f63f`. Models come from synthetic static Codex, Claude, xAI, and OpenAI-compatible API-key groups. A mock upstream in the pinned Python image shares CPA's network namespace and answers one chat completion.

| Step | Result |
| --- | --- |
| Load | CPA loads and registers the plugin at version 0.1.0, enabled |
| Idle (no `include`) | `GET /v1/models?client_version=0.159.0` returns 18 entries (590,828 bytes) including Claude, `cpa-*`, `or-*`, Grok, and image models; two snapshots are byte-identical |
| `include: [gpt-[0-9]*, codex-*]` (default `remove`) | 9 entries (568,006 bytes), byte-identical to the idle catalog with non-matching entries removed. Listed: `gpt-6.1-sol`, `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`. `codex-auto-review` kept as `hide`. No `ETag` header |
| Other responses while active | Plain `GET /v1/models` (OpenAI), `GET /v1/models` with `Anthropic-Version` (Claude), `GET /v1beta/models` (Gemini), and a non-streaming chat completion are unchanged |
| `action: hide` | All 18 entries kept, byte-identical except the changed `visibility` values; same listed set |
| `exclude: [gpt-5.5]` | Removed, byte-exact |
| `action: drop` | CPA logs `plugin.reconfigure failed` and deactivates the plugin; the catalog is served unfiltered. Fixing the value re-registers the plugin and filtering resumes without a restart |
| Restart | The persisted configuration filters from startup |

Model lists other than the Codex catalog are compared after each configuration reload with entry order and the `created`/`created_at` registration timestamps normalized. CPA builds those lists from a map and re-stamps statically configured models on every reload. Between reloads the bytes are identical. The Codex catalog is sorted by priority and compared byte for byte throughout.

CPA v8.0.4's built-in Codex model list includes `gpt-6.1-sol`, which `gpt-[0-9]*` keeps. That is intended: new GPT models appear without a configuration change.

## Codex CLI 0.159.0

`codex debug models` was run once against a throwaway filtered CPA container. It used an isolated temporary `CODEX_HOME` with command-based provider auth, like a Keychain-backed setup, and never touched a real Codex configuration.

- Filtered: listed `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `gpt-6.1-sol`. Hidden: `codex-auto-review`, plus the bundled `gpt-daybreak-blue-latest` and `gpt-daybreak-red-latest`, which confirms Codex merged CPA's catalog into its bundled one. `models_cache.json` recorded no `etag`.
- Idle: Codex also listed `claude-fable-5-1`, `cpa-sonnet`, `grok-4.7`, and `or-kimi-k2`.
- With `exclude: [gpt-5.5]`: under `remove`, CPA's catalog no longer contained `gpt-5.5`, but Codex still listed its bundled copy. Under `hide`, Codex showed it as `hide`.

## Install without a restart

The CPA v8.0.4 image started with no Codex Catalog Filter file or configuration. The library was then copied to `plugins/linux/amd64/codex-catalog-filter-v0.1.0.so`, which alone changed nothing. Next, a hand-written `codex-catalog-filter` block was appended to `config.yaml`, including a complete `store:` block in the Plugin Store's layout. CPA's configuration reload loaded and registered the plugin from that path, and the Codex catalog was filtered within 2 seconds, with no restart.

## Suite coexistence in CPA v7.2.155

`python3 scripts/quota-cache-smoke.py --candidate codex-catalog-filter` passed. The candidate loaded and registered alongside the six published peers in the suite's pinned v7.2.155 image, with status routes, persistence, restart, and both optional-cache modes checked. A run with `--candidate token-usage` also passed and skipped the unreleased plugin, as the release tooling now does for any peer without a catalog entry. `python3 -m unittest discover -s scripts/tests` (10 tests) and `python3 scripts/check-catalog.py` passed.

## Not verified here

- The production CPA instance and your real Codex configuration.
- Hosted GitHub Actions results, until the pull request runs them.
- Platforms other than Linux amd64.
