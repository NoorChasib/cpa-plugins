# Sidebar design, authentication and storage audit

Scope: the **Token Usage 0.1.1 implementation**, implementing the approved existing-volume/sidebar plan. Reference revisions below were source-audited; this is not a claim that the three reference pages or the user's deployed console were live-browser tested. The reference repositories and deployment were not modified. Candidate execution evidence is separate in [verification-sidebar.md](verification-sidebar.md).

## Exact source pins and files

| Source | Audited revision | Relevant files and purpose |
|---|---|---|
| [Account Health Pushover v0.4.0][health] | `870456ecdbf3a86c76c6274f1d02e14dadddabf4` | [`internal/plugin/status_html.go`][health-style]: primary visual tokens, cards, pills, tables and timestamp presentation. [`internal/plugin/browser_auth_script.go`][health-auth]: session/HTML-upgrade behavior to audit, not copy wholesale. [`LICENSE`][health-license]: MIT, Copyright (c) 2026 NoorChasib. |
| [Auto Baseline v0.1.2][baseline] | `a7f5946b90e41d2a89d800cb143561fae0b4d9ea` | [`internal/plugin/status_html.go`][baseline-style]: same visual family. [`internal/config/config.go`][baseline-config]: accepted/ignored host `store` metadata and state below working-directory `plugins/`. [`internal/plugin/browser_auth_script.go`][baseline-auth]: auth helper comparison. [`LICENSE`][baseline-license]: MIT, Copyright (c) 2026 Noor Chasib. |
| [Reset Priority v0.1.4][reset] | `d5dfcb2a8517d87c7741ec07100f6400c7db60c3` | [`internal/plugin/status_page.go`][reset-page]: fixed data-free public shell. [`internal/plugin/runtime.go`][reset-runtime]: resource registration. [`internal/plugin/browser_auth_script.go`][reset-auth] and [`internal/plugin/management_status_page.go`][reset-private]: upgrade/auth comparison. [`LICENSE`][reset-license]: MIT, Copyright (c) 2026 Noor Chasib. |
| [CLIProxyAPI v7.2.155][cpa] | `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974` | [`internal/pluginhost/management.go`][cpa-management]: resource routing, private `Menu` conversion, response handling. [`internal/api/server_management.go`][cpa-auth]: public-resource versus authenticated-management split. [`sdk/pluginapi/types.go`][cpa-types]: resource/management wire types. [`Dockerfile`][cpa-docker]: image layout and working directory. |
| [Official Management Center v1.22.15][console] | `ed5f1c48e11ba7335f1e8f676f228c280196af85` | [`src/stores/useAuthStore.ts`][console-auth]: remembered session persistence/logout. [`src/services/storage/secureStorage.ts`][console-storage]: JSON serialization and storage. [`src/utils/encryption.ts`][console-codec]: host/user-agent XOR/Base64 codec. [`LICENSE`][console-license]: MIT, Copyright (c) 2026 Router-For.ME. |

These pins are compatibility evidence, not a claim that the user's installed console, CPA image or proxy policy matches them. In particular, the deployed console version and reverse-proxy configuration remain **unknown**.

The separate candidate installation regression now passed against exact official image `eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`, Linux amd64, Debian bookworm/glibc 2.36, cwd `/CLIProxyAPI`. It exercised the real store handler/generated config/native resource/SQLite/same-volume restart, plus a browser using the native page, pinned-codec synthetic remembered session and real CPA-authenticated JSON. This was **not full console sign-in or sidebar-navigation testing**, nor operator-deployment inspection. Final post-review/artifact checks remain separate.

## Visual decisions

**Account Health/Auto Baseline are the visual authority**, not Reset Priority's slightly different colors/type. The embedded `internal/plugin/pageassets/style.css` uses the reference values:

| Token | Light | Dark |
|---|---|---|
| Background | `#f6f7f9` | `#0c111d` |
| Surface | `#ffffff` | `#131a2b` |
| Border | `#e4e7ec` | `#1f2a3d` |
| Text | `#101828` | `#e6eaf2` |
| Muted | `#667085` | `#94a3b8` |
| Accent | `#2563eb` | `#3b82f6` |

The shared treatment includes `system-ui,-apple-system,"Segoe UI",Roboto,sans-serif`, 14px body/22px heading/13px table text, 1280px content width, 10px cards, 8px buttons, uppercase micro-labels, status pills, subtle borders and OS `prefers-color-scheme`. UTC timestamps are explicitly labeled. The current dark primary-button foreground is `#0c111d`, maintaining readable contrast on its blue accent. There are no external fonts/CDNs, additional console navigation, charts or categorical chart colors.

The page is a focused read-only view: selected-interval input/output/events, provider/model table, raw counters/provenance, coverage and local diagnostics. It does not introduce a normalization engine, pricing, account/alias drill-down or a new aggregation store. MIT credits for adapted styling and source material are preserved in [third-party notices](third-party-notices.txt), the project LICENSE and the adjacent console-fixture LICENSE.

## Public page is not public operational data

CPA's resource routes are **unauthenticated**. Moreover, a nonempty `Menu` on a management GET can convert that route into a legacy public resource. Therefore only resource `/status` receives `Menu: "Token Usage"`; all three private GET menus stay empty.

The Reset Priority **fixed, data-free shell** pattern is reused, but not a fetched-HTML/`document.write()` upgrade. Account Health's public redacted operational snapshot is not appropriate for Token Usage and is not copied. The candidate exposes exactly GET `/v0/resource/plugins/token-usage/status` as fixed HTML/CSS/JavaScript, with **zero private runtime values**. It is served before examining collector state or query values and must be byte-identical across data/auth/error states. Public route guesses must never reach the private JSON handlers.

`internal/plugin/status_page.go` embeds the three `pageassets/` files and hashes the actual embedded script/style bytes. CSP is `default-src 'none'`, hash-only script/style, `connect-src 'self'`, `frame-ancestors 'self'`, `base-uri 'none'`, `form-action 'none'`, `object-src 'none'`, with no-store, nosniff and no-referrer response headers. Private names/errors/provenance are text nodes, not dynamic HTML. Schema 6 preserves JSON strings rather than making them HTML-safe, so safe DOM rendering is necessary even behind authentication.

## Existing console session: audited constraints

1. **Cookies are not the management key.** Reference pages use their own origin's localStorage. `credentials: "same-origin"` alone cannot authenticate CPA management requests; an explicit management authorization header is required. The normal UX is to click Token Usage using the existing remembered login, not manually call APIs or complete another login form.
2. **Modern storage is authoritative.** Official `useAuthStore.ts` persists `cli-proxy-auth` as a Zustand `{state, version}` envelope. The persisted fields include `apiBase`, `rememberPassword`, and `managementKey` only when remembered; `isAuthenticated` is not required in the persisted subset. The candidate accepts version 0 with a remembered usable key/matching base and rejects an explicitly false authenticated flag. Any present modern record—including logged out, malformed, null, invalid-version, remember-off or missing-key state—blocks legacy fallback.
3. **Legacy-only recovery is narrow.** Only if the modern key is entirely absent may the page consider a literal legacy `isLoggedIn` value `"true"`, a usable `managementKey`, and matching legacy `apiBase`/`apiUrl`. Console logout can leave legacy entries behind; blindly copying fallback from a reference helper could revive a stale credential.
4. **Obfuscation is not protection from scripts.** Plain JSON and `enc::v1::` Base64/XOR records are supported. The codec salt uses `cli-proxy-api-webui::secure-storage|` plus `location.host`, `|`, and `navigator.userAgent`. This is reversible obfuscation, not secure encryption. Host or user-agent changes can make remembered obfuscated state unusable; the safe response is fresh console sign-in, not guessed decryption or relaxed binding.
5. **Bind credentials to effective origin and prefix.** The page derives its fetch root from its own path ending `/v0/resource/plugins/token-usage/status`. The stored base must URL-normalize to that exact origin and reverse-proxy prefix. Default HTTP/HTTPS ports, trailing slashes and a terminal `/v0/management` are normalized. Different schemes, effective ports, hosts or prefixes remain different. Userinfo/query/fragment bases are rejected. HSTS, aliases or redirects are not reasons to weaken the comparison: sign in using the correct HTTPS base/prefix. The stored base is never used as a fetch destination.
6. **Do not bridge unavailable credentials.** A parent console's in-memory-only/non-remembered session is not available to this page. Blocked storage, cross-origin embedding or an incompatible console session produces guidance. There is no new credential form, parent/opener inspection, `postMessage`, URL key, credential logging/persistence or CORS/frame relaxation.
7. **Read, validate, then render.** Keys are validated against Fetch's bounded **Headers ByteString** contract, not an ASCII-only allowlist: internal spaces and representable Latin-1 bytes work, while CR/LF/NUL, untrimmed values, other disallowed controls and unsupported Unicode are rejected before requests. The raw-byte `0xe9` browser fixture does not imply compatibility with arbitrary UTF-8-encoded operator keys. The only requests are same-origin GET status, summary and models, with explicit authorization, same-origin mode/credentials, no-store, redirect rejection, bounded body/deadline and abort signals. Status comes first; MIME/schema/source/field validation prevents HTML login pages or malformed responses becoming UI. `location.search` is not forwarded. Token/event values stay decimal strings and are formatted with `BigInt`.
8. **Stop on auth/session failure.** Session is reread for each request and checked before accepting responses. Storage changes/logout/401/403 abort work, clear private values and invalidate late responses. No aggressive polling or automatic auth retry is used: CPA can ban an IP after repeated bad management keys. The user explicitly refreshes after correcting login.
9. **Same-origin plug-ins remain mutually trusted.** Their scripts share the console's storage/credential trust boundary. Hash-based CSP and same-origin restrictions reduce accidental exposure; they do not isolate malicious same-origin plug-ins from the console or each other.

## Existing-volume storage decisions

`internal/config/config.go` accepts CPA's bounded mapping-shaped `store` node in a decode-only raw struct, discards it and preserves strict validation of real options. This follows the Auto Baseline compatibility lesson without adding runtime discovery or making Config noncomparable.

`internal/plugin/plugin.go:New` captures cwd once; missing `database-path` resolves beneath it to `plugins/data/token-usage/usage.sqlite`. With official-image cwd `/CLIProxyAPI`, this is inside the already mounted `/CLIProxyAPI/plugins` volume. **No Compose change or additional mount** is needed for that standard layout. Nonstandard cwd/plugin directories need an explicit absolute path into the intended existing persistent storage.

An existing explicit clean absolute path remains authoritative. No automatic history search/migration/reset occurs. `internal/store/store.go:prepare` retains private-leaf `0700`, file `0600`, ownership/single-link/symlink checks, local-filesystem/WAL policy and exclusive locking, without recursively changing the shared volume root. Auth directories and host metadata are never storage-discovery sources.

Valid initial storage failure keeps metadata/sidebar/config fields available and yields sanitized status 503 without coverage/counters or fake zero history. Corrected configuration may retry before the first successful collector; success establishes the history binding, after which storage/collector changes require native restart. Invalid config/protocol is not granted this recovery contract.

## Retention and evidence limits

Presets end at server coverage and visibly show clipping. If the requested start is at/before the **rolling** coverage start (`coverage.from == retention_floor`), the page proactively selects exactly **reported floor + 60s**, disclosing the excluded `[reported floor,new from)` minute, moving-boundary reason and **not the full retained window** before fetching totals. This avoids an immediately expiring boundary; it is not an estimate of omitted usage. Interior presets and a young/stable coverage start receive no arbitrary minute subtraction. Nanosecond UTC boundaries and decimal counters remain exact.

Empty custom fields reuse an appropriate selected range or suggest a disclosed range inside coverage; **typed custom dates never shift automatically**. An interval too short to leave the margin produces guidance rather than zeros. A later rolling 416 still clears totals and offers an explicit fallback button for `newly reported coverage.from + 60s`, disclosing the exclusion before clicking/fetching, with no automatic retry. The button is not the only case where the page excludes a minute. No UI state implies complete provider delivery or billing accuracy.

The reproducible browser test uses byte-identical pinned `encryption.ts` and `secureStorage.ts`, plus their LICENSE, under `internal/plugin/frontendtests/upstream/`; Node strips TypeScript for test execution. Required acceptance pins Node 26.8.1/npm 11.19.0, agent-browser 0.37.1 via npm lock and binary SHA, and Chrome for Testing 153.0.8010.36 via ZIP SHA. The fixtures and `scripts/browser/` tools are test-only, not a front-end runtime dependency or production session setter. Candidate tests execute the shipped page rather than merely checking JavaScript source strings. The **final post-review local gates all passed**: 39 Python contracts, 71 actual browser scenarios, native/official-image integration and exact tested-library/package/frozen-notice validation. The [candidate verification record](verification-sidebar.md) records the completed evidence, local artifact hashes and limitations separately. These passes do not establish the deployed console/proxy policy or full console sign-in/sidebar-navigation behavior, and do not authorize publication.

[health]: https://github.com/NoorChasib/cpa-plugin-account-health-pushover/tree/870456ecdbf3a86c76c6274f1d02e14dadddabf4
[health-style]: https://github.com/NoorChasib/cpa-plugin-account-health-pushover/blob/870456ecdbf3a86c76c6274f1d02e14dadddabf4/internal/plugin/status_html.go
[health-auth]: https://github.com/NoorChasib/cpa-plugin-account-health-pushover/blob/870456ecdbf3a86c76c6274f1d02e14dadddabf4/internal/plugin/browser_auth_script.go
[health-license]: https://github.com/NoorChasib/cpa-plugin-account-health-pushover/blob/870456ecdbf3a86c76c6274f1d02e14dadddabf4/LICENSE
[baseline]: https://github.com/NoorChasib/cpa-plugin-auto-baseline/tree/a7f5946b90e41d2a89d800cb143561fae0b4d9ea
[baseline-style]: https://github.com/NoorChasib/cpa-plugin-auto-baseline/blob/a7f5946b90e41d2a89d800cb143561fae0b4d9ea/internal/plugin/status_html.go
[baseline-config]: https://github.com/NoorChasib/cpa-plugin-auto-baseline/blob/a7f5946b90e41d2a89d800cb143561fae0b4d9ea/internal/config/config.go
[baseline-auth]: https://github.com/NoorChasib/cpa-plugin-auto-baseline/blob/a7f5946b90e41d2a89d800cb143561fae0b4d9ea/internal/plugin/browser_auth_script.go
[baseline-license]: https://github.com/NoorChasib/cpa-plugin-auto-baseline/blob/a7f5946b90e41d2a89d800cb143561fae0b4d9ea/LICENSE
[reset]: https://github.com/NoorChasib/cpa-plugin-reset-priority/tree/d5dfcb2a8517d87c7741ec07100f6400c7db60c3
[reset-page]: https://github.com/NoorChasib/cpa-plugin-reset-priority/blob/d5dfcb2a8517d87c7741ec07100f6400c7db60c3/internal/plugin/status_page.go
[reset-runtime]: https://github.com/NoorChasib/cpa-plugin-reset-priority/blob/d5dfcb2a8517d87c7741ec07100f6400c7db60c3/internal/plugin/runtime.go
[reset-auth]: https://github.com/NoorChasib/cpa-plugin-reset-priority/blob/d5dfcb2a8517d87c7741ec07100f6400c7db60c3/internal/plugin/browser_auth_script.go
[reset-private]: https://github.com/NoorChasib/cpa-plugin-reset-priority/blob/d5dfcb2a8517d87c7741ec07100f6400c7db60c3/internal/plugin/management_status_page.go
[reset-license]: https://github.com/NoorChasib/cpa-plugin-reset-priority/blob/d5dfcb2a8517d87c7741ec07100f6400c7db60c3/LICENSE
[cpa]: https://github.com/router-for-me/CLIProxyAPI/tree/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974
[cpa-management]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/management.go
[cpa-auth]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/api/server_management.go
[cpa-types]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginapi/types.go
[cpa-docker]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/Dockerfile
[console]: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/ed5f1c48e11ba7335f1e8f676f228c280196af85
[console-auth]: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ed5f1c48e11ba7335f1e8f676f228c280196af85/src/stores/useAuthStore.ts
[console-storage]: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ed5f1c48e11ba7335f1e8f676f228c280196af85/src/services/storage/secureStorage.ts
[console-codec]: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ed5f1c48e11ba7335f1e8f676f228c280196af85/src/utils/encryption.ts
[console-license]: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ed5f1c48e11ba7335f1e8f676f228c280196af85/LICENSE
