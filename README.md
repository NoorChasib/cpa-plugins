# CPA Token Usage

**Version 0.1.0** — a native CLIProxyAPI plugin that persists **CPA-reported** input, output, and supporting raw token counters by provider and execution model in SQLite.

Release repository: [NoorChasib/cpa-plugin-token-usage](https://github.com/NoorChasib/cpa-plugin-token-usage). The guarded [release workflow](.github/workflows/release.yml) publishes version 0.1.0 only after full tests, pinned native CPA acceptance, and draft-asset validation. **The import/download URLs below become usable after that release is published**; source preparation alone is not proof of publication. Nothing here deploys to or changes your running CPA instance.

## What it does

- Bounded asynchronous collection, raw-only SQLite history, checked exact aggregation, and decimal-string JSON counters.
- Authenticated, read-only `GET /status`, `/summary`, and `/models` under `/v0/management/plugins/token-usage`.
- Provider/model separation, executor provenance, success/failure counts, retention coverage, queue/drop/storage diagnostics, and clean/unclean run information.
- Required explicit persistent database path; no provider calls, credential-file access, or host callback bridge.

No UI, charts, rollups, CSV, account/alias drilldown, pricing, billing reconciliation, or cross-instance aggregation. There is no plugin write/reset/import API.

> **Collection is not lossless accounting.** CPA has no durable replay to the native plugin. In the verified CPA build, Claude split streams can omit input/cache, cumulative streams can keep only the first update, and failed/truncated streams can lose buffered tokens or retain an earlier success. Some executions emit no event. Zero reported tokens do not prove zero consumption. Cache/reasoning counters overlap and must not all be added together. See the [actual upstream fixture results](docs/upstream-compatibility.md#real-executor-to-native-synthetic-results).

## Compatibility and build

Claimed and natively tested platform: **Linux amd64 only**, Go `c-shared`, ABI **1**, RPC schema **6**. Compatibility floor: **CPA v7.2.155**, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. Older/newer CPA versions and other platforms are not validated by this release. The deployed CPA version was not inspected.

The module requires Go **1.26.0+**, CGO and GCC. CI/releases explicitly select **Go 1.27.1 on Ubuntu 24.04 amd64**; local validation used Go 1.27.1 / GCC 15.2.0. SQLite is bundled through `github.com/mattn/go-sqlite3`; a separate SQLite development library is not required. Release builds use **glibc**, and must pass the digest-pinned glibc native-smoke runtime before publication. The earlier local artifact referenced GLIBC through 2.34; that is not a promised maximum for the hosted build. Ubuntu 24.04 or a compatible newer glibc runtime is the conservative deployment target. **Alpine/musl and older glibc distributions are not supported by this binary.** The workflow logs `readelf --version-info` for the actual release bytes. Build and smoke-test against the target runtime when changing libc/toolchain.

From this project directory:

```sh
make ci
make smoke
make package-current VERSION=0.1.0
make checksums
make verify-release
```

`make smoke` **fails rather than skips** if Linux amd64, required tools, Docker, native loading, or an assertion is unavailable/fails. It compiles unmodified hash-verified CPA source, uses synthetic local upstreams inside a digest-pinned, network-disabled container, and does not read operator configuration or credentials. Build-time dependency/source/image downloads may use the network; runtime requests cannot leave the container.

The bundle is `dist/token-usage_0.1.0_linux_amd64.zip`, containing root `token-usage.so`, `LICENSE`, and `THIRD-PARTY-NOTICES.txt`. `dist/checksums.txt` covers the archive and generated `dist/registry.json`. The committed root `registry.json` is a schema-2 **GitHub-release** store source; it has no toolchain-dependent digest and is never rewritten by packaging. The generated release registry uses **direct** install with the exact versioned public ZIP URL, SHA-256, and size. Local packaging does not publish anything. The release workflow uses `package-tested` instead of rebuilding after native smoke, so uploaded bytes are the accepted production bytes.

## Install and configure

First read [operations](docs/operations.md) for persistent volumes, ownership, backup, and lifecycle procedures. These commands/configuration are operator instructions, not actions performed by this project.

### Import through the CPA plugin store

After [v0.1.0](https://github.com/NoorChasib/cpa-plugin-token-usage/releases/tag/v0.1.0) is published, add **one** of these as an additional plugin-store source (not a `.so` URL):

- **Stable source, follows latest published release:** `https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json`
- **Pinned 0.1.0 source, exact ZIP digest:** `https://github.com/NoorChasib/cpa-plugin-token-usage/releases/download/v0.1.0/registry.json`

The verified CPA configuration key is `plugins.store-sources`, a list of registry URL strings. Append to the existing list; CPA keeps its built-in official source automatically. Do not add both URLs unless you intentionally want duplicate source entries for the same plugin. Refresh the plugin store and select `token-usage` / **Token Usage**, version 0.1.0, on Linux amd64. The stable source uses CPA's GitHub-release mode: `version` is a display fallback, **not a tag lock**. Use the pinned source if future latest releases must not change the selected version.

CPA verifies the ZIP checksum before installing its versioned native library. Importing the store entry does **not** provision the persistent database path or enable the plugin; finish the configuration below before loading it. Public assets do not require a GitHub token/store-auth rule, though GitHub API rate limits can affect the stable source. The pinned direct source avoids the release-metadata API lookup. No central official-store listing is claimed.

### Configure storage and load the native plugin

1. Back up existing CPA configuration and any prior plugin database; stop CPA cleanly before manually changing native binaries. If using CPA's store UI, use its supported install/restart procedure and avoid replacing a loaded native library.
2. For manual installation, download all three assets from the trusted release: the ZIP, `checksums.txt`, and `registry.json`; then run `sha256sum -c checksums.txt` in that directory. For a local build, use `make verify-release`. A checksum detects corruption; it does not authenticate an untrusted manifest.
3. For manual installation, extract into a staging directory and install **one** `token-usage.so` into CPA's plugin directory, preferably `plugins/linux/amd64/`, preserving unrelated plugins. CPA store installation writes a versioned filename; do not also copy an unversioned library over it. Remove/relocate obsolete Token Usage libraries from scanned directories: CPA prefers versioned libraries over an unversioned file. Retain the downloaded archive and its accompanying license notices.
4. Prepare a dedicated private persistent data directory owned by the UID running CPA. It must be mode `0700`; existing database/lock/SQLite companion files must be mode `0600`, regular, non-symlink files owned by that UID. Keep the database out of the auth directory and out of replaceable plugin binaries.
5. **Merge** the following into the existing CPA configuration. Keep existing credentials, routing, management security, `plugins.configs` entries, and plugin directory settings. Do not paste a second top-level `plugins` mapping.

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  # Optional store import; append to existing sources, do not replace them.
  store-sources:
    - https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json
  configs:
    token-usage:
      enabled: true
      priority: 20
      database-path: /CLIProxyAPI/plugin-data/token-usage/usage.sqlite
      queue-capacity: 8192
      batch-size: 256
      flush-interval: 1s
      raw-retention: 720h
      maintenance-interval: 1m
      max-disk-bytes: 1073741824
      max-models: 10000
      query-timeout: 5s
```

Only `database-path` is required; the remaining storage values shown are defaults. `enabled`/`priority` belong to CPA selection/load order. Unknown keys and invalid values fail closed. Queue/batch/retention/budget/query limits are detailed in the [runtime contract](docs/runtime-contract.md#configuration). Storage/collector setting changes require a **native restart**, not a live reconfigure. An invalid path/config does not silently fall back to memory.

6. Start CPA, query authenticated status, and check `storage: "sqlite"`, `state: "running"`, limits, coverage, and collection diagnostics. Verify fresh synthetic or already-authorized traffic appears after a flush. Native collection was verified with `usage-statistics-enabled` **both false and true**; enabling CPA's destructive usage queue is not required.

## Query the private API

Use CPA's existing management authentication over an appropriately protected connection. These are management keys, not client API keys. No public resources are registered. Do not put a key in a URL, source file, or plugin configuration.

```sh
# Supply CPA_MANAGEMENT_KEY securely in your shell/session; do not commit it.
BASE=http://127.0.0.1:8317/v0/management/plugins/token-usage
curl --fail --silent --show-error \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" "$BASE/status"

# Choose a range inside status.coverage; to must not be in the future.
FROM='2026-09-09T06:00:00Z'
TO='2026-09-09T06:10:00Z'
curl --fail --silent --show-error --get \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  --data-urlencode "from=$FROM" --data-urlencode "to=$TO" "$BASE/summary"
curl --fail --silent --show-error --get \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  --data-urlencode "from=$FROM" --data-urlencode "to=$TO" \
  --data-urlencode 'limit=100' "$BASE/models"
```

Summary/models require RFC3339 `from` and `to`, use exact UTC `[from,to)` reported-request-time attribution (flagged receipt fallback when needed), and optionally filter exact `provider`/`model`. Models supports bounded `limit`/`offset` with deterministic provider/model ordering and `has_more`. There is no all-time promise beyond raw retention. A query preceding retained coverage returns **416**, not misleading zero totals. See [API and error semantics](docs/runtime-contract.md#private-api).

Counters are strings, for example `"input_tokens": "9007199254740993"`. Keep them as decimal strings or use arbitrary-precision integers, never JavaScript `Number`/floating-point arithmetic. `observed_events` counts received native observations, **not unique requests**. Results are eventually consistent with the asynchronous writer. `upstream_completeness` is always `"unknown"` even when local storage is healthy.

## Validation completed locally

- Unit/race/static checks and normal/fixture c-shared builds; see [runtime validation](docs/runtime-contract.md#completed-runtime-validation).
- The retained Phase A nativefixture suite: **30 real pinned CPA mock executions**, both statistics settings, all established upstream expectations preserved.
- The production library: **38 mock HTTP executions**, both settings, **18 committed observations per 19 executions**, including the known no-event Claude case. Verified all seven raw counters, provider/model grouping, executor provenance, authenticated status/summary/models, missing/invalid auth and public no-leak, SQLite integrity and sensitive-field exclusion, clean restart preserving history, and a new execution incrementing exactly once.
- A Claude nonstream mock input of **9007199254740993** remained exact through the real executor, native ABI, SQLite, and authenticated decimal-string API. A **separate direct ABI** probe verified two equal-looking failed/missing-account callbacks sum to **18014398509481986**, lifecycle configuration, stopped-status visibility, closed-query rejection, late RAM-only drop diagnostics, same-config reopen and shutdown/reinit on both native libraries; this direct probe is not an authentication test.

These are synthetic local tests, not live-provider measurements, power-loss testing, production deployment validation, or proof of complete upstream delivery. [Development and packaging](docs/development.md) explains reproducible commands, artifact checks, CI boundaries, and retained evidence. Licensing/provenance is preserved in [LICENSE](LICENSE) and [third-party notices](docs/third-party-notices.txt).
