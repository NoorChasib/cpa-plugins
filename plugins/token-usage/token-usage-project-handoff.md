# Implementation handoff: CPA Token Usage

Prepared: 2026-09-09

**Working project name:** `token-usage`
**Plug-in ID / configuration key:** `token-usage`  
**Management Center name:** **Token Usage**

This document is self-contained. Give it to an implementation agent together with an instruction to build the project. The agent does not need the original conversation or access to the original machine. Source links below identify both the baseline plug-in and CPA itself.

## 1. Mission and status

Build a **separate native CLIProxyAPI plug-in that persists token usage by individual model**, primarily input and output tokens, for requests passing through the operator's CPA instance.

The user asked whether the existing account-health plug-in could be a starting point, requested a feasibility analysis, discussed the working name above, and requested this implementation handoff. **No token-tracking implementation or new repository has been created as part of that work.** This handoff is not evidence that the user approved every proposed feature, dependency, or default below.

The intended product promise is:

> Track CPA-reported input, output, and supporting token counters by provider and model, with persistent history and clear collection limitations.

Do **not** promise provider-global consumption, accurate invoices, a live per-token stream, complete historical backfill, or lossless accounting. CPA can omit usage, and the native event stream has no durable replay guarantee.

### Requirement versus recommendation

| Status | Item |
|---|---|
| User's core goal | Track individual models' input and output token usage. |
| User's project direction | A new plug-in/new project, potentially based on the existing account-health repository. |
| Working naming recommendation | Repository `NoorChasib/cpa-plugins`, module `plugins/token-usage`, plug-in ID `token-usage`, display name `Token Usage`. Treat these as the working names unless the user changes them. |
| Recommended MVP | Native usage callback, persistent SQLite history, provider/model totals, date filters, authenticated read-only API, collection-health reporting. |
| Proposed optional additions | Alias drill-down, account drill-down, a small authenticated browser table, CSV export. These are not already approved requirements. |
| Deliberately deferred | Pricing, budgets, quota polling, Pushover alerts, charts, dashboards, client/session analytics, cross-instance aggregation, provider billing reconciliation. |

Use sensible repository conventions for routine choices. Ask focused questions only when an unresolved choice materially affects the deliverable—for example, a requirement for exact billing-grade totals or a deployment filesystem incompatible with SQLite WAL. Continue independent work while awaiting such answers.

## 2. Repositories, documents, and version pins

### Required URLs

**Baseline plug-in repository:**

https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/account-health-pushover

**CLIProxyAPI / CPA repository:**

https://github.com/router-for-me/CLIProxyAPI

**CPA official documentation:**

https://help.router-for.me/

**CPA management API documentation:**

https://help.router-for.me/management/api

**CPA releases:**

https://github.com/router-for-me/CLIProxyAPI/releases

**Proposed new repository URL, NOT created or verified to exist:**

`https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/token-usage`

Do not assume the proposed remote exists, is owned by the current operator, or is already authorized for publication. Establish the destination when implementation is assigned.

### Revisions behind the research

| Component | Inspected revision |
|---|---|
| Baseline plug-in | `870456ecdbf3a86c76c6274f1d02e14dadddabf4` — release 0.4.0 source |
| CPA usage-contract research | `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974` — v7.2.155 |
| CPA release metadata checked on 2026-09-09 | v7.2.155, published 2026-09-08; also default-branch `main` head when checked |
| Older CPA pin in baseline's audit | `81e1b5374f99c212f196f34956eeed964a46b8fa` |

Pinned trees:

- Baseline: https://github.com/NoorChasib/cpa-plugins/tree/870456ecdbf3a86c76c6274f1d02e14dadddabf4
- CPA: https://github.com/router-for-me/CLIProxyAPI/tree/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974
- CPA release: https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.2.155

Recheck the deployed CPA version and current official source before implementing. These pins are an evidence baseline, not a claim that v7.2.155 remains latest or that another version behaves identically.

### Optional companion analysis

A longer research note was created at `docs/model-token-tracking-analysis.md` in the baseline working tree. At handoff preparation it was **untracked**, so cloning the published baseline may not include it. Ask for that file only if additional research detail is useful; this handoff contains the essential findings and direct source URLs.

Original local directory, for orientation only:

`/home/noor/Code/account-health-pushover`

Do not require that path to exist in another environment.

## 3. Read these sources first

Paths and line ranges below refer to the pinned revisions. Line numbers may move on newer branches.

### Baseline source map

Use the baseline pinned tree above and inspect:

| File | Why it matters |
|---|---|
| `README.md` | Native plug-in conventions, config, management/resource boundary, installation and packaging. |
| `abi.go` | C ABI, memory ownership, init/call/free/shutdown, host-call gate, safe unload. |
| `abi_test.go` | Generic native lifecycle, envelope, and sanitization tests; also contains Pushover-specific tests to remove. |
| `internal/protocol/types.go` | Locally mirrored RPC/lifecycle/management wire types. Re-audit against target CPA. |
| `internal/plugin/plugin.go` | Capability and route registration, lifecycle wiring, current usage callback. |
| `internal/config/config.go` | Base64 `config_yaml` decoding, YAML conventions, validation. |
| `internal/state/store.go` | Restrictive permissions and atomic-write discipline, NOT a suitable event-history schema. |
| `internal/plugin/status_html.go` | Public redaction and authenticated rendering patterns, not a token-usage UI. |
| `Makefile`, `.github/workflows/`, `scripts/` | Native builds, smoke test, packaging, checksums, release matrix. |
| `release_contract_test.go` | Artifact/registry contracts and workflow hardening; contains old identities and an older upstream validator mirror. |
| `LICENSE` | MIT copyright/permission notice to preserve in copied code. |
| `docs/upstream-audit.md` | Host callback limits, authentication boundary, state-path pitfalls. |

**Critical baseline behavior to replace:** `internal/plugin/plugin.go:36–42` decodes only `AuthIndex`, `Failed`, and `Failure.StatusCode`; `:174–192` discards successful events and events without an auth index. Token collection must not inherit either filter.

### CPA source map

All links use the researched CPA pin:

- [Native usage wire fields](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginapi/types.go#L1359-L1430)
- [Internal-to-native usage adapter](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/adapters_usage_translation.go#L132-L198)
- [Usage plug-in registration](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/adapters_usage_translation.go#L19-L93)
- [ABI/schema constants and host callback names](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginabi/types.go#L5-L105)
- [Schema negotiation](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/rpc_client.go#L58-L80)
- [Usage callback invocation/error handling](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/rpc_client.go#L561-L565)
- [Reporter attribution and timestamp](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L58-L104)
- [Requested model preservation](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/api/handlers/handlers_execution.go#L45-L76)
- [Alias/model resolution](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/auth/conductor_models.go#L326-L363)
- [OpenAI-style and provider-family usage parsers](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L724-L978)
- [Canonical accounting contract, NOT fully exposed to native plug-ins](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/usage/accounting.go#L5-L78)
- [Usage publication and failure behavior](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L344-L417)
- [Usage manager queue, dispatch, and stop](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/usage/manager.go#L250-L398)
- [Service shutdown order](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/service_lifecycle.go#L319-L348)
- [Authenticated management versus public resource routes](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/api/server_management.go#L243-L289)
- [Nonempty `Menu` converts a management GET to a public resource](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/management.go#L156-L166)

## 4. Native integration contract

Use a Go native plug-in with capabilities `usage_plugin` and `management_api`. Reuse the baseline's C ABI approach, not a sidecar or response interceptor by default.

At the research pin:

- Native ABI is **1**.
- JSON schema is **6**; the baseline advertises **4**.
- The host accepts older schema declarations, but this does not prove identical behavior across versions.
- Entrypoint: `cliproxy_plugin_init`.
- Build mode: `CGO_ENABLED=1 go build -buildmode=c-shared`.
- Usage method: `usage.handle`.
- Usage wire names are **PascalCase**.
- Usage callbacks have no request-scoped `host_callback_id`.
- No `host.usage.*` snapshot/replay callback is available in the inspected ABI.

Choose and document the minimum supported CPA version and schema after a compatibility spike. Do not merely change a schema constant to 6 without reviewing its consequences, including management response encoding.

### Decode only the necessary fields

| Native field | Treatment |
|---|---|
| `Provider` | Primary grouping dimension. |
| `ExecutorType` | Preserve as accounting provenance; avoid merging incompatible parser semantics silently. |
| `Model` | CPA-attributed execution model, not necessarily a provider's physical revision. |
| `Alias` | Preserve separately from `Model`; use for optional filtering. |
| `RequestedAt` | Event time for UTC buckets; add a separate local receipt time. |
| `Failed` | Preserve, including failed events with nonzero usage. |
| `Failure.StatusCode` | Optional structured outcome; never decode/log failure body. |
| `Generate` | Preserve; target host normalizes omitted internal values to true. Validate actual wire behavior. |
| `Detail.InputTokens` | Raw CPA-reported input count, signed 64-bit on the wire. |
| `Detail.OutputTokens` | Raw CPA-reported output count. |
| `Detail.TotalTokens` | Raw CPA-reported total; CPA may have derived it upstream. |
| `Detail.ReasoningTokens` | Auxiliary count; not necessarily additional output. |
| `Detail.CachedTokens` | Legacy convenience count; not an additional bucket. |
| `Detail.CacheReadTokens` | Explicit cache-read count. |
| `Detail.CacheCreationTokens` | Explicit cache-write/creation count. |
| `AuthIndex` | Optional account dimension only if implemented; never require it for admission. |
| `Latency`, `TTFT` | Optional; Go duration values are not already milliseconds. Defer if unused. |

Exclude `APIKey`, `Source`, raw auth IDs when unnecessary, session identifiers, response headers, and response/failure bodies. `APIKey` can contain the actual client key; `Source` can contain an email or upstream key. See [source construction](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L566-L624).

The native payload does **not** expose canonical `TokenBreakdown`, its quality/version, a stable request/event ID, `Stream`, `ResponseServiceTier`, or token-field presence at the original provider response. Do not design v1 around those fields.

Allow unknown JSON fields for forward compatibility, while retaining a narrow decoder. Missing/invalid identity or timestamps need an explicit policy: use a clearly marked unknown group/time fallback where valid, or reject with visible counters. Do not silently assign missing provider/model values to a real model. Missing token fields on the native wire can be flagged if detectable, but original provider omission versus explicit zero is generally already lost.

## 5. Accounting rules and known limitations

### Non-negotiable accounting rules

1. Preserve original counters. Call them **CPA-reported** in the API and documentation.
2. Group by at least `(provider, model)`, not model alone. Preserve executor provenance and alias without silently splitting or collapsing totals.
3. Do not infer a fixed model catalog. Model strings are runtime data with length/cardinality limits.
4. Never add every counter together. Cache/reasoning categories overlap differently.
5. Accept successful records and records without account attribution.
6. Retain reported usage on failures. A failure does not mean zero consumption.
7. Count `observed_events`, not `unique_requests` or guaranteed physical attempts.
8. Do not deduplicate by timestamp/model/account/token equality. Equal-looking records can be legitimate.
9. Zero reported tokens do not establish zero actual consumption.
10. Keep raw totals separate from any future versioned normalization. Do not claim native data has CPA's internal canonical accounting quality.

### Inspected parser semantics

| Family | Input/cache | Output/reasoning |
|---|---|---|
| OpenAI-style, including shared Codex parsing | Cache counters are subsets of input. | Reasoning is a subset of output. |
| Claude | Cache-read and cache-creation input are independent of ordinary input. | Flat output already includes reported reasoning/thinking. |
| Gemini-family | Input includes cache; reported tool-use prompt tokens also affect the parser's input count. | Flat output excludes the separate reasoning count. |

For Claude, legacy `CachedTokens` falls back to cache creation if cache reads are zero. Do not add it to explicit cache counters. These semantics come from specific CPA parsers, not a universal guarantee for every custom executor sharing a provider label.

The recommended MVP does **not** need a normalization engine. If one is added, require a verified executor-specific rule, rule version, fixtures, and an unclassified fallback. Do not silently repair contradictory counts.

### Streaming validation is a first milestone, not a late cleanup

Static inspection found:

- Claude's stream helper reads top-level `usage`, not `message_start.message.usage`. Its executor publishes the first matching usage via a once-only reporter. Input present only in the initial nested event can therefore be absent from the published record; later cumulative usage is not guaranteed to replace the first publication.
- OpenAI-compatible stream errors can publish a zero-detail failure before buffered usage is finalized, preventing that buffered usage from replacing the failure.
- Codex can emit an additional model-usage record for image-tool work outside the ordinary once-only publication.
- Retried execution can produce multiple records for one client request. Internal retries can also happen within one reporter.

Sources:

- [Claude stream helper](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L883-L893)
- [Claude stream publication](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/claude_executor_stream.go#L424-L478)
- [Split Claude usage fixture](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/claude_executor_test.go#L7435-L7450)
- [OpenAI-compatible stream publication/errors](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/openai_compat_executor.go#L419-L568)
- [Codex terminal usage](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/codex_executor_stream.go#L212-L269)
- [Additional-model event](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/codex_executor_request.go#L482-L488)
- [Retry path](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/auth/conductor_execution.go#L498-L598)

These are source findings, **not live-provider measurements**. Reproduce them with synthetic fixtures against the chosen CPA build. Report known omissions prominently; the plug-in cannot recover usage CPA never sends. If the user's required workloads need stronger accuracy, propose an upstream fix separately rather than quietly widening this project into a proxy fork.

### Delivery boundary

The inspected core usage manager uses an unbounded RAM queue and serial sink dispatch. Native callback errors are debug-logged, without durable replay or acknowledgement. Service shutdown disables/shuts down native plug-ins before stopping the usage manager, and the manager's stop does not join the dispatcher.

A plug-in can guarantee correct handling of locally committed records under its storage policy. It cannot guarantee receipt of every host event. Do not label a healthy local writer as proof that upstream collection is complete.

### Do not substitute the existing usage queue casually

CPA explicitly [removed built-in usage statistics since v6.10.0](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/README.md#L137-L147).

`GET /v0/management/usage-queue` destructively pops a short-lived in-memory queue; it is not a history snapshot. Its payload is richer, but it competes with other consumers, expires, and is not durable. Old `/usage`, `/usage/export`, and `/usage/import` endpoints are unavailable in the researched version. `/api-key-usage` provides request counters, not the required per-model token history.

`usage-statistics-enabled` gates the built-in queue sink, not native usage-plug-in delivery at the researched version. Verify this in the spike; do not require the switch based on its name alone.

Sources: [queue handler](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/api/handlers/management/usage.go#L23-L55), [queue sink](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/redisqueue/plugin.go#L21-L193), [queue implementation](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/redisqueue/queue.go#L9-L241).

## 6. Bootstrap the separate project safely

1. Inspect the destination and its instructions/status before writing. Do not overwrite an existing project or discard unrelated changes.
2. Use the baseline as a source reference or selective seed. Do not transform the existing health plug-in in place.
3. Preserve applicable MIT copyright and permission notices from copied source. The baseline `LICENSE` has `Copyright (c) 2026 NoorChasib`; do not strip it or replace it with a different author's notice.
4. Do not copy `.git`, credentials, `.env`, local settings, state databases, `dist` artifacts, or deployment secrets into a fresh project. If choosing a history-preserving clone, explicitly verify its remote before any push; never accidentally push token-tracking changes to the baseline remote.
5. Change the module path, plug-in ID, metadata, config key, route namespace, library names, archive names, version injection, README, registry metadata, workflow references, and smoke-test identities coherently.
6. Use a new project version such as `0.1.0`, not the baseline's `0.4.0`. Version choice is a proposed default.
7. Remove health classification, Pushover, weekly quota polling, incidents, reminders, and their tests/config/env variables. Preserve generic lifecycle and security tests after checking their assumptions.
8. Do not create/publish a remote, push, tag, release, deploy, or change the production CPA instance unless authorized by the implementation request. Prepare artifacts and instructions locally where appropriate.

Search for stale identities including `account-health-pushover`, `Account Health Pushover`, `CPA_PUSHOVER`, and the original module path. Attribution and historical provenance references may intentionally remain; active build/runtime identities must not.

Specific rename hotspots: `go.mod`, `Makefile`, `internal/plugin/plugin.go`, `registry.json`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `scripts/verify-release.sh` (including its literal archive regex), `scripts/smoke-test.sh`, and `release_contract_test.go`. `scripts/package-release.py` is already parameterized.

## 7. Recommended architecture

Suggested package boundaries, not mandatory file names:

| Package / boundary | Responsibility |
|---|---|
| Root native ABI + `internal/protocol` | C memory ownership, RPC envelopes, host compatibility. |
| `internal/plugin` | Lifecycle/config wiring, callback and management dispatch. |
| `internal/usage` | Narrow wire decoder, validation, compact event type, raw counter rules. |
| `internal/collector` | Admission, bounded queue, writer ownership, flush/drain, local health. |
| `internal/store` | SQLite migrations, event transactions, rollups, queries, retention. |
| `internal/config` | Typed configuration/defaults/validation. |
| Optional rendering module | Read-only authenticated table; keep separate from accounting. |

Avoid an elaborate framework, message broker, ORM, microservice, or provider-client layer. Native collection should need no provider network access, auth-file reading, or account-health polling.

### Hot-path callback

- Decode a whitelisted record; never persist the complete raw payload.
- Validate counters and bounded metadata, copy owned data, attach receipt time and a locally unique event ID.
- Attempt a bounded enqueue and return promptly.
- No database query, provider call, auth lookup, synchronous host callback, or per-event logging in the normal path.
- Do not reuse the health monitor's size-one wake/coalescing channel as event storage. Coalescing notifications is safe only when events are already retained elsewhere.

### Writer and lifecycle

- One writer owns storage mutations. Batch events in transactions.
- Insert an event and update any rollups atomically. Update rollups only when that local event ID was newly inserted.
- Preserve the same local ID through write retries. A new ID on each retry defeats local idempotency.
- Stop new admission before draining/closing; coordinate reconfiguration and route readers so no worker uses a closed database.
- Never unload native code while plug-in-owned work or admitted host calls can still execute.
- A database path change on reconfigure must not silently split history or start two writers against incompatible state. Prefer rejecting it until restart unless migration is deliberately implemented.
- Bound retries and memory. On full queue, count dropped records and mark local collection degraded instead of blocking the host indefinitely.

The default recommended policy is asynchronous admission plus batched persistence. This has an explicit admission-to-commit loss window. Synchronous durable admission is an alternative only if its callback latency is acceptable; it still cannot repair the host's pre-callback loss window.

## 8. Storage and time semantics

### SQLite recommendation

Choose a maintained SQLite driver after testing native `c-shared` builds. The baseline has no database dependency. A pure-Go driver avoids another C library but does **not** eliminate CGO for this plug-in. A CGO SQLite driver adds compiler/linker considerations across every claimed platform. Do not choose solely from a linux-amd64 unit test.

Use a local persistent filesystem, explicit data path, restrictive permissions, migrations, and a single active writer per database. Do not share a database across CPA replicas. Enforce/detect concurrent ownership or document and verify a deployment lock rather than relying on convention alone.

WAL is appropriate for concurrent read queries on supported local filesystems. Select a synchronization policy explicitly; `synchronous=FULL` is a reasonable proposed default for committed-record durability, but benchmark it. Keep WAL/checkpoint files within the same persistent mount. Do not use WAL on a network filesystem. See https://sqlite.org/wal.html.

Do not derive the storage directory by examining account credentials. Do not put JSON state under CPA's auth directory, where it can be discovered as an auth file. Keep plugin data separate from replaceable versioned binaries.

Backups must use a supported SQLite backup procedure or a properly quiesced/checkpointed copy. Copying only the main file of a live WAL database can omit committed state. Never silently recreate a corrupt or newer-schema database.

### Suggested logical schema

Names are recommendations; encode the invariants, not necessarily this exact layout.

| Entity | Minimum purpose |
|---|---|
| `schema_migrations` / metadata | Schema version and migration bookkeeping. |
| `collection_runs` | Start/stop/last persistence, version/provenance, local health, clean-shutdown marker. An unclean restart indicates possible loss, not an exact missing-event count. |
| `usage_events` | Local event ID; requested/received time; provider, executor, model, alias; outcome/generation; seven raw token counters; bounded anomaly flags; optional account index. |
| Optional `usage_daily` | UTC-day rollups for history beyond raw retention, keyed by the dimensions the API actually supports. |
| Local collection diagnostics | Admission/rejection/drop/storage-error counters, scope and time interval. Diagnostics surviving a crash are themselves best-effort unless already committed. |

Recommended properties:

- Use signed 64-bit storage for native counters and checked integer arithmetic for sums. Reject/flag negatives or out-of-range values; never let SQLite integer overflow silently promote accounting into floating point.
- Keep invalid counter records out of normal totals; count and explain rejected/quarantined observations without storing sensitive raw JSON.
- An event retry with an already committed local ID must not increment totals again. Two distinct native callbacks with identical content must remain distinct.
- Preserve optional grouping dimensions in rollups if the API promises historical filters for them. Do not claim account/alias drill-down after dropping those dimensions.
- Keep timestamps in UTC. Use explicit half-open intervals `[from, to)`; document whether time attribution uses reported or fallback receipt time.
- Index around actual date/provider/model query patterns. Parameterize every query.
- Model names and aliases are untrusted data. Impose length, result-row, and cardinality limits with visible rejection/degradation behavior.

### Retention and query precision

Suggested initial defaults are **30 days raw events** and **365 days daily rollups** if rollups are implemented. These are design defaults, not user-specified limits. Validate them against expected volume; expose retention boundaries in responses.

Exact custom time ranges can use raw events while retained. Daily rollups cannot answer arbitrary partial-day ranges exactly after raw records expire. Choose and test an explicit policy: reject unsupported older partial-day queries, or return a clearly described resolution/coverage limitation. Never silently return approximate values as exact totals. A raw-plus-rollup query must partition time without overlap or gaps.

MVP may instead keep raw events only and bound all queries to raw retention, provided that limitation is clear. Do not promise all-time totals unless separately maintained counters survive raw and rollup cleanup by design. Retention must never masquerade as a new zero-usage period.

Set a disk budget including database, WAL, and bounded diagnostic state. Cleanup is bounded background work, not callback work. If the budget cannot be maintained, stop accepting/persisting under a documented degraded policy rather than silently deleting newer promised history. Document that deleting rows does not necessarily shrink SQLite's file immediately.

## 9. Proposed configuration

The following is a **design example for the new plug-in**, not configuration supported by the baseline. Implement and document only keys that actually exist in the final code.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    token-usage:
      enabled: true
      priority: 20
      database-path: "/CLIProxyAPI/plugin-data/token-usage/usage.sqlite"
      queue-capacity: 8192
      batch-size: 256
      flush-interval: 1s
      raw-retention: 720h
      aggregate-retention: 8760h
      max-disk-bytes: 1073741824
      account-breakdown: false
      display-timezone: UTC
```

`priority` is CPA plug-in load/order priority, not credential routing priority. `display-timezone` applies only if a browser view is implemented; API timestamps remain explicit UTC. Omit aggregate retention if rollups are not part of the chosen MVP.

Validate negative/zero/overflowing durations and capacities, unreasonable resource limits, unsupported timezones, unwritable paths, and incompatible retention settings. Prefer explicit configuration errors over silent fallback to in-memory-only storage. Do not require a Pushover token or provider credentials.

Document a dedicated persistent volume mounted at `/CLIProxyAPI/plugin-data` in Docker Compose/Coolify. Show merging this config into the existing CPA config rather than replacing unrelated settings. Do not publish a store-source URL until the new registry actually exists.

## 10. Proposed read-only API

Use authenticated management routes under:

`/v0/management/plugins/token-usage`

Suggested MVP routes:

| Method/path | Purpose |
|---|---|
| `GET /status` | Plug-in/storage status, retention/coverage, local queue/drop/error information, limitations. |
| `GET /summary?from=...&to=...` | Selected-range raw counter totals and observed-event count, with optional provider/model filters. |
| `GET /models?from=...&to=...` | Bounded/paginated provider/model breakdown, deterministic ordering. |
| Optional `GET /series?from=...&to=...&bucket=day` | Daily buckets from the supported retention/resolution. |
| Optional `GET /status/html` | Authenticated read-only table if a UI is included. |

The paths are relative to the base above. Reconcile exact registration shapes with the CPA target; this is not an assertion these routes already exist.

### Response contract

Every statistics response should include:

- API schema version and `source: "cpa_reported"`.
- Effective interval and UTC/resolution semantics.
- Raw counter names clearly marked as reported; auxiliary counters are not advertised as additive.
- `observed_events`, with success/failure breakdown if implemented.
- Coverage/retention metadata and local degradation indicators.
- A statement/flag that upstream delivery completeness is unknown, even when local persistence is healthy.

Choose a stable large-integer representation. **Recommended:** encode token totals and event counters as decimal strings in JSON so JavaScript does not round values above `2^53 - 1`. Test this at both API and rendering boundaries; do not parse them through floating-point `Number` merely to format them.

Missing model/filter results within a covered range can return an empty collection. Data outside retention or a storage outage must not become a misleading zero result. Distinguish invalid query parameters, unsupported resolution/range, and storage unavailability with documented HTTP errors. Bound date ranges, page sizes, result rows, and execution time. Statistics are eventually consistent with the writer; expose last persistence time rather than claiming every callback has committed.

### Authentication and privacy boundary

- CPA authenticates management routes. Resource routes are unauthenticated at the inspected revision.
- **Leave `Menu` empty on private management GET routes.** A nonempty value can convert the route to a public resource.
- Token totals, model activity, optional account identifiers, errors, and database paths are not public-page data.
- If a sidebar resource is added, it must be a static/redacted shell. Fetch private data only through the authenticated management boundary.
- Do not embed management keys in HTML, URLs, logs, bundles, or persisted plug-in configuration. Do not introduce a new login/credential collection flow for this project.
- The baseline's client-side management-session recovery is implementation-specific; re-audit it if reused. Do not assume localStorage format or schema escaping behavior is stable.
- Escape model/alias text in HTML, avoid unsafe DOM insertion, and retain a restrictive CSP. Treat query/filter strings as untrusted.
- No reset/delete/import/write endpoints in the recommended MVP; adding them requires deliberate authorization and CSRF design.

A browser view is a useful follow-up, not a prerequisite for proving the collector. If included, keep it to a read-only model table and date filters unless the user requests more. Do not let UI work postpone streaming/accounting validation.

## 11. Phased implementation sequence

### Phase A — Establish the target and validate the telemetry

1. Inspect destination instructions and existing files; determine whether an existing new repo is being supplied.
2. Identify the supported/deployed CPA release. Re-read the pinned contract and compare relevant changes.
3. Write a short upstream compatibility note with exact CPA commit, ABI/schema choice, payload fields, route security, and known omissions.
4. Build a minimal native test plug-in or harness using sanitized synthetic records.
5. Exercise normal and streaming provider fixtures through the real CPA execution-to-native boundary, not only a direct Go call to the decoder.
6. Test collection with `usage-statistics-enabled` true and false.
7. Record expected-versus-observed input/output/cache values, including the known Claude split-usage case.

**Exit criterion:** prove the data reaches the native plug-in, establish supported executor coverage, and document accuracy limits before implementing the full persistence layer. If required accuracy is unattainable, surface it immediately with a fixture reproduction; continue work that does not depend on the unresolved requirement.

### Phase B — Bootstrap identity and contracts

- Create the separate source tree with minimal reused native infrastructure and preserved license notices.
- Replace identity/release/config metadata coherently.
- Remove unrelated domain packages and dependencies.
- Introduce the narrow event decoder, config validation, and tests.
- Prove a native shared library builds and loads with the selected schema.

### Phase C — Persistence and lifecycle

- Choose/test SQLite driver and filesystem requirements.
- Implement migrations, local IDs, checked counters, idempotent event-plus-rollup transactions if applicable.
- Implement bounded admission, batching, storage-error handling, retention, and diagnostics.
- Test restart, concurrent access, path ownership, reconfigure, quiesce, and shutdown.

### Phase D — Query interface

- Implement authenticated status and provider/model totals.
- Implement exact date/filter semantics and bounded results.
- Test integer serialization, retention boundaries, and raw/rollup consistency if used.
- Add a browser table only if included in the chosen deliverable; keep it authenticated and read-only.

### Phase E — Native integration, packaging, and operations

- Replace the baseline's health/Pushover smoke assertions with synthetic usage, persistence, and auth assertions.
- Run unit/race/static checks and real native loading against a pinned CPA build.
- Validate every platform claimed by the release matrix.
- Document installation, persistent volume, config, APIs, compatibility, limitations, backup/restore, update/rollback, and uninstall behavior.
- Prepare registry, archives, and checksums without claiming they have been published.

## 12. Build and release constraints from the baseline

The inspected baseline uses Go **1.26.0** in `go.mod`, Go **1.26.x** in workflows, and CGO for the native ABI. Its only Go dependency is `gopkg.in/yaml.v3`. Recheck toolchain/driver compatibility in the new project.

Existing targets worth adapting:

```bash
make fmt-check
make vet
make test
make test-race
make build
make c-shared
make ci
make package-current VERSION=0.1.0
make checksums
make verify-release
```

`make ci` currently runs formatting, vet, race tests, and ordinary build; **it does not replace `make c-shared` or the Docker smoke test**. `make verify-release` checks a partial local bundle. `make verify-release-full` needs all claimed platform archives.

The baseline also has:

```bash
CPA_SMOKE_REQUIRE_DOCKER=1 make smoke
```

Run this only after adapting the smoke script to the new plug-in. The baseline's script is health-specific and uses an unpinned `eceasy/cli-proxy-api:latest` image. For reproducible token tests, select a verified CPA image digest or a locally built pinned CPA commit, document how that source maps to the image, and never silently skip a required native smoke test. The baseline skip path is intentional for machines without Docker; CI requires Docker explicitly.

Baseline release platforms:

| OS/architecture | Native library | Baseline compiler |
|---|---|---|
| Linux amd64 | `token-usage.so` | `gcc` |
| Linux arm64 | `token-usage.so` | `aarch64-linux-gnu-gcc` |
| macOS amd64 | `token-usage.dylib` | `clang` |
| macOS arm64 | `token-usage.dylib` | `clang` |
| Windows amd64 | `token-usage.dll` | MSYS2 UCRT64 GCC |

Supporting all five is a **baseline convention, not a user-confirmed launch requirement**. If the new project claims that matrix, validate the SQLite driver and `c-shared` artifact on every target. Otherwise explicitly narrow the matrix and update registry/verifier/test expectations together. Do not ship a nominal five-platform registry backed by one tested binary.

Proposed artifact convention: `token-usage_0.1.0_linux_amd64.zip`, containing exactly the appropriate root library, plus the release checksum manifest. Verify naming against the target CPA store validator.

Port generic ABI drain tests, packaging tests, checksums, pinned-action/workflow hardening, and release identity contracts. Re-derive schema/version assertions and review the older validator mirror. Pushover endpoint override tests, health classification tests, and notification/quota smoke steps do not belong in this project. Some baseline shell verifier tests skip on Windows; do not describe that as equivalent native Windows test coverage.

Do not execute a publish workflow merely to test a build. Inspect event/job conditions before any dispatch and obtain appropriate authorization for outward-facing runs. A first-release rehearsal should be non-publishing.

## 13. Required acceptance tests

Use synthetic tokens and local mock upstreams; do not require real provider accounts or billable requests for automated verification. Any live-provider check is an explicitly authorized optional supplement.

| Area | Test and expected result |
|---|---|
| Basic ingestion | Two events for one provider/model sum exactly; a different model remains separate. |
| Provider dimension | Equal model names from two providers remain separate. |
| Alias dimension | One alias routed to two execution models preserves both attributions; historical events are not relabeled from current config. |
| Missing account | Successful events with empty `AuthIndex` still count. |
| Outcome | Nonzero failed-event tokens are retained; observed-event count is not labeled unique requests. |
| Wire tolerance | Unknown fields are ignored; malformed JSON is rejected and counted without echoing payloads. |
| Sensitive payload | Plant secrets in `APIKey`, `Source`, failure body, headers, and omitted metadata; they never appear in DB, logs, status, HTML, or query responses. |
| Counter ranges | Negative, fractional, excessive, and overflow-producing counters cannot corrupt totals or turn them into floats. |
| Large integers | Counts above `2^53 - 1` remain exact through storage, JSON, and optional rendering. |
| Cache/reasoning | Synthetic parser-family fixtures preserve raw values without adding overlapping buckets. Unknown semantics remain raw/unclassified. |
| Zero/missing | Zero reported usage is not presented as proven zero consumption; original presence is not invented. |
| Native stream path | Final usage, missing usage, cumulative updates, split Claude input/output, terminal errors, and disconnects have documented expected outcomes against the CPA pin. |
| Retries/additional models | Equal-looking distinct records remain distinct; extra model usage is not deleted by heuristic deduplication. |
| Local write retry | Retrying the same locally identified event never increments committed totals twice. |
| Transactionality | Event insert and rollup update commit together or neither does. |
| Restart | Previously committed history survives restart; a fresh event increments once; unclean shutdown marks possible local loss without inventing its size. |
| Storage failure | Disk full, read-only path, DB busy, corruption, and newer schema are visible failures, never silent memory-only success or empty totals. |
| Concurrency | Callback/query/flush/retention/reconfigure races pass race tests and do not deadlock or use closed state. |
| Queue pressure | A small queue and slow writer trigger the documented overflow policy; local drops become visible and memory remains bounded. |
| Lifecycle | Admission closes safely; accepted work is drained according to policy; no live plug-in work remains when native code unloads. |
| Time and retention | UTC `[from,to)` boundaries, midnight, fallback time flags, DST display, expired raw data, and rollup-only periods follow the documented contract. |
| Rollup boundaries | If implemented, raw/rollup queries cannot overlap/double-count; unsupported partial-day historical ranges are not called exact. |
| Resource controls | Oversized metadata, excessive model cardinality, broad queries, and result limits are bounded and observable. |
| Auth boundary | Missing/invalid management authentication reveals no usage; public resources reveal no totals/model/account data; private GET routes have empty `Menu`. |
| HTML/SQL | Malicious model names render as text; filters remain parameterized and cannot inject SQL or HTML. |
| Host settings | Native collection works with queue statistics enabled/disabled if supported by the chosen version. |
| Deployment | Replacing the library does not delete the persistent DB; token and health plug-ins can be enabled together with distinct identities/state. |
| Artifacts | Every claimed platform artifact builds, follows the registry contract, and is covered by checksums; unsupported/skipped checks are reported honestly. |

Known upstream omissions are not an excuse for failed local accounting tests. Conversely, do not falsify a native fixture's expected result to make a known upstream limitation look fixed.

## 14. Completion checklist and handback

A complete recommended MVP should provide:

- [ ] A separate, correctly named project with license/provenance preserved and no secrets or old runtime identities.
- [ ] A minimal native usage collector with no health/Pushover/provider-polling dependency.
- [ ] Persisted provider/model input/output totals and the available auxiliary raw counters.
- [ ] Explicitly documented accounting semantics, collection scope, retention, and crash-loss window.
- [ ] Authenticated bounded read-only queries and collection-health reporting.
- [ ] Tested migrations, local write idempotency, restart behavior, resource limits, and safe unload.
- [ ] Native fixture results for the supported CPA/executor paths, especially streamed input/cache accounting.
- [ ] A compatibility audit pinned to the tested CPA source and actual native integration target.
- [ ] Build/package verification for the platforms actually claimed.
- [ ] Install/config/API/backup/restore/update/rollback/uninstall documentation.
- [ ] Optional UI/account/alias/export features clearly identified as included or deferred, not silently assumed complete.

The implementation agent's final response should state:

1. Where the new project lives and what was implemented.
2. Exact CPA version/commit and supported platforms tested.
3. Commands run and results, including failures/skips and unperformed live tests.
4. How to build/install/configure and query it.
5. Remaining upstream accuracy limitations and local durability/retention tradeoffs.
6. What was not created, published, deployed, or otherwise left pending.

Do not claim success solely because Go unit tests pass. Verify a real native load and fixture-to-persistence-to-authenticated-query path.

## 15. Copy/paste kickoff message for the next agent

> Build the separate `token-usage` project described in this handoff. The core goal is persistent per-model input/output token tracking for traffic through CLIProxyAPI. Use `https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/account-health-pushover` as a selective native plug-in baseline and `https://github.com/router-for-me/CLIProxyAPI` as the authoritative host source. Keep the existing health plug-in unchanged. Read the requirements-versus-recommendations section, inspect the destination and target CPA version, and start by validating the actual native usage payload and streaming limitations with fixtures. Then implement and verify the scoped collector, persistence, authenticated API, lifecycle, and packaging. Use `token-usage` as the working plug-in ID. Preserve raw CPA counters; do not promise exact billing/global usage, double-count cache/reasoning, store keys/bodies, or expose private statistics through public resources. Treat the suggested SQLite/config/API/package designs as defaults you may refine with a documented reason. Ask only about material unresolved choices and continue independent work. Do not publish, push, release, deploy, alter production CPA, or make billable provider calls without authorization. Hand back working source, docs, exact test results, and known limitations.

---

**Handoff verification status:** Based on the prior pinned CPA source research and a fresh read-only check of the baseline's licensing, build, release, and native-test conventions. No new plug-in code, deployment, live-provider test, or token-accounting runtime validation was performed while preparing this document. The phased validation work above is for the implementation agent, not work already completed.
