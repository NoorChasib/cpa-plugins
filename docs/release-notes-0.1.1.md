# Token Usage 0.1.1

Adds the Token Usage sidebar and existing-volume SQLite default. Published v0.1.0 and its historical notes remain unchanged. The [historical local prepublication verification](verification-sidebar.md) records the local acceptance results and limitations.

## Added

- A complete, read-only **Token Usage** sidebar page. Click the sidebar item while signed in with a compatible remembered console login to load statistics automatically—no manual API calls or second password form.
- Selected-interval input/output/observed-event totals; exact provider/model filters; 25-row pagination; expandable raw counters and executor provenance; coverage, last persistence and collection diagnostics.
- Default 24-hour view, 7/30-day presets ending at server coverage, and exact custom UTC `[from,to)` ranges with nanosecond precision. All token/event values remain exact decimal strings/`BigInt`.
- Account Health/Auto Baseline visual tokens and components, light/dark mode, labeled keyboard controls, readable large values and mobile table overflow. Source pins and reuse/avoid decisions are recorded in [sidebar-audit.md](sidebar-audit.md).

## Fixed

- CPA-generated mapping-shaped `store` metadata is accepted and discarded instead of blocking native registration. Unknown plug-in options, malformed documents and invalid limits still fail.
- Omitted `database-path` now defaults to `plugins/data/token-usage/usage.sqlite` beneath CPA's working directory captured at native construction. With standard official-image cwd `/CLIProxyAPI`, history lives under `/CLIProxyAPI/plugins/data/token-usage/` inside the existing **`cliproxy-plugins` volume**: **no Compose edit or additional mount**.
- Valid configuration with initially unavailable storage still exposes registration metadata/sidebar/config fields. Authenticated status reports sanitized 503 storage-unavailable without fabricated coverage/counters. Corrected configuration can retry until storage first opens successfully; later storage/collector changes still require native restart.

## Authentication and privacy

The public sidebar resource is fixed HTML/CSS/JavaScript with **zero private runtime values**. It automatically fetches only authenticated same-origin status/summary/models; private route menus remain empty. It is not a public operational snapshot.

The session reader supports the audited official console v1.22.15 plaintext and `enc::v1::` host/user-agent-bound obfuscation. Existing modern `cli-proxy-auth` is authoritative even after logout, invalid storage or remember-off; legacy recovery requires modern absence, a logged-in marker and an exact normalized base match. Default ports, terminal `/v0/management` and trailing slashes are normalized, without weakening origin/scheme/prefix binding.

Valid keys with internal spaces are supported using the browser's HTTP Headers ByteString rules rather than an ASCII-only filter. CR/LF/NUL, leading/trailing whitespace, other disallowed controls and values outside that header representation are rejected before fetching. The Latin-1 fixture verifies actual byte `0xe9`; it does not establish compatibility with arbitrary UTF-8-encoded operator keys.

A parent-only in-memory login, blocked storage or mismatched origin/base produces guidance rather than a credential bridge. There are no URL/log/message keys, additional credential persistence, external requests or new credential form. Session changes/401/403 abort requests, clear private values and stop retries. Redirects/HTML responses are rejected; private strings render as text under a restrictive hashed-script/style CSP and `frame-ancestors 'self'`. Installed same-origin scripts remain mutually trusted; CSP and obfuscation do not isolate untrusted plug-ins.

## Upgrade and operational notes

- **Keep any existing explicit clean absolute database path and its persistent mount.** This change never discovers, migrates, merges or resets old history. Removing an override selects a new default location; it does not move a database.
- Custom cwd/plugin-directory layouts need an explicit path if the captured default is outside the intended volume. No auth-directory/store-metadata discovery is performed.
- SQLite private-leaf `0700`, file `0600`, ownership/symlink/single-owner/local-WAL protections remain; the shared plugins root is not recursively changed. Back up first and do not replace a loaded native library.
- Presets that reach the moving retention boundary **proactively start exactly 60 seconds after the reported boundary**. Before fetching totals, the page discloses the exact excluded minute, explains the moving-boundary reason and states that this is **not the full retained window**. Omitted usage is not estimated. Presets already inside coverage and young installations with a stable history start lose no extra minute.
- Empty custom fields suggest the selected range or an appropriate range inside coverage, disclosing any retention margin. **Typed custom dates are never automatically changed**, including valid dates inside the suggested minute; expired/invalid values remain entered with guidance.
- If a later 416 still occurs, totals clear and the current boundary is shown. **Load range after retention edge** remains an explicit fallback using the newly reported boundary plus exactly 60 seconds, with the exclusion disclosed before clicking/fetching. There is no automatic retry after 416. A retained interval too short for the margin requires explicit custom dates, not fabricated totals.
- Stopped, unavailable, no elapsed coverage, empty and degraded states are distinct. Refresh is manual after initial loading. Summary/models are separate eventually consistent queries, not a single atomic snapshot.

## Compatibility, availability and verification

Target remains Linux amd64, native ABI 1 / RPC schema 6, with CPA v7.2.155 at `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. Candidate actual store-install/default-volume/restart acceptance passed in **`eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`**, Linux amd64, Debian bookworm/glibc **2.36**, cwd `/CLIProxyAPI`. The audited console is v1.22.15 at `ed5f1c48e11ba7335f1e8f676f228c280196af85`. Your deployed console/image/proxy policy remains unknown; this does not imply Alpine/musl, other-platform or arbitrary-version compatibility.

**Final post-review local verification passed:** `make ci` with **39 Python contracts**, Go tests, both production/nativefixture race and vet variants, and builds; **71 actual real-browser cases**, with zero axe 4.12.1 light/dark violations or incomplete checks; **2 ABI probes, 30 nativefixture and 38 production HTTP executions**; actual official-image store-handler/generated-enabled-and-store-only-config/native metadata/menu/auth/default SQLite/one-volume/restart checks; and final package/same-tested-library/**both CPA SDK installer** validation. The browser suite includes real `store.MakeCoverage` advancing 2 ms per request, mature-retention presets loading automatically in three requests per selection, stable young coverage, unchanged typed custom dates and actual HTTP-header key-byte checks.

The checksum-locked immutable published-v0.1.0 negative control, **executed optionally/manually locally rather than as a CI gate**, reproduced successful installation but failed native registration/status 404. The candidate also passed corrupt-SHA rejection, valid-config/unavailable-storage metadata/resource plus sanitized 503 without fake history, constant public bytes, actual HTTP security-header checks and positive CSP inline-script rejection. Its native page rendered **1 then 2 events** before/after same-volume restart using a synthetic remembered session generated by the pinned official codec and **real authenticated CPA JSON**. **Full console sign-in and sidebar-navigation clicks were not tested.** Browser tools are test-only and version/integrity pinned; both prepared workflows require browser/native/official-image gates before packaging.

All required local prepublication acceptance gates passed. Exact local artifact hashes and packaging provenance are retained in the [historical local verification record](verification-sidebar.md), not presented here as hosted release checksums. Validate downloaded assets using the `checksums.txt` supplied with their release.

The stable source `https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json` follows the latest published release. The version-specific v0.1.1 source is `https://github.com/NoorChasib/cpa-plugin-token-usage/releases/download/v0.1.1/registry.json`; its assets become available upon publication. Publishing assets does not install the plug-in or change an operator deployment.

## Unchanged limitations

CPA delivery is incomplete and has no durable replay. Streaming omissions, duplicate-looking observations, asynchronous admission-to-commit loss, bounded raw retention, overlapping auxiliary counters, best-effort diagnostics and unknown upstream completeness remain explicit. Healthy local storage is not complete provider consumption or billing accuracy. No charts, pricing, account/alias drilldown, rollups, CSV, cross-instance aggregation or plug-in write/reset/import API is added. No provider calls, production configuration/credential reads, deployment or reference-project changes are part of this candidate work.
