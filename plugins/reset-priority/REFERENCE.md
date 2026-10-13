# Reset Priority for CLIProxyAPI

`reset-priority` is a native CLIProxyAPI (CPA) plugin that maintains hard credential priorities for Claude OAuth and Codex OAuth accounts. Within each provider, the account whose regular weekly quota resets soonest receives the highest priority. Optionally, it also chooses which Claude API key from CPA's configuration serves a request, by Console organization credit; see [Claude API credit routing](#claude-api-credit-routing-optional).

For OAuth credentials the plugin is a priority manager, not a utilization-balancing scheduler. It does not use weights and does not modify CPA's global routing configuration. Only the optional API credit router declares CPA's scheduler capability, and it chooses only among Claude API keys from CPA's configuration; it never writes a priority.

## Policy invariant

The exact deadline of the regular weekly quota is the only value allowed to determine ordering among healthy OAuth accounts:

- Claude: `seven_day.resets_at`
- Codex: the window whose declared duration is exactly `604800` seconds (`10080` minutes), using `reset_at`/`resetAt` or `reset_after_seconds`/`resetAfterSeconds`; usage probes require a resolved ChatGPT account ID and send a Codex-CLI-shaped `User-Agent`

The plugin does not rank OAuth accounts from utilization, percentage used or remaining, five-hour windows, model-scoped Claude windows, monthly Codex windows, additional/code-review limits, reset credits, plan type, burn rate, request success, cooldown state, or any other signal.

Claude and Codex are ranked independently.

## Priority model

For `N` healthy accounts in one provider group:

```text
priority = floor + step * (N - 1 - rank)
```

The v0.1.0 defaults are a hard floor of `100`, a step of `100`, and a quarantine sentinel of `0`:

```text
1 healthy account: 100
2 healthy accounts: 200, 100
3 healthy accounts: 300, 200, 100
5 healthy accounts: 500, 400, 300, 200, 100
```

Accounts with a future confirmed or still-future stale weekly reset sort first by the exact timestamp. Healthy accounts with `awaiting_new_window` or `unknown` reset state sort afterward with a stable filename/ID tie-break and still count in `N`.

### Credential health is separate from quota availability

Only definitive credential-health failures leave the active ranking pool:

- intentionally disabled credentials;
- narrow CPA auth-error states indicating unauthorized, revoked, forbidden, `invalid_grant`, or reauthentication required;
- recovered credentials that have not yet produced a fresh post-recovery future weekly reset.

These accounts receive priority `0`, do not count in the healthy `N`, and have that sentinel written into their physical auth JSON so CPA's selector actually demotes them.

Quota exhaustion, HTTP 429, CPA cooldown/backoff, temporary unavailability, five-hour exhaustion, model-specific exhaustion, and generic network/provider failures are not quarantine signals. If explicit transient wording appears alongside a generic auth marker such as `forbidden`, `403`, `revoked`, or `reauth`, the transient classification wins; disabled state, `unauthorized`, and `invalid_grant` remain authoritative. CPA continues to own request-time availability and failover.

While any account is quarantined or recovering, the plugin runs a short local health probe (about once a minute) over the `host.auth.get_runtime` callback. It reads runtime health fields only — no credential JSON and no provider quota request — so CPA self-recovery is detected in about a minute rather than at the next hourly reconciliation. The probe never mutates state; a detected change triggers the ordinary reconciliation. It is disarmed whenever every managed account is healthy. After the plugin itself saves a quarantine sentinel, it requires a later token-refresh or physical-file modification timestamp before accepting the host's `active` runtime state as recovery, preventing the audited save-side active rebuild from falsely re-enabling the account.

A reauthenticated account remains `recovering` at `0` until the plugin confirms a future regular weekly reset observed after recovery. Faster detection does not shortcut this: cached pre-failure data cannot re-promote it.

## Exact reset rollover

The hourly reconcile is a safety net, not reset-time precision. The plugin maintains an exact timer for the nearest known weekly deadline.

At the deadline it synchronously:

1. marks the expired observation `awaiting_new_window`;
2. removes that old timestamp from active earliest-deadline ordering;
3. recomputes priorities and writes the local demotion;
4. only then starts asynchronous provider reads.

Fresh reads use bounded retries at `+5s`, `+30s`, `+2m`, `+5m`, and `+15m`, then return to the normal reconciliation interval. An expired or repeated provider timestamp never re-promotes the account.

In quota-cache mode the same schedule also handles first confirmation for accounts not yet confirmed in this process: every account after a restart, and an account when it is added. It is armed after each reconciliation that leaves such an account `unknown`, as one schedule shared by all of them. Each step re-reads every still-unconfirmed account from the local snapshot, never from the provider. The schedule writes the corrected order once: at the step that confirms the last unconfirmed account, or at the `+15m` step for the accounts confirmed by then. Until that write, an account the schedule has confirmed shows `confirmed` but keeps its provisional priority, unless another write (a reset deadline, a retry, or a reconciliation) persists the order first. A step that confirms nothing writes nothing. An account leaves the schedule at its first cache answer. A fresh entry that reports no weekly window also counts, so the same answer is not re-read on every pass. Standalone mode does not retry unconfirmed accounts this way, because each attempt would be a provider request.

### Restarting in quota-cache mode

Runtime state is rebuilt after a CPA restart, so every account starts unconfirmed. Quota-cache mode is `use-quota-cache: true` (see the [README](README.md#optional-shared-quota-cache)). The startup pass reads the snapshot once. An account whose entry is missing, older than 30 minutes, or failed ranks last for now, using the stable filename/ID tie-break, and that provisional order is written. The schedule above writes the corrected order once Quota Cache has fresh data for every account, usually within the first few minutes, so the schedule writes each account at most once. Quota Cache often refills its entries a few at a time, and the partial rankings in between are never written. If an account is still unconfirmed at the `+15m` step, that step writes the order of the confirmed accounts, with the rest last, and the rest wait for the next `reconcile-interval` pass.

Codex can lazily continue reporting an expired window. v0.1.0 keeps the account in `awaiting_new_window` and performs passive retries; it does not send a quota-consuming activation request. `codex-reset-window-activation: true` is accepted but ignored with a status warning.

## Requirements and platform support

- A current plugin-capable CLIProxyAPI build with native plugin ABI v1 support
- Persistent access to CPA's plugin directory
- Physical Claude and/or Codex OAuth auth files exposed by CPA host callbacks (`host.auth.list`, `host.auth.get`, `host.auth.get_runtime`, `host.auth.save`, `host.http.do`, `host.log`)
- For the optional API credit routing only: Quota Cache 0.2.0 or newer, and CPA v8.0.4 or newer (verified on v8.0.23); see [its requirements](#requirements), including what to do before turning Quota Cache off on a CPA older than v8.0.22
- Go 1.26.0 and a native C toolchain only when building from source

The v0.1.0 release workflow builds these five native targets:

| Platform | Asset suffix | Library |
| --- | --- | --- |
| Linux x86-64 | `linux_amd64` | `reset-priority.so` |
| Linux ARM64 | `linux_arm64` | `reset-priority.so` |
| macOS Intel | `darwin_amd64` | `reset-priority.dylib` |
| macOS Apple Silicon | `darwin_arm64` | `reset-priority.dylib` |
| Windows x86-64 | `windows_amd64` | `reset-priority.dll` |

Windows ARM64, FreeBSD, Linux architectures other than amd64/arm64, and portable/no-plugin CPA builds are not release targets in v0.1.0. The Linux amd64 and arm64 artifacts are compiled on matching-architecture GitHub runners inside pinned `manylinux2014` containers and must pass a `readelf` gate proving that every referenced GLIBC symbol version is `2.17` or older. macOS and Windows artifacts are built natively on matching hosted runners. Ordinary Go cross-compilation is not sufficient for these CGO `c-shared` libraries.

## Installation

For Docker Compose or Coolify, first persist `/CLIProxyAPI/plugins` with a named volume. Then install manually or from this repository's custom Plugin Store source.

- [Docker Compose and manual installation](docs/install-docker-compose.md)
- [Custom Plugin Store install, update, and uninstall](docs/custom-plugin-store.md)
- [Troubleshooting and upstream compatibility caveats](docs/troubleshooting.md)

Start with `dry-run: true`. Do not enable writes until every real Claude/Codex account and desired priority is correct.

A complete example is in [`config.example.yaml`](config.example.yaml). Merge it into the existing CPA configuration; do not replace unrelated auth, logging, model, management, session-affinity, or routing settings.

### Plugin load priority versus credential priority

These are different settings:

```yaml
plugins:
  configs:
    reset-priority:
      priority: 10
```

`plugins.configs.reset-priority.priority` is CPA's plugin registration/load-order priority. The plugin ignores it when ranking credentials.

With `route-api-credits` on, it also decides which scheduler CPA uses. CPA calls one scheduler plugin, the one with the highest `plugins.configs.<id>.priority`, then the smaller ID, and silently ignores any other plugin that declares one.

The credential priorities managed inside physical OAuth auth JSON are `100`, `200`, `300`, and so on, with `0` reserved for quarantine/recovery.

## Configuration reference

| Field | Default | Meaning |
| --- | ---: | --- |
| `enabled` | `false` | Enables the plugin runtime. |
| `priority` | host-owned | CPA plugin load/order priority; ignored by credential ranking. |
| `reconcile-interval` | `1h` | Full roster/provider reconciliation interval. `refresh-interval` is an alias. A short interval is cheap only with the quota cache, where each pass reads the local snapshot. Never shorten it in standalone mode, where every pass makes one provider request per account. |
| `request-timeout` | `10s` | Per-account provider quota-request timeout. |
| `priority-floor` | `100` | Lowest healthy priority; must be positive. |
| `priority-step` | `100` | Gap between healthy ranks; must be positive. |
| `quarantine-priority` | `0` | Disabled/reauth/recovering sentinel; must be non-negative and below the floor. |
| `manage-claude` | `true` | Manage physical Claude OAuth credentials. |
| `manage-codex` | `true` | Manage physical Codex OAuth credentials. |
| `providers` | — | Alias list containing `claude` and/or `codex`; explicit `manage-*` fields win. |
| `dry-run` | `false` | Computes, schedules, reports, and logs proposed writes without calling `host.auth.save`. Use `true` first. |
| `display-timezone` | `UTC` | IANA zone (for example `America/Los_Angeles`) or `local` used only to render timestamps on the HTML status view. Unknown names fall back to UTC with a status warning. The JSON status route always stays RFC3339 UTC. |
| `codex-reset-window-activation` | `false` | Phase-2 option; `true` is ignored with a warning in v0.1.0. |
| `route-api-credits` | `false` | Declare CPA's scheduler and route Claude API-key attempts by Console organization. Independent of `dry-run` and `manage-*`. See [Claude API credit routing](#claude-api-credit-routing-optional). |

Durations accept Go duration strings such as `1h` and `10s`, or bare YAML integers interpreted as seconds.

## Status and management routes

The plugin exposes:

| Route | Access | Behavior |
| --- | --- | --- |
| `GET /v0/management/plugins/reset-priority/status` | Authenticated CPA Management API | Sanitized JSON snapshot, plus `router`, the API credit router's [status](#status). |
| `GET /v0/management/plugins/reset-priority/status/html` | Authenticated CPA Management API | Browser HTML status view rendered from the same sanitized snapshot, with a Refresh now action and a Claude API credit routing section. |
| `POST /v0/management/plugins/reset-priority/refresh` | Authenticated CPA Management API plus `X-Reset-Priority-Refresh: 1` | Synchronous full reconciliation with explicit success, no-op, or error status. |
| `GET /v0/resource/plugins/reset-priority/status` | Unauthenticated resource route | Static readiness shell with no account-level data; never mutates state. |

Example with a placeholder management key:

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'

curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/status

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H "X-Reset-Priority-Refresh: 1" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/refresh
```

Refresh returns HTTP `200` with `status: ok` only after a complete pass, `409` with `status: no_op` when the engine is disabled/stopping, `503` with `status: error` when roster reconciliation is deferred, and `403` when the CSRF gate fails.

The HTML status view at `GET /v0/management/plugins/reset-priority/status/html` is part of the authenticated Management API and requires the same management authentication as the JSON route. It shows per-account rows keyed by the physical auth file name (or a redacted stable auth-index when no file name exists) with provider, reset timestamp/state, current and desired priority, refresh health and sanitized errors, next deadline/reconcile timers, and the dry-run/stopped state. Timestamps are rendered in the configured `display-timezone` as readable values such as `Tue Sep 1 2026 - 6:25:36 PM PDT`, with the exact RFC3339 UTC instant kept on each `<time>` element and shown on hover. It deliberately omits the snapshot account label/email fields, tokens, and raw provider errors; physical filenames can still be identifying and should be redacted in screenshots. Its Refresh now button makes a same-origin `POST .../refresh` request, adds the required `X-Reset-Priority-Refresh: 1` CSRF header, attaches `Authorization: Bearer` when a same-origin management-console key is recoverable from localStorage, and preserves the query string.

At the audited CPA revision, Management API authentication is header-only (`Authorization: Bearer ...` or `X-Management-Key`); a query parameter is not a management credential, and ordinary address-bar navigation cannot add that header. Open the HTML route only through a browser/profile or authenticated reverse proxy that supplies the management header to both the page GET and its same-origin refresh POST. A fronting proxy that injects ambient management authentication must enforce its own CSRF/origin policy for the entire Management API, must not synthesize `X-Reset-Priority-Refresh` on arbitrary inbound requests, and must pass browser `Sec-Fetch-Site` unchanged. The plugin requires the custom header and, when the browser sends fetch metadata, accepts it only when `Sec-Fetch-Site` is `same-origin` or `none`; it rejects `same-site`, `cross-site`, and malformed/empty metadata. Browsers attach fetch metadata only when CPA is reached over HTTPS or a loopback host; over plain HTTP on any other host they send none, so the route then accepts an `http://` `Origin` (the only origin a browser can post to a plain-HTTP server from, since mixed-content blocking keeps HTTPS pages away) and rejects `https://`, `null`, or malformed origins without metadata. Non-browser management clients may omit both `Origin` and fetch metadata but still need the custom header. Serve CPA over HTTPS to keep the stricter metadata check. This stricter same-origin check matters because the audited CPA CORS layer can approve a sibling site's preflight and custom header; it is still not a substitute for proxy-wide CSRF protection. Otherwise use the authenticated JSON route and `curl` commands above.

The browser resource is intentionally unauthenticated under CPA's resource-route model. The server response is a static readiness shell with no account labels, identifiers, timestamps, priorities, errors, configuration values, or actions:

```text
http://127.0.0.1:8317/v0/resource/plugins/reset-priority/status
```

The shell also carries a client-side upgrade so the page is useful from the management-console sidebar (the console iframes resource pages from the CPA origin). Its inline script recovers the management key that the official management console (Cli-Proxy-API-Management-Center) persists in same-origin localStorage — the console's documented `enc::v1::` reversible obfuscation under `cli-proxy-auth` (or the legacy `managementKey` entry) — then fetches the authenticated `GET .../status/html` view over the same origin with `Authorization: Bearer` and swaps it in. This follows the officially documented trust model for same-origin plugin resource pages: the upgrade happens entirely in the operator's browser with credentials that browser already holds (a remembered console session, ambient reverse-proxy auth, or cookies), and the key is only ever sent to same-origin CPA management routes. When the console runs on a different origin, or no key is remembered (`Remember password` off) and no ambient auth exists, the fetch fails closed and the static shell stays up with a short explanatory note. The server still never authenticates or personalizes the resource response.

No route or log includes access tokens, refresh tokens, auth headers, raw auth JSON, or provider response bodies.

A panic in work the plugin runs in the background (a reconciliation, an account's quota read, a retry or deadline timer, or the worker behind a provider request) is caught instead of ending CPA. That piece of work is abandoned and the log says `reset-priority: recovered from an internal error in background work`, without the panic's value; a provider request that panicked gives the account the error `host callback host.http.do: internal error`. Reset Priority 0.2.0 and older end the CPA process instead. See [troubleshooting](docs/troubleshooting.md#logs-report-reset-priority-recovered-from-an-internal-error-in-background-work).

## Write safety

`host.auth.save` replaces a complete auth JSON document; it is not a field patch and has no compare-and-swap operation. Before every write, the plugin re-reads the latest physical document, changes only top-level `priority`, preserves unrelated JSON values, skips no-op writes, and continues after per-account failures.

The plugin never calls `host.auth.save` in dry-run.

Quarantined credentials **are** written, once, to persist the sentinel `0`. This is a deliberate tradeoff. At the audited revision the save/upsert path rebuilds the runtime record as `Status=active` with `Disabled` unset and does not read a `disabled` key from the document, so any save transiently reactivates the runtime record. A sentinel that lives only in plugin status demotes nothing, because CPA's selector reads priority from the auth record — so the write is what actually removes a dead credential from competition. The exposure is bounded by the no-op-skip rule (one write per quarantine, never per reconciliation), by verbatim preservation of every other field including any `disabled` key, by the sentinel priority itself putting the credential last in selection order, by requiring post-write refresh/file evidence before accepting recovery, and by CPA re-quarantining a genuinely broken credential on next use. See [troubleshooting](docs/troubleshooting.md).

## Current upstream compatibility caveats

The ABI and auth behavior were audited against CLIProxyAPI commit `81e1b5374f99c212f196f34956eeed964a46b8fa` (nearest release `v7.2.146`). `scheduler.pick`, which the optional API credit router answers, was verified at CPA v8.0.23 (`d318bcc3`). Revalidate after upgrading CPA.

1. **Runtime selector refresh is not synchronously guaranteed by `host.auth.save`.** At the audited revision, selection reads priority from the runtime auth `Attributes["priority"]`, while the immediate `host.auth.save` rebuild/upsert path places the file's priority in metadata without synthesizing that selector attribute. The physical file is updated, but an active selector may not observe the new priority until CPA's auth watcher resynthesizes the file or CPA restarts. If status looks correct but routing does not, restart/reload CPA and see [troubleshooting](docs/troubleshooting.md).
2. **Session affinity can delay visible priority changes.** At the audited revision, an already-bound, still-available session remains on its bound credential even when a higher-priority credential is available; priority applies to new sessions, requests without a session, and failover. Older CPA revisions had a reported priority-prefilter interaction that could replace affinity bindings. Do not assume priority changes migrate existing sessions; validate with new session IDs on the deployed CPA version.
3. **The plugin does not implement request-level routing for OAuth credentials.** It writes their priorities and relies on CPA for quota exhaustion, cooldowns, 429 handling, retries, and fallthrough. The optional API credit router answers CPA's scheduler only for attempts whose every candidate is a Claude API key from CPA's configuration, and leaves the rest, and all failover, to CPA.
4. **A same-path native unload/reinstall requires a CPA restart.** Once a resident CPA process has run the plugin's terminal native shutdown, the library's Go runtime state in that process is permanently terminal. A later `cliproxy_plugin_init` for the same library path is refused (nonzero) instead of reinitializing terminal state. Restart CPA to load a reinstalled or updated library.
5. **Terminal shutdown waits unboundedly for host callbacks already in flight.** ABI v1 host callbacks carry no direct native cancellation, so shutdown must drain an entered callback before the host may release the host API table; abandoning it on a timer would risk use-after-free during unload. Provider callbacks initiated by Management Refresh carry CPA's `host_callback_id`, giving the host the originating request context when that ID is still registered. A worker that observes cancellation before native dispatch skips the callback. ABI v1 provides no acknowledgement after CPA resolves the ID, however, so a narrow dispatch-to-host-entry race remains: if the management request retires first, the audited host falls back to a background context. Separately, the plugin caps admitted native host callbacks at 64 process-wide; saturation fails new callbacks with `host callback limit reached` instead of accumulating unbounded blocked cgo calls. A callback that still never returns can keep unload blocked until CPA exits. There is no safe plugin-side bounded drain or complete callback-context lease at this ABI; see [troubleshooting](docs/troubleshooting.md).
6. **`host.auth.save` transiently reactivates the runtime auth record.** At the audited revision the save-side rebuild sets `Status=active`, leaves `Disabled`/`Unavailable` unset, and does not read a `disabled` key from the supplied document; it carries over only `CreatedAt`, `LastRefreshedAt`, `NextRetryAfter`, and `Runtime`. This affects any caller of the callback. The plugin accepts it in order to persist the quarantine sentinel, bounds it to one write per quarantine, and does not accept the resulting active record as recovery unless a later refresh/file timestamp proves a post-write change; see [Write safety](#write-safety).
7. **`host.auth.get_runtime` returns an error rather than an entry for some healthy-looking cases.** At the audited revision it errors on an unknown auth index, on a runtime-only auth that is disabled, and on a disabled or management-removed file-backed auth whose file no longer exists. The health probe therefore treats every probe error as "no observation" and defers to roster reconciliation, never as a health transition.
8. **Browser management authentication remains header-only, and the mutating route is CSRF-gated.** CPA accepts `Authorization: Bearer ...` or `X-Management-Key`; it does not authenticate a management route from its query string. The HTML status route and Refresh action therefore require a browser/profile or authenticated reverse proxy that supplies the management header to both requests. Refresh additionally requires `X-Reset-Priority-Refresh: 1`; when fetch metadata is present only `Sec-Fetch-Site: same-origin` or `none` is accepted, while `same-site`, `cross-site`, and invalid/empty metadata are rejected. Over plain HTTP on a non-loopback host browsers send no fetch metadata, so an `http://` `Origin` is accepted there and `https://`, `null`, or malformed origins are rejected. Header-based non-browser clients may omit browser metadata. This closes the audited sibling-site CORS/preflight path, but an authentication-injecting proxy must still enforce CSRF/origin protection across the complete Management API and must not add the plugin CSRF header to arbitrary requests.

## Build and validation

```bash
make fmt-check
make vet
make test
make race
make build
make check-linux-glibc  # Linux only; package runs this automatically on Linux
make package
make check-release
bash -n scripts/smoke-test.sh
make bench  # the API credit router's per-pick cost; reported, never a gate
```

On Linux, `make build` creates `reset-priority.so`. The smoke test uses a disposable container from the digest-pinned CPA v8.0.4 image (override with `CPA_SMOKE_IMAGE`), a temporary config in the v8 layout (`CPA_SMOKE_LAYOUT=legacy` uses the legacy layout), and a disposable named plugin volume:

```bash
./scripts/smoke-test.sh ./reset-priority.so
```

It performs no real OAuth calls and keeps evidence under `dist/smoke/<run-id>/`. Real-account validation must remain dry-run-first.

`make smoke` (needs docker) builds the library and runs the repository's `scripts/reset-priority-smoke.py`, which loads it into a disposable CPA container with synthetic keys and Quota Cache files and drives the API credit router: routing on, an explored pick, and picks stopping once routing is off. Nothing reaches a provider.

## Operator acceptance checklist

- [ ] `/CLIProxyAPI/plugins` is backed by a persistent named volume.
- [ ] The correct native library loads after a CPA restart.
- [ ] Claude physical OAuth accounts are discovered.
- [ ] Codex physical OAuth accounts are discovered.
- [ ] Dry-run computes the expected `100`-point hard priorities.
- [ ] Only Claude `seven_day.resets_at` affects Claude ranking.
- [ ] Only the exact `604800`-second Codex window affects Codex ranking.
- [ ] Utilization, five-hour, model-scoped, monthly, additional, code-review, and credit signals do not change OAuth ordering.
- [ ] A one-account provider group remains at `100`.
- [ ] Adding a second healthy account automatically yields `200 / 100`.
- [ ] The next exact reset deadline appears in status.
- [ ] Quota/429/cooldown/unavailable states do not cause quarantine by themselves.
- [ ] A disabled or definitive reauth-required auth is outside the healthy count at `0`, and (outside dry-run) its physical auth JSON carries `"priority": 0`.
- [ ] A steady quarantined account produces no repeated writes across several reconciliations.
- [ ] CPA self-recovery of a quarantined credential is reflected in status within about a minute.
- [ ] A recovered auth remains at `0` until a fresh post-recovery future weekly reset is confirmed.
- [ ] A deadline event demotes locally before a provider network refresh succeeds.
- [ ] `dry-run: false` is enabled only after all desired priorities are verified.
- [ ] New sessions route according to updated priorities after any required CPA reload/restart.
- [ ] Existing session-affinity behavior is understood and tested separately.
- [ ] CPA restart/redeploy retains the installed plugin.
- [ ] With `route-api-credits` on, the status shows `router.active` as `true`, and `router.counters.picks` moves with credits traffic.

## Claude API credit routing (optional)

This section covers `route-api-credits`. The README's [first-use steps](README.md#spend-the-credit-that-refills-soonest-optional) say how to turn it on.

### What it does

With `route-api-credits: true`, Reset Priority declares CPA's `scheduler` capability, and CPA asks it which credential to use for every request attempt. Reset Priority answers only when **every** candidate CPA offers for that attempt is a Claude API key from CPA's own configuration (`api-keys.claude`). It then picks a key of the Console organization whose monthly API credit refills soonest and still has credit left, so that credit is spent before it expires.

For every other attempt it answers `handled: false`, and CPA's own selector chooses over the same candidates. That covers subscriptions (OAuth credentials), other providers, a mix of kinds, and anything Reset Priority is unsure of. It never refuses a request and never hands the choice to a named built-in scheduler. The router never writes a priority and never calls CPA, so OAuth priorities work exactly as they do with routing off.

**Scope is decided per attempt.** For each attempt CPA offers the scheduler only the highest available priority tier of the credentials it has not tried yet. What reaches the credits keys depends on how they are set up:

- **Model aliases, recommended.** The credits keys sit in an `api-keys.claude` group with no `prefix`, and a `models:` list maps each Claude model they serve to an alias such as `credits-claude-sonnet-5-5`, the same on every credits key and on no other credential. A key with a `models:` list serves only the names it lists, so the credits keys serve only the aliases and nothing else serves them. Every attempt for an alias offers only credits keys, so the router chooses for all of them, and plain Claude model names never reach the credits keys, whatever `force-model-prefix` says. Clients that should spend the credit ask for the aliases.
- **A prefix with `force-model-prefix: true`.** With the group's `prefix` set, such as `credits`, and CPA's `routing.force-model-prefix: true`, only `credits/…` requests reach the credits keys, so only they are routed. The setting is global: every other prefixed credential then serves only its prefixed names too.
- **Neither.** A credits key with a prefix and `force-model-prefix: false`, CPA's default, or with no prefix and no `models:` list, also serves the plain Claude model names, so where a plain request goes depends on priorities. A subscription credential and a config key both default to priority 0, which puts them in the same tier. Reset Priority steps aside for that mixed attempt, but CPA's own selector then chooses among all of them, credits keys included, even while every subscription is healthy. Healthy subscriptions that Reset Priority manages have priority 100 and up, so a plain request reaches the credits keys only once every subscription credential above them is cooled or blocked for that model. A quarantined or recovering subscription credential sits at `quarantine-priority`, 0 by default, the same as the credits keys, so while CPA offers one the attempt is mixed and stepped aside (`out_of_scope`). When only API keys are left in the tier, Reset Priority chooses. Interactive Claude Code on a plain model name can then reach the credits keys, which the monthly credit does not cover; see [Failover](#failover).

**Off, nothing changes.** The capability is not declared, and CPA routes exactly as it does without the setting. Reset Priority still rebuilds its routing table every minute, so the status shows whether turning routing on would route.

### Configuration

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `route-api-credits` | boolean | `false` | Declare the scheduler and route Claude API-key attempts by organization. |

On CPA's Plugins page, open Reset Priority's **Edit config** to set it, or set it in `config.yaml`. Absent and `null` mean off. Anything that is not a boolean fails the configuration, and CPA then drops the plugin until a good configuration applies, which also leaves every request to CPA. A change applies on the next configuration reload, without a restart, in both directions. With `enabled: false` the scheduler is never declared, whatever this says. Routing is independent of `dry-run`, `manage-claude` and `manage-codex`: `dry-run` only stops priority writes. Reset Priority 0.1.8 and older ignore the key.

The router reads Quota Cache's files beside `quota-cache-path`, or beside Quota Cache's default `plugins/data/quota-cache/snapshot.json` when that is unset, whatever `use-quota-cache` says. So `use-quota-cache: false` with a custom `quota-cache-path` keeps the OAuth engine standalone while the router reads Quota Cache. A bare `quota-cache-path`, without `use-quota-cache`, switches the OAuth engine to cache mode as it always has. The form on CPA's Plugins page shows an unset `use-quota-cache` as off and saves only the fields you change, so to save it as false there, switch it on and off again before you **Save**.

The rule that makes a refused request move to the next key is CPA's configuration, not a plugin setting; see [Failover](#failover).

### Requirements

- Quota Cache **0.2.0 or newer**, with the Console organizations, their monthly credits and refill dates set on its page. Quota Cache works out what each organization has left and publishes it in `snapshot.credits.json` beside its snapshot, and its meter records which CPA auth ID each Claude API key from CPA's configuration uses, and whether its latest attempts succeeded. Against an older Quota Cache the router stays inactive with `credits_missing`.
- CPA v8.0.4 or newer. Verified on v8.0.23. Before v8.0.22, CPA does not stop a plugin that is turned off on its Plugins page, or with `enabled: false`, until CPA restarts: it only stops sending it usage records. Quota Cache then keeps saving its meter and publishing a fresh credit state that no longer counts spend, so the router never steps aside. It keeps routing on spend frozen at that moment, and an organization whose credit runs out is never marked `out`, because its refusals are no longer counted. On those versions, turn `route-api-credits` off before turning Quota Cache off. Turning Quota Cache back on records no gap, so its estimate leaves out the spend in between without saying so: enter a Console reading for each organization on its page before turning routing back on. Removing Quota Cache stops it on every version.
- CPA not in Home mode: started without `-home-jwt` and without the `HOME_JWT` environment variable. Home mode bypasses plugin schedulers. It has no setting in `config.yaml`.
- Reset Priority the only scheduler. CPA uses one scheduler plugin, the one with the highest `plugins.configs.<id>.priority` (then the smaller ID), and silently ignores the rest.
- The credits keys set up as [What it does](#what-it-does) describes, before the [bootstrap](#new-and-unproven-keys): model aliases, or a `prefix` with `force-model-prefix: true`. Adding, changing or removing a key's prefix gives it a new auth ID, which counts only once that key serves again, while the old ID keeps counting for 7 days after its last request. A `models:` list is not part of the auth ID.
- Every credits key at the same `priority` (the default, 0). A lower-priority credits key is never offered while a higher tier has a key available.
- `routing.retry.max-retry-credentials` at 0 (try every credential, CPA's default) or at least 3. With 1, a refused or explored attempt is the request's only one. With 2, an exploration followed by a dry organization that is not yet marked out can use both.

### How it chooses

Each candidate key is followed to an organization: its auth ID to the auth index Quota Cache saw with it, then that auth index to the organization Anthropic's responses named. The organization's state is its account in the credit state Quota Cache last published, as the router's last rebuild read it: the same row Quota Glance shows on its Monthly API Credit card.

| The account in the credit state | For the router |
| --- | --- |
| `ok`, with an estimate and credit left | Eligible (`""`) |
| The same, after a refusal in a Claude Code-based session (`claudeCodeRefused`) | Eligible, demoted, re-checked (`claude_code_refused`): ranked after every other eligible one. A later success, a newer Console reading or the next refill clears it |
| `ok` with no estimate | `no_estimate` |
| `ok` with nothing left | `no_credit_left`. Never re-checked: a success there could be billed to purchased credit. Within the cycle only a change to its settings, such as a new Console reading, clears it; the next refill clears it on its own |
| `out`, when its estimate still had credit left | `out`, re-checked |
| `out` otherwise | `out` |
| `stale`, `needsSettings`, `pending`, `misconfigured`, `cacheTooOld` | `stale`, `needs_settings`, `pending`, `misconfigured`, `cache_too_old` |
| Any other state (none today) | `unknown_state` |

The code in the second column is the organization's `reason` in the status's `router.orgs`. Only the first two rows are eligible.

**Ranking.** Eligible organizations rank by refill date, soonest first. One with no refill date comes after every one with a date. Ties go to the order Quota Cache lists them in, then the account ID. An organization demoted for a Claude Code refusal (`claude_code_refused`) comes after every other eligible one.

**Only proven keys are ranked.** A key is proven once it has succeeded through CPA, and stays proven until it fails twice with no success between. A key that has never succeeded, or has failed twice in a row, is `failing`: it is explored (below) but never the ranked choice. This keeps a key that fails every request with a 400 CPA does not fail over, such as a missing workspace ID, from being ranked first. One client-caused 400 between successes does not demote a key.

**Within one organization** (a Team pool can have several keys), the smallest proven auth ID wins. The same key every time keeps that organization's prompt cache warm.

**After a failed attempt.** CPA picks again within the same request, and that pick names the attempt that failed. Most such failures are about the whole organization: a low-credit refusal, a rate limit, an overload. CPA's tried set and cooldowns are per key, though. So the failed attempt's organization ranks after every other eligible one for that pick only, and its other keys are still tried if every other organization fails too.

**At a refill.** Quota Cache's credit state says until when its figures hold (`valid_until`), the next refill boundary of any organization. From then on the router declines `credits_stale` until its next rebuild reads Quota Cache's new state, within a minute, so it never ranks on last cycle's figures.

**An example.** Six organizations at noon on October 11. alpha refills on the 13th but is `out`, with nothing left on its estimate either, so it is not re-checked; bravo refills on the 21st with $40 left; charlie refills on the 26th with nothing left; delta refills on November 1st with $120; echo, a Team pool with two keys, on November 10th with $300; foxtrot has no refill date and $25. A request offered every key, where every attempt fails over, goes bravo, delta, echo's smaller key, foxtrot, then echo's other key, and finally back to CPA's selector, with only keys the router never picks left: alpha's, charlie's, and one whose responses never name an organization. Had alpha's estimate still shown credit, that last pick would re-check alpha's key instead, once per wait. A key Quota Cache has not linked to an organization yet is explored first, once.

### When it steps aside

Every decline hands the attempt to CPA's own selector. The reason is counted in the status's `router.counters.declined`, and the last one is `router.last_decision`.

| Reason | Meaning | Fix |
| --- | --- | --- |
| `disabled` | `route-api-credits` is off, the plugin is disabled, or it shut down | Turn it on |
| `no_table` | No table yet: the router's first rebuild after a configuration apply has not run, the plugin was quiesced, or the last build failed (`table_reason` is then `build_failed`) | Wait a minute; for `build_failed`, see Reset Priority's log and report it |
| `credits_missing` | No credit state beside Quota Cache's snapshot: Quota Cache is older than 0.2.0, it has not published yet, or `quota-cache-path` does not match Quota Cache's `cache-path` | Update Quota Cache to 0.2.0 or newer, or fix `quota-cache-path` |
| `credits_unreadable` | The credit state file cannot be read | See Quota Cache's log; while it runs it rewrites the file at least once a minute |
| `credits_stale` | The credit state is more than 3 minutes old, or its `valid_until` has come. Quota Cache publishes at least once a minute while it runs, and again at each refill boundary, so it is disabled, stopped or stuck. On a CPA older than v8.0.22, Quota Cache keeps publishing after it is turned off, so this never comes; see [Requirements](#requirements) | Enable Quota Cache, or see its log. At a refill boundary this clears on its own at the next rebuild |
| `settings_unavailable` | Quota Cache cannot read its settings file, `snapshot.settings.json`, so none of the values entered on its page apply and its figures could be wrong | Put back a good copy of the file, or move it aside and enter the values on Quota Cache's page again. See [When a file cannot be read](../quota-cache/docs/credit-settings.md#when-a-file-cannot-be-read) |
| `meter_missing` | No meter file beside the snapshot | Add a Console organization on Quota Cache's page, or wait for its first save |
| `meter_unreadable` | The meter file cannot be read | See Quota Cache's log |
| `meter_stopped` | Quota Cache's meter is stopped. The meter file's `stop_reason`, which Quota Cache's page shows under **Claude API meter**, says why: `shutdown` (Quota Cache was turned off or removed on CPA's Plugins page, or CPA is stopping), `quiesce` (CPA is replacing Quota Cache, as in an update), `disabled` (Quota Cache rejected its configuration), `no_items` (Quota Cache stopped listing any organization while its meter ran, as when the last one is removed on its page or its organizations file can no longer be read), or `failed` (an internal error). Quota Cache records a stop only when it stops a running meter, and starts none while it lists no organization, so the file then keeps the stop the last run recorded: right after updating from Quota Cache 0.1.14, or after a restart with an organizations file it cannot read, that is `quiesce` or `shutdown`, not `no_items`. On a CPA older than v8.0.22, turning Quota Cache off does not stop its meter, so this never comes; see [Requirements](#requirements) | Turn Quota Cache back on, fix its configuration, or add an organization on its page. A `shutdown` or `quiesce` that stays while Quota Cache runs means its meter has not started since CPA loaded it: it lists no organization yet, or it has rejected a configuration since. Add the organizations on its page; if its page says it cannot read its organizations file, see [When a file cannot be read](../quota-cache/docs/credit-settings.md#when-a-file-cannot-be-read). After `failed`, see Quota Cache's log; its next configuration apply starts the meter again. A quiesce clears on its own once CPA loads Quota Cache again, provided it lists an organization |
| `meter_too_old` | The meter file does not record auth IDs: no Quota Cache 0.2.0 or newer has run its meter on it yet | Update Quota Cache. If it is new enough already, its meter starts only while Quota Cache is enabled and lists at least one organization |
| `meter_stale` | The meter has not been saved for 15 minutes | Check that Quota Cache is running |
| `no_api_credits` | Quota Cache lists no Claude Console organization | Add one on Quota Cache's page |
| `no_linked_orgs` | No organization Quota Cache lists has an Organization ID | Add the organizations on Quota Cache's page, under **Organizations**, each with its Organization ID |
| `table_stale` | The table is more than 3 minutes old: the router loop stopped or a rebuild is stuck | See Reset Priority's log; a configuration apply restarts the loop |
| `out_of_scope` | A candidate is not a Claude API key from CPA's configuration | Nothing; this is CPA's choice to make |
| `invalid_request` | The request could not be read, or offered no candidate | Report it |
| `unmapped` | No offered key is linked to an organization yet, and none can be explored now | Let it learn; see [New and unproven keys](#new-and-unproven-keys) |
| `no_proven` | Offered keys belong to eligible organizations, but none is proven, and none can be explored now | The same |
| `no_eligible` | No offered key belongs to an eligible organization. Each belongs to one that is not eligible (see [How it chooses](#how-it-chooses); an `out` one between re-checks included), or is `foreign` or `unlinked` (see [New and unproven keys](#new-and-unproven-keys)) | Nothing. An organization that is not eligible comes back once its credit refills or a reading changes. `foreign` and `unlinked` keys are always CPA's to choose, so a request offered only those, such as one for another `api-keys.claude` group, counts here every time while the credits organizations still have credit |
| `panic` | An internal error, caught | Report it |

An `out` organization whose estimate still had credit left is re-checked with the same backoff as an exploration: one request per wait goes to it, ahead of the ranked choice, on the key the ranked choice would take, its smallest proven key. A success clears `out` at Quota Cache's next publish and the router's next rebuild; a refusal backs it off. That keeps a premature `out` from stranding the rest of the cycle's credit. A proven key that then fails twice in a row, such as one whose workspace has hit a spend limit, stops being the one re-checked from the next rebuild. While the organization offers no proven key, each of its keys is re-checked in turn, each with its own wait, so a key that fails every request cannot keep it `out`. `no_credit_left` is never re-checked. Within the cycle only a change to the organization's settings, such as a new Console reading, clears it; the next refill clears it on its own, because what is left is then the new cycle's.

### New and unproven keys

Three kinds of key are **explored**: one not yet linked to an organization (`unmapped`), one linked to an eligible organization but not proven (`failing`), and a key of an organization being re-checked (`recheck`): its smallest proven key, or each of its keys in turn while it offers no proven one (see [When it steps aside](#when-it-steps-aside)). Each is picked at most once per wait, ahead of the ranked choice. On a pick that follows a failed attempt it is explored only when no ranked key is offered, so a request spends at most one attempt on exploration while a ranked key is available.

The wait backs off from a 10-minute base, and each exploration that taught nothing doubles it before the next one. So after the first exploration of a key that taught nothing, the next comes 20 minutes later, then 40, 80, 160 and 320 minutes later, then every 6 hours. "Taught nothing" means the attempt reached Quota Cache's meter, or failed within its request, and the key is still not settled: it is still unlinked or unproven, or, for a re-check, its organization has not been cleared yet (below). Reset Priority judges an exploration 3 to 4 minutes after it. Until then the status still shows the wait from before the exploration (10 minutes for a key's first one), and the doubled one after. An attempt that produced no usage record at all is given one quick second try 3 minutes later before the wait doubles.

A key's wait starts over only once it is proven or found to be `foreign` or `unlinked`, once a re-checked organization is eligible again and no longer demoted, or after 7 days without an exploration. An organization that is ineligible for a while, such as `stale` or `out`, does not start it over: its keys stay in `exploring` with their step, and are explored again once the wait has passed and the organization is eligible, or due a re-check, again.

So a key that never becomes proven is picked at most 8 times in its first 24 hours and 4 times a day after that, however often its organization's state changes, or 16 and 8 if none of those attempts produces a usage record. What each costs the client:

- A credential failure (429, 5xx, a connection error, 401, 402 or 403), or a low-credit 400 with the [failover rule](#failover), costs one attempt that CPA moves on from. The client does not see it while `max-retry-credentials` is 0 or at least 2.
- A 400 CPA does not fail over, such as a missing workspace ID, a spend limit, or a low-credit 400 without the rule, costs one failed client request each time. The optional rules in [Failover](#failover) turn those into failovers.

**Bootstrap.** The cheapest start is to leave routing off for a while after installing Quota Cache 0.2.0. CPA's own routing then gives the keys traffic, and the meter learns each key's organization and outcomes whether routing is on or not. A key CPA has not sent a request to yet is in none of the status's counts, so `table_reason` `""` with `unmapped_keys` and `failing_keys` at 0 is already true at the meter's first save.

Every key is known once, in addition, `mapped_keys`, `foreign_keys` and `unlinked_keys` add up to the number of Claude API keys in CPA's configuration: every key of every `api-keys.claude` group, whatever models it serves, not only the credits group. Quota Cache learns each of them from its first request, so a key pointed at another service counts as `foreign` once it has succeeded, and a key of an organization Quota Cache does not list as `unlinked`. A key listed in two groups has two auth IDs and counts twice. Comparing with the credits group alone can look complete while a credits key is still unknown, because another group's key stands in for it. A key outside the credits group that never serves keeps the sum short; the check then cannot confirm every credits key, and one still unknown is explored once routing is on.

A key that fails every request never brings its count to 0. If its failures never name an organization, such as a key pointed at a gateway that errors, it stays in `unmapped_keys`. If they do but it never succeeds, such as a personal key sent without an `anthropic-workspace-id` header, it stays in `failing_keys` while its organization is eligible, and in that organization's `failing_keys`. Neither ages out while CPA keeps sending it requests. Fix such a key, for example with that header in its `headers`, or remove it from CPA's configuration, before turning routing on. Otherwise it is explored once routing is on, at the costs above. The optional rules in [Failover](#failover) make a missing workspace ID or a spend limit move on to the next key, but do not make the key succeed.

The counts cover keys seen in the last 7 days. A key idle for longer counts only in `stale_keys`, though the router still knows it. Changing a key's key, prefix, base URL, proxy or headers gives it a new auth ID, which counts only once it serves; the old ID keeps counting for 7 days after its last request, then moves to `stale_keys`. So does the ID of a key you remove. Until then the check above cannot confirm the change. A changed key's old ID stands in for the new one until the new one serves, so the sum still matches while the new ID is unknown, and that ID is explored once routing is on. Once it serves, the sum is one over. A removed key's old ID makes it one over at once. An old ID that was not proven can also still hold `unmapped_keys` or `failing_keys` above 0. CPA no longer offers an old ID, so it is never explored. That is why the credits keys are set up before the bootstrap (see [Requirements](#requirements)).

Under `fill-first`, CPA's selector keeps choosing the same first key, so the others may never be learned that way. Either switch `routing.strategy` to `round-robin` until every key is counted, or expect each remaining key to be explored, at the costs above, once routing is on.

**count_tokens.** CPA asks the scheduler for token-count attempts too, and Reset Priority cannot tell them from messages. They send no usage record, so they teach the meter nothing. A client that sends mostly token counts to a new key delays its learning by tens of minutes.

Keys whose responses never name an organization (`foreign`, not Anthropic's) and keys of an organization Quota Cache does not list (`unlinked`) are never explored or picked by the router. CPA can still choose them when the router steps aside.

### Failover

Failover is CPA's job. Add this rule to the `api-keys.claude` group that holds the organizations' keys; each key inherits it:

```yaml
api-keys:
  claude:
    - name: credits
      models:
        - name: claude-sonnet-5-5
          alias: credits-claude-sonnet-5-5
      request-scoped-errors:
        - status: 400
          match: ["credit balance is too low"]
          # optional, survives a capitalization change:
          # match-regexr: ["(?i)credit balance is too low"]
          action: continue-and-cooldown
      keys:
        - api-key: "<organization 1 key>"
        - api-key: "<organization 2 key>"
```

In the console the rule is set on each key instead: AI Providers → Claude → the key's **Edit** → **Advanced runtime policy** → **Request error rules**: **Override**, then **Add rule** with **Status code** `400`, **Match type** **Contains text**, **Condition** `credit balance is too low` and **Action** **Continue and cool down**, and **Save**. **Override** on a key replaces the group's list for that key; **Inherit** keeps the group's.

`match` is a case-sensitive substring test on Anthropic's response body. Without the rule, a request refused for low credit returns the 400 to the client. That happens when the estimate was optimistic, or when an exploration or a re-check reached a dry organization. With the rule, CPA cools that key for that model for about a minute and picks again, and the next pick ranks the refused key's organization last. So a dry organization costs one refused attempt per request. Each further request within that minute costs one more per remaining key of that organization, until the meter marks it `out`.

**The lag.** A refusal reaches the router after Quota Cache saves its meter (within a minute) and publishes the credit state (within 10 seconds), and the router's next rebuild (within a minute), so about 1 to 2 minutes. Until then the dry organization stays ranked.

**Optional rules.** CPA returns some 400s to the client on every request instead of moving on: a personal key used without `anthropic-workspace-id`, and a spend limit set on an organization or workspace. If one of your keys can hit them, add to the same list:

```yaml
        - status: 400
          match: ["anthropic-workspace-id is required"]
          action: continue-and-cooldown
        - status: 400
          match: ["You have reached your specified"]   # org and workspace spend limits
          action: continue-and-cooldown
```

**Claude Code.** The monthly credit does not cover interactive Claude Code. If Anthropic answers an uncovered Claude Code request with the same low-credit text, which is likely but undocumented, the rule makes one Claude Code turn walk every credits key and cool each for that model for about a minute. Covered clients on that model then have no credits key meanwhile. So never point interactive Claude Code at the credits keys' names. Give these keys no `cloak` block, or one with `mode: never`, and no `fingerprint-profile: claude-code-cli`: a `cloak` block in any other mode (`auto`, the default, included) or that profile makes CPA send every request on them as Claude Code, whichever client sent it. `claude -p` and the Agent SDK are covered.

With model aliases, interactive Claude Code reaches the credits keys only if it asks for an alias. With a prefix, it reaches them through the plain names too unless `force-model-prefix` is `true`; see [What it does](#what-it-does). It does not matter whether Reset Priority or CPA's own selector chooses the key.

**Rate limits.** Routing sends every in-scope attempt to one organization, so its per-model rate limits carry all of it. An individual organization new to the API may also be on Anthropic's lower Evaluation tier. A 429 fails over and cools that key, and the next pick ranks the organization last. Two failures in a row make the key unproven until an exploration succeeds, which spreads the load, at the cost of one failed-over attempt each time.

### What turning it on changes for other traffic

CPA has no per-provider scheduler registration. Once any plugin declares a scheduler, CPA changes how it picks for every request, including the ones the router never chooses for, OAuth subscriptions among them:

1. Every attempt takes CPA's legacy selector path instead of its scheduler fast path. Candidate filtering and priority tiers are the same, and so is the configured strategy within one provider. Built-in rotation state, such as the round-robin cursor, is separate and starts fresh.
2. For a model served by several providers, `fill-first` and `round-robin` choose across providers differently: by the smallest ID over one list sorted by ID, instead of in provider order or with a cursor per provider list. So turning routing on can change which provider `fill-first` prefers for such a model, though the router never chooses for that request.
3. A downstream websocket request to Codex or xAI no longer prefers websocket-enabled credentials.
4. Each attempt costs CPA one JSON encoding of the request, headers and candidates included, and one call into Reset Priority. Reset Priority declines a subscription request in well under a microsecond.
5. If CPA already runs a selector that is not built in, such as session affinity, it is on the legacy path already, and only item 4 applies. Session affinity applies only when the router steps aside. When it routes, every session gets the same organization until that organization's state changes.
6. All of this applies while the setting is on, even while the router cannot route at all, for example with Quota Cache older than 0.2.0 (`credits_missing`). That is why the README says to check the status first. When the scheduler has been declared and the router has been unable to route for 30 minutes for a reason that needs you (`credits_missing`, `credits_unreadable`, `credits_stale`, `settings_unavailable`, `meter_missing`, `meter_stopped`, `meter_too_old`, `no_api_credits` or `no_linked_orgs`), Reset Priority logs one warning, once per configuration apply. With Quota Cache disabled, its last credit state goes `credits_stale` 3 minutes after it stopped publishing, and that is the reason the warning names. On a CPA older than v8.0.22, Quota Cache keeps publishing after it is turned off, so the router keeps routing and never warns; see [Requirements](#requirements).

### Status

`GET /v0/management/plugins/reset-priority/status` carries a `router` object, always, computed at the time of the request. The HTML status view shows the same in its **Claude API credit routing** section.

| Field | Meaning |
| --- | --- |
| `enabled` | `route-api-credits` is on and the plugin enabled |
| `declared` | What Reset Priority last declared to CPA (see below) |
| `active` | Enabled, and the table would route |
| `reason` | `disabled` when off, else `table_reason` |
| `table_reason` | Why the table would not route, whether routing is on or off: `build_failed`, `no_table`, a reason from [When it steps aside](#when-it-steps-aside), `table_stale`, `meter_stale` or `credits_stale`. `""` when it would route |
| `built_at`, `meter_flushed_at`, `auth_ids_since` | When the table was built, when Quota Cache last saved its meter, and since when the meter records auth IDs. `null` when unknown |
| `max_table_age_seconds`, `max_meter_age_seconds` | 180 and 900: the ages past which the router steps aside |
| `explore_every_seconds`, `max_explore_wait_seconds` | 600 and 21600: the base the exploration wait doubles from, and the longest wait. The first doubling comes before the second exploration, so that one comes 20 minutes after the first; see [New and unproven keys](#new-and-unproven-keys) |
| `orgs` | Linked organizations, eligible ones by rank, then the rest: `id`, `label`, `rank` (0 when not eligible), `eligible`, `demoted`, `recheck`, `reason` (from the table in [How it chooses](#how-it-chooses); `""` when eligible and not demoted), `renews_at`, `left_text`, `estimate_left_text` (what the estimate had left before `out`), `refusal`, `issue` (Quota Cache's sentence for the account), `keys` and `failing_keys`. `refusal` is the account's low-credit refusal, if any: `refused` or `refusedNearlySpent`, which mark it `out`, `claudeCodeRefused`, which demotes it while it is otherwise eligible, or `""` |
| `mapped_keys`, `unmapped_keys`, `failing_keys`, `unlinked_keys`, `foreign_keys` | Claude API keys from CPA's configuration seen in the last 7 days, in any `api-keys.claude` group, by what the router knows of them. `failing_keys` are counted in `mapped_keys` too |
| `stale_keys` | Keys not seen for 7 days: a key that has been idle, which the router still knows, or the old auth ID of a key you changed |
| `exploring` | Keys being explored, soonest next first: `config_index` (the key's position among every Claude API key in CPA's configuration, counted from 0 across all `api-keys.claude` groups in file order: after a one-key group, `"2"` is the credits group's second key), `kind` (`unmapped`, `failing` or `recheck`), `step` and `next_at` |
| `last_pick_at`, `last_routed_at`, `last_decision`, `last_routed_org` | The latest pick, the latest that named a key, the latest answer, and the account ID of the organization of the key that pick named. `last_routed_org` is `""` before the first such pick, and after an `unmapped` exploration, whose key is not linked to an organization yet |
| `counters` | Since CPA loaded this copy of the plugin: `picks`, `routed`, `explored`, `rechecked`, `demoted` (picks where ranking the failed attempt's organization last changed the answer), `build_panics`, and `declined` by reason, every reason listed |

**`counters.picks` stays at 0** while `declared` is `true` and credits traffic flows: CPA is not calling Reset Priority. Another plugin that declares a scheduler sorts ahead of it, or CPA runs in Home mode: it was started with `-home-jwt` or with the `HOME_JWT` environment variable set. Neither can be seen from the plugin.

**`declared` is Reset Priority's intent, not CPA's state.** For a moment after a configuration change the two can disagree in either direction. If `counters.picks` keeps rising while `declared` has been `false` for more than a minute, CPA is running a rolled-back old copy whose last good registration declared the scheduler. Those picks still step aside (`disabled` or `no_table`), and the next successful configuration apply ends it.

The log says `reset-priority API credit routing on` or `… off` when the setting changes, and `reset-priority API credit router active` or `reset-priority API credit router inactive (<reason>)` when routing starts or stops working. It warns `reset-priority API credit router could not be built` once per configuration apply when a rebuild fails, which leaves the router stepped aside with `build_failed` until a build succeeds, and `reset-priority API credit routing is on but cannot route (<reason>); …` when routing has been unable to route for 30 minutes (item 6 above). It never logs a pick, an ID, a header or an error's detail.

### Updating or removing Reset Priority

A pick that reaches Reset Priority while CPA is closing it, during an update, a disable or an uninstall, fails that client request. CPA calls the scheduler on every attempt, so before any of those, including an update that is only about OAuth priorities:

1. Set `route-api-credits: false`. That is a configuration change, which removes the capability without closing the plugin.
2. Wait until two consecutive requests leave the status's `router.counters.picks` unchanged.
3. Proceed, then turn routing back on afterwards.

### Renewal day

The ranking turns over at 00:00 UTC on an organization's refill date, while Anthropic deposits the new credit shortly after the plan payment. Under soonest-refill-first, the soonest organization carries all routed traffic in its last hours. A refill date that is off by a day either leaves credit to expire or keeps spending a balance that has already expired, which Anthropic refuses.

Set each refill date on Quota Cache's page, as **Refills on, UTC** under **Claude API credits**, to the day the linked claude.ai plan renews, as [Quota Cache's README](../quota-cache/README.md#claude-api-credits-optional) says. Editing `renews` in Quota Cache's configuration changes nothing: Quota Cache ignores `claude-api-credits`. Console's expiry date for the current credit (Settings, Billing, Promotional credits) may be the day before; if so, use the claude.ai date. A date one day early starts the next cycle a day early: on the old credit's last day the organization already reads as refilled, its refill date moves a month on, it drops behind the other organizations, and what it had left expires unspent.

Watch the soonest organization on its refill day. A refusal on that day before the deposit lands marks it `out`; re-checks clear that once the credit is there.

### Privacy

- Reset Priority never decodes the request's headers or model, and CPA removes keys and tokens from the candidates before it sends them. From each pick it decodes only the candidates' IDs, provider, `auth_kind`, `source` and `config_index`, and the previous attempt's auth ID only when it has the shape of a config API key: `claude:apikey:` and 12 hex digits. A subscription credential's file name or e-mail address from a higher tier is never turned into a string.
- Quota Cache's meter keeps, for each Claude API key from CPA's configuration, its auth ID, which is `claude:apikey:` and 48 bits of a SHA-256 over the key and its routing settings. That is the same kind of value as the auth index it already kept. Beside it are the auth index, the latest success and failure times, and a count of failures. At most 64 are kept, for 40 days. No subscription credential's name and no response body is kept.
- The status's `router` never carries an auth ID, an auth index, an organization UUID, a header or a key. It shows the account ID (`org-<12 hex>`), the label and Quota Cache's sentence for the account, which Quota Cache's page and Quota Glance's dashboard already show, and `config_index`, a key's position in CPA's configuration. The HTML view, which hides OAuth identities, shows organizations by label and their sentences, and never an account ID either.

## Releases

See [docs/release.md](docs/release.md) for the version, test, native build, tag, artifact, checksum, smoke, and release-note procedure.

## Security

Examples contain placeholders only. Never paste OAuth JSON, access/refresh tokens, provider bodies, cookies, or real management keys into issues, logs, release notes, test fixtures, or status screenshots.

## License

MIT. See [`LICENSE`](LICENSE).
