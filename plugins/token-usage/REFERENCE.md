# CPA Token Usage

**Token Usage.** A native CLIProxyAPI plug-in that persists **CPA-reported** token usage in SQLite and displays it in a dedicated **Token Usage** sidebar page.

## Open Token Usage

With the plug-in installed and enabled, **click Token Usage in the CPA console sidebar**. If you are already signed in on the same origin/API base with **Remember password** enabled, the page automatically loads your retained statistics. You do not need to call APIs manually, enter a second plug-in password, or configure a credential bridge.

The page provides:

- Input tokens, output tokens and observed usage events for the selected interval.
- Last 24 hours by default, 7/30-day presets, and an exact custom UTC interval.
- Case-sensitive, exact-match provider/model filters; 25-row pages and expandable raw counters/executor provenance.
- Last persistence, retention coverage, local collection health and best-effort diagnostics.
- The Account Health/Auto Baseline visual family, system light/dark mode, keyboard controls and a horizontally scrollable model table on small screens.

If the page cannot read a compatible remembered session, it explains how to sign in using its exact origin and API base. A console login held **only in the parent page's memory**, a blocked localStorage, or a different scheme/hostname/port/prefix cannot be reused. Modern logged-out, invalid or remember-off storage never falls back to an old legacy key. See [session troubleshooting](docs/operations.md#sidebar-session-and-range-troubleshooting) and the [pinned design/auth audit](docs/sidebar-audit.md).

### Reading ranges and totals

Presets end at the server's reported coverage, **not your device clock**, and visibly disclose clipping to available local history. If a preset reaches the **moving retention boundary**, the page proactively starts exactly **60 seconds after that reported boundary** so the range can load while retention advances. Before fetching totals, it shows the excluded `[reported boundary, selected start)` minute, explains why it is excluded, and states that this is **not the full retained window**; that usage is not estimated. Presets already inside coverage do not lose a minute. A young installation's stable history start is kept exactly, with no extra minute omitted.

Custom timestamps require UTC `Z`, seconds, and optionally up to nine fractional-second digits; the interval is exact **[from,to)**. Empty custom fields suggest the selected range or an appropriate range inside coverage, with any retention margin disclosed. **Dates you type are never automatically shifted**; an invalid or expired range stays entered and shows guidance instead.

If a query still receives 416 because coverage moved, statistics are cleared and the returned coverage is shown. When enough interval remains, **Load range after retention edge** remains an explicit fallback: it offers a custom start exactly 60 seconds after the newly reported boundary, discloses the exclusion **before you click and before fetching**, and keeps the selected end. There is no automatic retry after 416. A very short retained interval may require your own explicit custom dates rather than a one-minute margin.

Refresh is manual after initial loading. Editing filters clears old statistics until applied; **Refresh** also applies the current inputs. Empty retained history, no elapsed coverage, stopped collection, degraded collection, and unavailable storage are distinct states—not interchangeable zero totals. Summary and model queries are separate, eventually consistent reads, not one atomic snapshot.

> **Not lossless accounting or billing.** CPA has no durable replay to the native plug-in. In the verified CPA build, Claude split streams can omit input/cache, cumulative streams can keep only the first update, and failed/truncated streams can lose buffered tokens or retain an earlier success. Some executions emit no event. Zero reported tokens do not prove zero consumption. Cache/reasoning counters overlap and must not all be added together. See the [actual upstream fixture results](docs/upstream-compatibility.md#real-executor-to-native-synthetic-results).

No charts, pricing, CSV, rollups/all-time guarantee, account/alias drilldown, cross-instance aggregation, billing reconciliation, or plug-in write/reset/import API is provided.

## Install and configure

### CPA store sources

Use one of these sources:

Add the combined source once:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

Select Token Usage in CPA's Plugin Store. The catalog pins each plugin's own published version and verified ZIP; updates advance that entry independently. Keep your existing source alias if already installed. See [release behavior](../../docs/releases.md).


See [development](docs/development.md) and the [historical local prepublication verification](docs/verification-sidebar.md) for build and validation details. Do not replace a loaded native library. Follow CPA's supported install/restart procedure, retain license notices and a verified backup, and keep only one selected Token Usage version in scanned plug-in directories. An old versioned library can shadow an unversioned manual copy.

### Existing plugins volume; no Compose change

For the standard official-image layout with CPA working directory `/CLIProxyAPI` and the existing **`cliproxy-plugins` volume mounted at `/CLIProxyAPI/plugins`**, no Compose edit or additional mount is needed. Omit `database-path` to use:

```text
/CLIProxyAPI/plugins/data/token-usage/usage.sqlite
```

The general default is **`<CPA working directory>/plugins/data/token-usage/usage.sqlite`**, captured once when the native instance is constructed. It does not follow later working-directory changes, discover CPA auth directories, or derive a location from store metadata. A custom working directory or independently configured `plugins.dir` requires an explicit absolute path if that default is not inside the intended persistent volume. The deployed image/console/proxy combination has not been inspected; see the verification record for the exact tested boundaries.

Merge the candidate's minimal configuration into the existing mapping; do not replace unrelated CPA settings or hand-edit CPA's generated `store` metadata:

```yaml
plugins:
  enabled: true
  configs:
    token-usage:
      enabled: true
      # database-path is optional for the standard existing-volume layout.
      # Preserve an existing explicit database-path when upgrading.
```

Defaults are queue 8192, batch 256, flush 1s, raw retention 720h, maintenance 1m, disk budget 1073741824 bytes, model limit 10000, and query timeout 5s. CPA-owned `enabled`, `priority`, and a bounded `store` mapping are accepted; store metadata is discarded, while unknown real options and invalid bounds still fail. See [configuration](docs/runtime-contract.md#configuration) for the complete contract.

**Existing history is never migrated, searched for, reset, or merged automatically.** Keep any existing clean absolute `database-path` override. Removing an override selects the default on the next native instance; it does not move the old database and can make an unrelated new history appear empty. Once storage has opened successfully, storage/collector configuration changes require a **native restart**.

Use a private, local SQLite-WAL-capable directory owned by the CPA UID. The plug-in creates a missing private leaf with mode `0700`, with database/companions `0600`, without recursively changing the shared plugins-volume root. Unsafe existing modes/ownership, symlinks/hardlinks, another owner or an incompatible database fail closed. Do not mount only the SQLite file: its WAL/SHM/lock companions must share the persistent directory. [Operations](docs/operations.md) covers backup and recovery.

If valid configuration cannot initially open storage, registration metadata, the sidebar and the database-path config field remain discoverable. The authenticated status is a sanitized **503 storage-unavailable** response, not fabricated zero history or an in-memory fallback. Correct the storage problem and use CPA's supported reconfigure/restart flow; clicking Refresh alone does not reopen storage. Invalid configuration/protocol still fails registration.

## Privacy and the API behind the page

The sidebar automatically makes three read-only authenticated GETs in order: `/status`, `/summary`, then `/models`, under `/v0/management/plugins/token-usage`. CPA's existing management-key authentication protects every private route; each private `Menu` stays empty. The public resource `/v0/resource/plugins/token-usage/status` serves **fixed HTML/CSS/JavaScript bytes with zero private runtime values**. It is a page loader, not a public usage snapshot.

The browser reads only a compatible session from its own origin, rechecks credentials for each request, and sends them only in the authorization header to the three allowlisted same-origin paths. Valid keys containing internal spaces are supported; unsafe line breaks/NUL, leading or trailing whitespace, and values the browser cannot represent in an HTTP header are rejected before any request. Redirects and HTML login responses are rejected. Logout/storage changes or 401/403 clear private values, abort requests and stop retries. Keys are not persisted again, passed in URLs/messages, logged, or requested in a new form. The `enc::v1::` console storage format is reversible obfuscation, **not encryption**.

A restrictive hashed-script/style CSP permits same-origin connections and framing only (`frame-ancestors 'self'`); private strings render as text. This does not isolate installed same-origin scripts from one another: they share the console credential trust boundary. Do not broaden CORS/framing or expose management publicly to work around session incompatibility.

For integrations, the [private API reference](docs/runtime-contract.md#private-api) documents parameters and errors; manual API calls are optional, not the normal sidebar workflow. Counters are decimal strings, such as `"9007199254740993"`; the sidebar formats them with `BigInt`, never floating-point arithmetic. `observed_events` means received observations, not unique requests, and upstream completeness remains `unknown` even when local storage is healthy.

## Compatibility and verification

Target: **Linux amd64**, Go `c-shared`, ABI **1**, RPC schema **6**; CPA **v7.2.155**, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. The audited console is official Management Center **v1.22.15**, commit `ed5f1c48e11ba7335f1e8f676f228c280196af85`. Neither pin establishes the version of your deployed console or reverse proxy. Other platforms/versions are not validated.

The module needs Go 1.26.0+, CGO and a compatible C toolchain; SQLite is bundled by `github.com/mattn/go-sqlite3`. Candidate installation/startup passed in the exact official image **`eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`**, Linux amd64, Debian bookworm/glibc **2.36**, cwd `/CLIProxyAPI`. This does not identify your deployed image or promise Alpine/musl/older-glibc compatibility. Historical source-built and current official-image evidence are separated in [development](docs/development.md).

**Final post-review local gates passed:** `make ci` with **39 Python contracts**, Go tests, both production/nativefixture race and vet variants, and builds; **71 actual real-browser scenarios**, with zero axe light/dark violations or incomplete checks; **2 ABI probes, 30 nativefixture and 38 production HTTP executions**; actual official-image store/default-volume/native-browser/restart acceptance; and final package/checksum/**both CPA SDK installer** validation. The browser regressions include real moving coverage, mature-retention presets loading automatically in one three-request sequence, exact young-history/custom ranges and compatible HTTP-header keys.

The real store handler generated only `enabled` plus `store`, with no database-path, then native metadata/menu/auth and SQLite collection worked on **one plugins volume**. Restart preserved history; one new event increased the count from **1 to 2**, which the native page rendered using a pinned-codec synthetic remembered session and **real authenticated CPA JSON**. Actual HTTP security headers, a positive CSP blocked-inline-script probe, unavailable-storage 503 and fixed public bytes passed. An optional, manually executed checksum-locked published-v0.1.0 negative control reproduced installation succeeding but native registration failing; that control is not a CI gate. This is **not** full console login/sidebar-navigation testing or an operator installation.

The [historical local prepublication verification](docs/verification-sidebar.md) records the accepted local artifacts, checks and limitations. Those local hashes are not assertions about hosted release bytes. Verify downloaded release assets against the `checksums.txt` supplied with that release.

The unchanged [historical v0.1.0 verification](docs/verification.md) records the earlier 30 nativefixture and 38 production mock executions, exact large counters, authentication, persistence and restart checks. Those establish that stage's results, not a pass for later candidate changes. No provider credentials, billable calls, production configuration access, deployment or new publication are needed for local synthetic validation.

See [v0.1.1 release notes](docs/release-notes-0.1.1.md), [LICENSE](LICENSE), and [third-party notices](docs/third-party-notices.txt).
