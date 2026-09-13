# Token Usage 0.1.0

Initial native plugin release for persistent **CPA-reported** raw token usage by provider and execution model. This is operational visibility, **not billing reconciliation or lossless accounting**.

## Supported target

- Linux amd64 **glibc** only; Go c-shared, native ABI 1, RPC schema 6.
- Validated against **CPA v7.2.155**, exact commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. Older/newer CPA versions and other platforms are not validated.
- Release workflow selects Go 1.27.1 on Ubuntu 24.04 and requires the pinned real CPA native/SQLite/restart suite. Use Ubuntu 24.04 or a compatible newer glibc runtime; Alpine/musl is unsupported. Actual GLIBC symbol requirements appear in the workflow's `readelf` output.

## Included

- Bounded asynchronous collection into a private persistent SQLite database; raw retention defaults to **720h / 30 days**.
- Exact integer aggregation and decimal-string JSON counters, provider/model grouping, executor provenance, success/failure observations, retention coverage, and queue/drop/storage diagnostics.
- Authenticated, read-only `/status`, `/summary`, and `/models` under `/v0/management/plugins/token-usage`.
- Clean restart preserving committed history; lifecycle, failure, privacy, authentication, large-integer, and actual CPA installer acceptance gates.
- ZIP with `token-usage.so`, `LICENSE`, and dependency notices; `checksums.txt` covers the ZIP and direct-install `registry.json`.

## Import after publication

Add **one** of these URL strings to CPA's existing `plugins.store-sources` list, refresh the plugin store, and choose Token Usage:

- Stable source (tracks latest): `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json`
For current installation and configuration, use the [Token Usage quick start](../README.md).

The stable source uses CPA's GitHub-release installer; its version is a display fallback rather than a tag lock. The pinned source explicitly selects only Linux amd64. Public source/download URLs become usable when the release is published. No central official-store listing is claimed.

For current installation and configuration, use the [Token Usage quick start](../README.md).

## Important limitations

- CPA upstream delivery is incomplete: Claude split streams can omit input/cache, cumulative streams can retain only the first update, and failed/truncated streams can lose buffered tokens or retain earlier success. Some executions emit no event. Zero reported tokens do not prove zero consumption. Cache/reasoning counters overlap and must not all be added together.
- There is no durable CPA replay. Asynchronous RAM admission can be lost on crashes; shutdown cannot recover upstream events never received, and late diagnostics are best effort. Healthy SQLite does not establish upstream completeness.
- No UI/charts, account or alias filters, rollups/all-time guarantees beyond raw retention, pricing, CSV, write/reset/import API, or cross-instance aggregation.
- Release publication does not inspect, configure, deploy to, or validate your production CPA instance. Synthetic native tests are not live-provider measurements or a billing guarantee.
